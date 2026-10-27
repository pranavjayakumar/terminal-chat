package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/transport"
	"github.com/skailama/tc/internal/trust"
)

const clientName = "tc"

// pendingReq is an in-flight REQ. hook runs on the read goroutine before
// any later frame is processed, so cache writes it makes are ordered with
// the event stream; it runs even if the waiter gave up.
type pendingReq struct {
	ch   chan proto.Res
	hook func(proto.Res) error
}

// session is one authenticated connection.
type session struct {
	conn     transport.Conn
	codec    *proto.Codec
	welcome  proto.Welcome
	done     chan struct{}
	failOnce sync.Once
	lastSend atomic.Int64
	// rosterMark is the PING sent right after sync: its PONG comes after
	// every roster the server sends for the sync (it answers in order), so
	// rosters before it are a baseline, never huddle starts.
	rosterMark atomic.Int64
	watchdog   *time.Timer // fails the session when nothing is received for recvTimeout
	timedOut   atomic.Bool
	// voiceIn carries VOICE frames (voice on the TCP fallback, §14.4) from
	// the read loop to the huddle; full = drop.
	voiceIn chan []byte

	mu      sync.Mutex
	nextID  uint32
	pending map[uint32]pendingReq
	closed  bool

	// read goroutine only
	syncing     bool
	sawSnapshot bool

	// E2E: our login key, our device keys once loaded, and devReady,
	// closed once the server has our device (e2e.go). devTried closes
	// after the first registration attempt whatever its outcome; devErr
	// is why the last attempt failed (nil once registered); devRetry asks
	// the e2e worker to try again (a revoke may have freed a slot).
	sshPub      []byte
	dev         atomic.Pointer[e2ecrypto.DeviceKeys]
	devReady    chan struct{}
	devOnce     sync.Once
	devTried    chan struct{}
	devTriedOne sync.Once
	devErr      atomic.Pointer[error]
	devRetry    chan chan error
	revokedSeen int           // e2e worker only: our revoked devices at the last retry (retryDue)
	devGap      time.Duration // e2e worker only: retryDue's current gap
	devNext     time.Time     // e2e worker only: no transient retry before

	// restarting: we ended the session on purpose (new device keys), so
	// the reconnect isn't a failure that grows the backoff.
	restarting atomic.Bool
}

func newSession(conn transport.Conn, codec *proto.Codec, w proto.Welcome, recvTimeout time.Duration) *session {
	s := &session{conn: conn, codec: codec, welcome: w, done: make(chan struct{}),
		pending: map[uint32]pendingReq{}, syncing: true, voiceIn: make(chan []byte, voiceInLen), devReady: make(chan struct{}),
		devTried: make(chan struct{}), devRetry: make(chan chan error)}
	s.lastSend.Store(time.Now().UnixNano())
	// A timer rather than a polling goroutine: a blocked write must not be
	// able to delay the receive timeout.
	s.watchdog = time.AfterFunc(recvTimeout, func() {
		s.timedOut.Store(true)
		s.fail()
	})
	return s
}

func (s *session) send(t proto.FrameType, v any) error {
	err := s.codec.Write(t, v)
	s.lastSend.Store(time.Now().UnixNano())
	return err
}

// restart ends the session on purpose: Run reconnects soon, without
// counting it as a failure.
func (s *session) restart() {
	s.restarting.Store(true)
	s.fail()
}

// errRestart is connect's result for a session ended by restart.
var errRestart = errors.New("reconnecting with new device keys")

// fail closes the connection and fails every pending request with ErrOffline.
func (s *session) fail() {
	s.failOnce.Do(func() {
		s.watchdog.Stop()
		s.conn.Close()
		s.mu.Lock()
		s.closed = true
		for _, p := range s.pending {
			if p.ch != nil {
				close(p.ch)
			}
		}
		s.pending = nil
		s.mu.Unlock()
		close(s.done)
	})
}

func (s *session) start(op string, args any, ch chan proto.Res, hook func(proto.Res) error) (uint32, error) {
	raw, err := proto.Marshal(args)
	if err != nil {
		return 0, fmt.Errorf("encoding %s: %w", op, err)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrOffline
	}
	s.nextID++
	id := s.nextID
	s.pending[id] = pendingReq{ch, hook}
	s.mu.Unlock()
	if err := s.send(proto.TReq, proto.Req{ID: id, Op: op, Args: raw}); err != nil {
		s.fail()
		return 0, ErrOffline
	}
	return id, nil
}

// call sends a REQ and waits for its RES. A refusal is returned as *proto.Error.
func (s *session) call(ctx context.Context, op string, args any, hook func(proto.Res) error) (proto.Raw, error) {
	ch := make(chan proto.Res, 1)
	id, err := s.start(op, args, ch, hook)
	if err != nil {
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, ErrOffline
		}
		if !r.OK {
			if r.Err == nil {
				return nil, proto.Errf(proto.ErrInternal, "")
			}
			return nil, r.Err
		}
		return r.Data, nil
	case <-ctx.Done():
		s.mu.Lock()
		if p, ok := s.pending[id]; ok {
			p.ch = nil
			s.pending[id] = p
		}
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

// fire sends a REQ whose RES is ignored.
func (s *session) fire(op string, args any) error {
	_, err := s.start(op, args, nil, nil)
	return err
}

func (s *session) resolve(r proto.Res) error {
	s.mu.Lock()
	p, ok := s.pending[r.ID]
	delete(s.pending, r.ID)
	s.mu.Unlock()
	if !ok {
		return nil
	}
	if p.hook != nil {
		if err := p.hook(r); err != nil {
			return err
		}
	}
	if p.ch != nil {
		p.ch <- r
	}
	return nil
}

// ---- handshake / login ----

func handshake(conn transport.Conn, codec *proto.Codec, signer ssh.Signer, invite, name, agent string, passive bool, timeout time.Duration) (proto.Welcome, error) {
	// Hardware keys may wait for a touch, hence the long timeout.
	t := time.AfterFunc(timeout, func() { conn.Close() })
	defer t.Stop()
	exp, err := conn.Exporter()
	if err != nil {
		return proto.Welcome{}, fmt.Errorf("tls exporter: %w", err)
	}
	sig, err := identity.Sign(signer, exp)
	if err != nil {
		return proto.Welcome{}, err
	}
	hello := proto.Hello{Client: clientName, Caps: []string{proto.CapE2E, proto.CapKeyHint, proto.CapVoice, proto.CapVoiceTCP, proto.CapAgent},
		Auth: proto.Auth{Pubkey: signer.PublicKey().Marshal(), Sig: sig, Invite: invite, Name: name}, Agent: agent}
	if passive {
		hello.Caps = append(hello.Caps, proto.CapPassive)
	}
	if err := codec.Write(proto.THello, hello); err != nil {
		return proto.Welcome{}, fmt.Errorf("sending hello: %w", err)
	}
	for {
		f, err := codec.Read()
		if err != nil {
			return proto.Welcome{}, fmt.Errorf("waiting for welcome: %w", err)
		}
		switch f.Type {
		case proto.TWelcome:
			var w proto.Welcome
			if err := f.Decode(&w); err != nil {
				return w, err
			}
			if w.User.ID == 0 {
				return w, errors.New("welcome without user")
			}
			if agent != "" && w.Agent == nil {
				// A server without agent1 would log us in as the owner.
				return w, errors.New("the server didn't accept this agent session")
			}
			if agent == "" && w.Agent != nil {
				return w, errors.New("this key is an agent's: log in with the agent's home")
			}
			codec.SetMaxFrame(proto.MaxFrame)
			return w, nil
		case proto.TBye:
			var b proto.Bye
			_ = f.Decode(&b)
			return proto.Welcome{}, &ByeError{Code: b.Code, Msg: b.Msg, Keys: b.Keys}
		}
	}
}

const stableSession = 10 * time.Second

// v020Retry reports whether an invite_invalid without a key hint is worth
// a second signature as a plain login, for a v0.2.0 server that checks the
// invite before the key. It can't tell server versions apart, so it costs
// a dead invite one extra signature against current servers; not with a
// security key (a touch each), and not for a key-bound invite refusing
// this key (the join loop moves on to the next key instead). Matching the
// msg text is only for frozen v0.2.0 servers; nothing new keys on msg.
func v020Retry(bye *ByeError, signer ssh.Signer) bool {
	return !strings.HasPrefix(signer.PublicKey().Type(), "sk-") && !strings.Contains(bye.Msg, "other SSH keys")
}

// Login redeems an invite (new user or new key for an existing user), or
// with a tokenless tc://host:port#pin logs in a key already registered,
// then writes home/config.json and starts a fresh cache. keyPath is an
// identity.Load spec: "" means ssh-agent or the default ~/.ssh keys. A key
// that is already registered logs in whatever the invite (§4), also
// against servers that answer key_exists.
func Login(ctx context.Context, home, invite, name, keyPath string) (proto.User, error) {
	home = homeOr(home)
	inv, err := proto.ParseLogin(invite)
	if err != nil {
		return proto.User{}, err
	}
	signer, err := identity.Load(keyPath)
	if err != nil {
		return proto.User{}, err
	}
	w, err := loginOnce(ctx, inv, signer, inv.Token, name, "")
	var bye *ByeError
	if errors.As(err, &bye) && inv.Token != "" {
		switch {
		case bye.Code == proto.ByeKeyExists:
			w, err = loginOnce(ctx, inv, signer, "", "", "")
		case bye.Code == proto.ByeInviteInvalid && len(bye.Keys) == 0 && v020Retry(bye, signer):
			// A v0.2.0 server checks the invite before the key, so a join
			// retried after a lost WELCOME gets invite_invalid there.
			if w2, err2 := loginOnce(ctx, inv, signer, "", "", ""); err2 == nil {
				w, err = w2, nil
			}
		}
	}
	if err != nil {
		return proto.User{}, err
	}

	keyFP := ""
	switch {
	case keyPath == "":
		keyFP = identity.Fingerprint(signer.PublicKey())
	case strings.HasPrefix(keyPath, "SHA256:"):
		keyFP, keyPath = keyPath, ""
	default:
		if abs, err := filepath.Abs(keyPath); err == nil {
			keyPath = abs
		}
	}
	cfg := Config{Addr: inv.Addr, Pin: inv.Pin, KeyPath: keyPath, KeyFP: keyFP, UserID: w.User.ID, Name: w.User.Name}
	if err := saveLogin(home, cfg, w); err != nil {
		return proto.User{}, err
	}
	return w.User, nil
}

// saveLogin starts a fresh cache in home, trusts w's server identity (an
// explicit login is the one place a new one is trusted) and writes cfg.
func saveLogin(home string, cfg Config, w proto.Welcome) error {
	if err := ensurePrivateDir(home); err != nil {
		return fmt.Errorf("creating %s: %w", home, err)
	}
	db, err := openCache(filepath.Join(home, "cache.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := wipe(tx); err != nil {
		return err
	}
	if err := upsertUser(tx, w.User); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(w.ServerID) == e2ecrypto.IDSize {
		ts, err := trust.Open(home)
		if err != nil {
			return err
		}
		err = ts.RepinServer(cfg.Addr, cfg.Pin, e2ecrypto.ServerID(w.ServerID))
		ts.Close()
		if err != nil {
			return err
		}
	}
	return writeConfig(home, cfg)
}

// loginOnce runs one handshake on a new connection.
func loginOnce(ctx context.Context, inv proto.Invite, signer ssh.Signer, token, name, agent string) (proto.Welcome, error) {
	conn, err := transport.Dial(ctx, inv.Addr, inv.Pin)
	if err != nil {
		return proto.Welcome{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	w, err := handshake(conn, proto.NewCodec(conn), signer, token, name, agent, false, defaultTiming.handshake)
	if err != nil && ctx.Err() != nil {
		return proto.Welcome{}, ctx.Err()
	}
	return w, err
}

func writeConfig(home string, cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(home, "config-*.json")
	if err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return os.Rename(tmp.Name(), filepath.Join(home, "config.json"))
}

// ---- connection manager ----

// fatal reports whether reconnecting is pointless.
func fatal(err error) bool {
	var bye *ByeError
	if errors.As(err, &bye) {
		return bye.Code == proto.ByeRevoked || bye.Code == proto.ByeAuthFailed
	}
	return errors.Is(err, transport.ErrPinMismatch) || errors.Is(err, errWrongUser) ||
		errors.Is(err, trust.ErrServerChanged)
}

// checkServer compares the WELCOME's server_id with the one pinned for the
// config's address and TLS pin (or, before pins existed, the cache's). A
// different id stops Run: every trust row is keyed by server_id, so
// accepting a new one would silently start trust over. Nothing is reset;
// `tc login` re-trusts. It returns the id to pin after the first sync when
// none is pinned yet.
func (c *Client) checkServer(w proto.Welcome) (*e2ecrypto.ServerID, error) {
	pinned, ok, err := c.trust.ServerIdentity(c.cfg.Addr, c.cfg.Pin)
	if err != nil {
		return nil, err
	}
	if !ok {
		if old, err := metaBytes(c.db, "server_id"); err == nil && len(old) == e2ecrypto.IDSize {
			copy(pinned[:], old)
			ok = true
		}
	}
	if len(w.ServerID) != e2ecrypto.IDSize {
		if ok {
			return nil, trust.ErrServerChanged
		}
		return nil, nil
	}
	sid := e2ecrypto.ServerID(w.ServerID)
	if ok && sid != pinned {
		return nil, trust.ErrServerChanged
	}
	return &sid, nil
}

// checkDowngrade marks a server that speaks e2e1, and raises a sticky
// alarm if a server once seen with e2e1 now doesn't advertise it: an
// attacker stripping the cap would otherwise get plaintext.
// TODO(step 7): refuse plaintext sends into e2e channels while alarmed.
func (c *Client) checkDowngrade(s *session) {
	sid, ok := s.serverID()
	if !ok {
		return
	}
	if s.e2e() {
		_ = c.trust.MarkServerE2E(sid)
		return
	}
	if was, err := c.trust.ServerUsesE2E(sid); err == nil && was {
		_ = c.trust.RaiseServerAlarm(sid, "the server stopped offering end-to-end encryption")
		c.post(Update{Channels: true, Status: true})
	}
}

// errWrongUser means our key now belongs to a different user than config.json
// says; the cache would mix two users' data.
var errWrongUser = errors.New("key belongs to another user (log in again)")

// Run connects, syncs and keeps the connection alive, reconnecting with
// jittered exponential backoff, until ctx is done, Close is called, or the
// server rejects us for good (see Status.Stopped). Call it once.
func (c *Client) Run(ctx context.Context) {
	if !c.running.CompareAndSwap(false, true) {
		return
	}
	c.mu.Lock()
	if c.ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	done := make(chan struct{})
	c.runDone = done
	c.mu.Unlock()
	defer close(done)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(c.ctx, cancel)()

	backoff := c.timing.backoffMin
	for {
		start := time.Now()
		online, err := c.connect(ctx)
		if ctx.Err() != nil {
			c.setStatus(Status{State: Offline})
			return
		}
		// Only a session that stayed up resets the backoff, so a server that
		// syncs and immediately drops us can't make us reconnect in a hot loop.
		restart := errors.Is(err, errRestart)
		if online && (restart || time.Since(start) >= stableSession) {
			backoff = c.timing.backoffMin
		}
		st := Status{State: Offline, Stopped: fatal(err)}
		if err != nil {
			st.Err = sanitize.Line(err.Error())
		}
		c.setStatus(st)
		if st.Stopped {
			return
		}
		d := backoff/2 + rand.N(backoff/2+1)
		if !restart || !online {
			backoff = min(backoff*2, c.timing.backoffMax)
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			c.setStatus(Status{State: Offline})
			return
		case <-t.C:
		}
	}
}

// connect runs one connection to completion. online reports whether it got
// through sync (which resets the backoff).
func (c *Client) connect(ctx context.Context) (online bool, err error) {
	c.setStatus(Status{State: Connecting})
	signer, err := identity.Load(c.cfg.KeySpec())
	if err != nil {
		return false, err
	}
	dctx, cancel := context.WithTimeout(ctx, c.timing.dial)
	conn, err := transport.Dial(dctx, c.cfg.Addr, c.cfg.Pin)
	cancel()
	if err != nil {
		return false, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	codec := proto.NewCodec(conn)
	w, err := handshake(conn, codec, signer, "", "", c.AgentHandle(), c.passive.Load(), c.timing.handshake)
	if err != nil {
		conn.Close()
		return false, err
	}
	if w.Agent != nil {
		c.followRename(*w.Agent) // renamed while we were away
	}
	if w.User.ID != c.cfg.UserID {
		conn.Close()
		return false, fmt.Errorf("%w: server says user %d, config says %d", errWrongUser, w.User.ID, c.cfg.UserID)
	}
	pinSID, err := c.checkServer(w)
	if err != nil {
		conn.Close()
		return false, err
	}
	if err := c.applyUsers([]proto.User{w.User}); err != nil {
		conn.Close()
		return false, err
	}

	s := newSession(conn, codec, w, c.timing.recvTimeout)
	c.setStatus(Status{State: Syncing, Transport: conn.Kind()})
	sctx, scancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	readErr := make(chan error, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		readErr <- c.readLoop(s)
		s.fail()
	}()
	go func() {
		defer wg.Done()
		c.keepalive(s)
	}()

	since, sid, err := c.syncPoint()
	if err == nil {
		_, err = s.call(sctx, proto.OpSync, proto.SyncArgs{Since: since, ServerID: sid}, c.syncHook(s))
	}
	if err == nil && pinSID != nil {
		// First successful sync with this address and pin: trust its id.
		err = c.trust.PinServer(c.cfg.Addr, c.cfg.Pin, *pinSID)
	}
	if err == nil {
		c.checkDowngrade(s)
		// Negative: never equal to a keepalive PING's unix-ms t.
		s.rosterMark.Store(-1 - time.Now().UnixNano()&(1<<62-1))
		_ = s.send(proto.TPing, proto.Ping{T: s.rosterMark.Load()})
		online = true
		if s.e2e() {
			// Before publishing s: RefreshDevices reads it without waiting.
			s.sshPub = signer.PublicKey().Marshal()
		}
		c.mu.Lock()
		c.sess = s
		away := c.away
		c.mu.Unlock()
		c.setStatus(Status{State: Online, Transport: conn.Kind()})
		c.pushReads(s)
		if away != nil {
			_ = s.send(proto.TEphemeral, *away)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.outboxLoop(sctx, s)
		}()
		if s.e2e() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.e2eLoop(sctx, s, signer)
			}()
		}
		if s.agent1() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.agentAfterSync(sctx)
			}()
		}
		err = <-readErr
	} else {
		s.fail()
		if rerr := <-readErr; errors.Is(err, ErrOffline) && rerr != nil {
			err = rerr // the read loop knows why (BYE, EOF, ...)
		}
	}
	s.fail()
	scancel()
	switch {
	case s.timedOut.Load():
		err = errTimeout
	case s.restarting.Load():
		err = errRestart
	}
	c.mu.Lock()
	if c.sess == s {
		c.sess = nil
	}
	typing := map[uint64]bool{}
	for ch := range c.typing {
		typing[ch] = true
	}
	c.typing = map[uint64]map[uint64]time.Time{}
	c.presence = map[uint64]Presence{}
	c.mu.Unlock()
	c.clearHuddles()
	c.clearAgentLive()
	wg.Wait()
	c.post(Update{Presence: true, Typing: typing})
	return online, err
}

var errTimeout = errors.New("connection timed out")

// keepalive PINGs after pingIdle without sending anything (PROTOCOL.md §9).
func (c *Client) keepalive(s *session) {
	t := time.NewTicker(c.timing.tick)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-t.C:
			if now.Sub(time.Unix(0, s.lastSend.Load())) >= c.timing.pingIdle {
				if err := s.send(proto.TPing, proto.Ping{T: now.UnixMilli()}); err != nil {
					s.fail()
					return
				}
			}
		}
	}
}

func (c *Client) readLoop(s *session) error {
	for {
		f, err := s.codec.Read()
		if err != nil {
			return err
		}
		s.watchdog.Reset(c.timing.recvTimeout)
		switch f.Type {
		case proto.TRes:
			var r proto.Res
			if err := f.Decode(&r); err != nil {
				return fmt.Errorf("bad RES: %w", err)
			}
			if err := s.resolve(r); err != nil {
				return err
			}
		case proto.TEvent:
			var ev proto.Event
			if err := f.Decode(&ev); err != nil {
				return fmt.Errorf("bad EVENT: %w", err)
			}
			u, err := c.applyEvent(ev)
			if err != nil {
				return fmt.Errorf("applying event %d: %w", ev.Seq, err)
			}
			c.post(u)
			if e2eKind(ev.Kind) {
				c.kickE2E()
			}
		case proto.TSnapshot:
			var sn proto.Snapshot
			if err := f.Decode(&sn); err != nil {
				return fmt.Errorf("bad SNAPSHOT: %w", err)
			}
			if err := c.applySnapshot(s, sn); err != nil {
				return fmt.Errorf("applying snapshot: %w", err)
			}
			c.kickE2E()
		case proto.TPong:
			var p proto.Ping
			if f.Decode(&p) == nil && p.T < 0 && p.T == s.rosterMark.Load() {
				c.huddlesLive(time.Now())
			}
		case proto.TEphemeral:
			var e proto.Ephemeral
			if f.Decode(&e) == nil {
				c.ephemeral(e)
			}
		case proto.TPing:
			err := s.codec.WriteRaw(proto.TPong, f.Payload)
			s.lastSend.Store(time.Now().UnixNano())
			if err != nil {
				return err
			}
		case proto.TVoice:
			// Only where voice rides the stream (§14.4), and never more than a
			// datagram: a misbehaving server can't park megabytes here.
			if len(f.Payload) > proto.MaxDatagram || !s.voiceTCP() {
				break
			}
			select {
			case s.voiceIn <- f.Payload:
			default:
			}
		case proto.TBye:
			var b proto.Bye
			_ = f.Decode(&b)
			return &ByeError{Code: b.Code, Msg: b.Msg}
		}
	}
}
