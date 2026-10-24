package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

const (
	handshakeTimeout = 30 * time.Second
	idleTimeout      = 45 * time.Second
	typingInterval   = 3 * time.Second
	replayPage       = 500
	// maxQueueBytes caps a session's unsent frames. A client that keeps
	// requesting without reading gets BYE slow_consumer instead of
	// growing server memory.
	maxQueueBytes = 8 << 20
	presenceEvery = time.Second
)

type outFrame struct {
	t       proto.FrameType
	payload []byte
	seq     uint64 // EVENT seq, so goLive can drop events sync already replayed
}

type session struct {
	srv   *Server
	conn  transport.Conn
	codec *proto.Codec
	user  proto.User

	mu      sync.Mutex
	queue   []outFrame
	queued  int        // bytes in queue (not yet taken by the writer)
	pending []outFrame // live events buffered while sync replays (§6 step 1)
	live    bool
	closed  bool
	wake    chan struct{}
	done    chan struct{}

	synced  bool
	e2e     bool     // both sides advertised proto.CapE2E
	byeKeys []string // Bye.Keys for a failed handshake (cap key_hint)
	voice   bool     // both sides advertised proto.CapVoice
	passive bool     // proto.CapPassive: no presence, typing or huddles
	// agentCap: both sides advertised proto.CapAgent. agent is set on an
	// agent session (HELLO agent, §15.1): it never counts as the user
	// being online and sees only its DM and access list's messages.
	agentCap    bool
	agent       *proto.Agent
	agentBucket bucket // EphAgent rate, under srv.mu
	// Voice datagrams (huddle.go); dg is nil unless voice over QUIC.
	dg           transport.DatagramConn
	dgOut        chan dgItem
	dgBucket     bucket // only the datagram reader goroutine touches it
	lastTyping   map[uint64]time.Time
	lastKeyNeed  map[uint64]time.Time // under srv.mu
	lastPresence time.Time
	lastRecv     time.Time
	recvMu       sync.Mutex
}

func (s *Server) handle(ctx context.Context, conn transport.Conn) {
	sess := &session{
		srv: s, conn: conn, codec: proto.NewCodec(conn),
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		lastTyping: map[uint64]time.Time{}, lastKeyNeed: map[uint64]time.Time{},
	}
	defer conn.Close()

	timer := time.AfterFunc(handshakeTimeout, func() {
		sendBye(sess.codec, conn, proto.ByeHandshakeTimeout, "")
	})
	user, code, msg := s.handshake(ctx, sess)
	if !timer.Stop() {
		return
	}
	if code != "" {
		sendBye(sess.codec, conn, code, msg, sess.byeKeys...)
		return
	}
	sess.user = user
	sess.codec.SetMaxFrame(proto.MaxFrame)

	// Register before WELCOME so a concurrent user.disable can't miss us;
	// frames queued meanwhile wait for the writer.
	if !s.register(sess) {
		sendBye(sess.codec, conn, proto.ByeRevoked, "")
		return
	}
	defer s.unregister(sess)
	w := proto.Welcome{
		Server: Version, Caps: append([]string{proto.CapHooks, proto.CapE2E, proto.CapPins, proto.CapGroups, proto.CapRemind, proto.CapVoice, proto.CapAgent}, s.inviteCaps()...), ServerID: s.st.serverID, User: user, Time: s.now(),
		Agent: sess.agent,
	}
	if sess.passive {
		w.Caps = append(w.Caps, proto.CapPassive)
	}
	if tamperEnabled {
		s.tamper.welcome(user.ID, &w)
	}
	if err := sess.codec.Write(proto.TWelcome, w); err != nil {
		return
	}
	go sess.writer()
	if dg, ok := transport.Datagrams(conn); ok && sess.voice {
		dctx, cancel := context.WithCancel(ctx)
		defer cancel()
		sess.startDatagrams(dctx, dg)
	}
	sess.readLoop(ctx)
}

// handshake reads HELLO and authenticates it. On failure it returns a BYE code.
func (s *Server) handshake(ctx context.Context, sess *session) (proto.User, string, string) {
	f, err := sess.codec.Read()
	if errors.Is(err, proto.ErrFrameTooLarge) {
		return proto.User{}, proto.ByeFrameTooLarge, ""
	}
	if err != nil || f.Type != proto.THello {
		return proto.User{}, proto.ByeProtocolError, "expected HELLO"
	}
	var h proto.Hello
	if err := f.Decode(&h); err != nil {
		return proto.User{}, proto.ByeProtocolError, "bad HELLO"
	}
	sess.e2e = slices.Contains(h.Caps, proto.CapE2E)
	sess.voice = slices.Contains(h.Caps, proto.CapVoice)
	sess.passive = slices.Contains(h.Caps, proto.CapPassive)
	sess.agentCap = slices.Contains(h.Caps, proto.CapAgent)
	exp, err := sess.conn.Exporter()
	if err != nil {
		return proto.User{}, proto.ByeProtocolError, "no TLS exporter"
	}
	pk, err := identity.Verify(h.Auth.Pubkey, h.Auth.Sig, exp)
	if err != nil {
		return proto.User{}, proto.ByeAuthFailed, ""
	}
	fp := identity.Fingerprint(pk)

	// A registered key logs in whatever invite comes with it, which stays
	// unspent (§4): the key alone already proves who this is, and so a join
	// retried after a lost WELCOME, or an invite reused on a second
	// computer, signs in instead of failing.
	if u, err := userByKey(ctx, s.st.db, fp); err == nil {
		if u.Disabled {
			return proto.User{}, proto.ByeRevoked, ""
		}
		if h.Agent != "" {
			if code, msg := s.agentLogin(ctx, sess, u, h.Agent); code != "" {
				return proto.User{}, code, msg
			}
		}
		return u, "", ""
	}
	if h.Agent != "" {
		return proto.User{}, proto.ByeAuthFailed, "unknown key"
	}
	if h.Auth.Invite == "" {
		return proto.User{}, proto.ByeAuthFailed, "unknown key"
	}
	var hint *[]string
	if slices.Contains(h.Caps, proto.CapKeyHint) {
		hint = &sess.byeKeys
	}
	return s.redeem(ctx, h.Auth, pk.Marshal(), fp, hint)
}

var errBye = errors.New("bye")

// redeem redeems a.Invite for a key not yet registered. A key-bound invite
// refusing fp stores the fingerprints it accepts in *hint (if non-nil).
func (s *Server) redeem(ctx context.Context, a proto.Auth, pubkey []byte, fp string, hint *[]string) (proto.User, string, string) {
	var user proto.User
	var hintFPs []string
	byeCode, byeMsg := "", ""
	fail := func(code, msg string) error { byeCode, byeMsg = code, msg; return errBye }

	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		now := s.now()
		hintFPs = nil // in case the transaction is ever retried
		var admin bool
		var target sql.NullInt64
		var expires, uses int64
		var used bool
		var createdBy uint64
		var stored string // tokenHash or boundTokenHash (invites.go)
		err := tx.QueryRowContext(ctx, `SELECT token, admin, user, expires, used, uses, created_by FROM invites WHERE token IN (?, ?)`,
			tokenHash(a.Invite), boundTokenHash(a.Invite)).Scan(&stored, &admin, &target, &expires, &used, &uses, &createdBy)
		// expires <= now matches invite.list, revoke and the member cap,
		// which count an invite as live only while expires > now.
		if err != nil || used || uses < 1 || expires <= now {
			return nil, fail(proto.ByeInviteInvalid, "invite is invalid, used or expired")
		}
		if msg, fps, err := s.redeemCheck(ctx, tx, stored, createdBy, target.Valid, fp); err != nil {
			return nil, err
		} else if msg != "" {
			hintFPs = fps
			return nil, fail(proto.ByeInviteInvalid, msg)
		}
		// Registered since handshake looked (a concurrent redemption).
		if _, err := userByKey(ctx, tx, fp); err == nil {
			return nil, fail(proto.ByeKeyExists, "this key is already registered; log in without an invite (tc login)")
		}
		// Conditional so a use can never be spent twice, whatever serializes
		// the callers; the rest of the transaction rolls back with it.
		res, err := tx.ExecContext(ctx,
			`UPDATE invites SET uses = uses - 1, used = (uses - 1 <= 0) WHERE token = ? AND uses > 0 AND used = 0`,
			stored)
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return nil, fail(proto.ByeInviteInvalid, "invite is invalid, used or expired")
		}

		var evs []proto.Event
		if target.Valid {
			user, err = getUser(ctx, tx, uint64(target.Int64))
			if err != nil || user.Disabled || user.Bot {
				return nil, fail(proto.ByeInviteInvalid, "invite user is gone")
			}
		} else {
			if !proto.ValidName(a.Name) {
				return nil, fail(proto.ByeProtocolError, "name must match ^[a-z0-9][a-z0-9_-]{0,31}$")
			}
			if strings.HasSuffix(a.Name, proto.BotSuffix) {
				return nil, fail(proto.ByeProtocolError, `names ending in "-bot" are reserved for webhook bots`)
			}
			if a.Name == proto.NameMe {
				return nil, fail(proto.ByeProtocolError, `the name "me" is reserved (@me means your own notes); pick another`)
			}
			var n int
			tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE name = ?`, a.Name).Scan(&n)
			if n > 0 || groupNameTaken(ctx, tx, a.Name) || agentHandleTaken(ctx, tx, a.Name) {
				return nil, fail(proto.ByeNameTaken, "")
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO users (name, admin, created, invited_by) VALUES (?, ?, ?, ?)`,
				a.Name, admin, now, createdBy)
			if err != nil {
				return nil, err
			}
			id, _ := res.LastInsertId()
			user = proto.User{ID: uint64(id), Name: a.Name, Admin: admin, Created: now}
			ev, err := appendEvent(ctx, tx, proto.KUserNew, 0, user.ID, 0, now, user)
			if err != nil {
				return nil, err
			}
			evs = append(evs, ev)
			if ev, ok, err := joinGeneral(ctx, tx, now, user.ID); err != nil {
				return nil, err
			} else if ok {
				evs = append(evs, ev)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO keys (fp, user, pubkey, created) VALUES (?, ?, ?, ?)`,
			fp, user.ID, pubkey, now); err != nil {
			return nil, err
		}
		return evs, nil
	})
	if byeCode != "" {
		if hint != nil && byeCode == proto.ByeInviteInvalid {
			*hint = hintFPs
		}
		return proto.User{}, byeCode, byeMsg
	}
	if err != nil {
		s.log.Error("redeeming invite", "err", err)
		return proto.User{}, proto.ByeProtocolError, "server error"
	}
	return user, "", ""
}

func (s *Server) register(sess *session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled[sess.user.ID] {
		return false
	}
	s.sessions[sess] = struct{}{}
	if sess.agent != nil { // the agent being here isn't its owner being here
		s.agentOnlineLocked(sess, 1)
		return true
	}
	if sess.passive { // a background listener isn't the user being here
		return true
	}
	s.online[sess.user.ID]++
	if s.online[sess.user.ID] == 1 {
		p := proto.Ephemeral{Kind: proto.EphPresence, User: sess.user.ID, State: proto.PresOnline}
		s.presence[sess.user.ID] = p
		s.ephemeralLocked(p, nil, sess)
	}
	return true
}

func (s *Server) unregister(sess *session) {
	sess.close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sess]; !ok {
		return
	}
	delete(s.sessions, sess)
	s.huddleGoneLocked(sess)
	if sess.agent != nil {
		s.agentOnlineLocked(sess, -1)
		return
	}
	if sess.passive {
		return
	}
	s.online[sess.user.ID]--
	if s.online[sess.user.ID] <= 0 {
		delete(s.online, sess.user.ID)
		delete(s.presence, sess.user.ID)
		s.ephemeralLocked(proto.Ephemeral{Kind: proto.EphPresence, User: sess.user.ID, State: proto.PresOffline}, nil, sess)
	}
}

// ---- outbound queue ----

// push queues one of this session's own frames (RES, PONG), capped by bytes.
func (sess *session) push(f outFrame) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed {
		return
	}
	if sess.queued+len(f.payload) > maxQueueBytes {
		sess.slowLocked()
		return
	}
	sess.enqueueLocked(f)
}

// pushSync queues sync output (snapshot/replay). It is produced once per
// connection and bounded by the database, so it bypasses the cap.
func (sess *session) pushSync(f outFrame) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if !sess.closed {
		sess.enqueueLocked(f)
	}
}

func (sess *session) enqueueLocked(f outFrame) {
	sess.queue = append(sess.queue, f)
	sess.queued += len(f.payload)
	sess.signal()
}

// pushLive queues a fan-out frame. Called with srv.mu held; never blocks.
func (sess *session) pushLive(f outFrame) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed {
		return
	}
	if !sess.live {
		if len(sess.pending) >= proto.MaxQueue {
			sess.slowLocked()
			return
		}
		sess.pending = append(sess.pending, f)
		return
	}
	if len(sess.queue) >= proto.MaxQueue || sess.queued+len(f.payload) > maxQueueBytes {
		sess.slowLocked()
		return
	}
	sess.enqueueLocked(f)
}

func (sess *session) slowLocked() {
	sess.closed = true
	sess.queue, sess.pending = nil, nil
	go sendBye(sess.codec, sess.conn, proto.ByeSlowConsumer, "")
}

// goLive flushes events buffered during sync and switches to live delivery.
// Events at or below the sync head h were already delivered by the sync.
func (sess *session) goLive(h uint64) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	for _, f := range sess.pending {
		if f.t != proto.TEvent || f.seq > h {
			sess.enqueueLocked(f)
		}
	}
	sess.pending = nil
	sess.live = true
	sess.signal()
}

func (sess *session) signal() {
	select {
	case sess.wake <- struct{}{}:
	default:
	}
}

func (sess *session) close() {
	sess.mu.Lock()
	if !sess.closed {
		sess.closed = true
		sess.queue, sess.pending = nil, nil
	}
	sess.mu.Unlock()
	select {
	case <-sess.done:
	default:
		close(sess.done)
	}
}

// bye sends BYE and closes. Safe to call with srv.mu held.
func (sess *session) bye(code, msg string) {
	go sendBye(sess.codec, sess.conn, code, msg)
}

// byeGrace lets a BYE reach the peer before the close: QUIC's
// CloseWithError discards unsent stream data.
const byeGrace = 200 * time.Millisecond

func sendBye(c *proto.Codec, conn transport.Conn, code, msg string, keys ...string) {
	// A peer that isn't reading (slow consumer) would block the write forever.
	t := time.AfterFunc(time.Second, func() { conn.CloseWithCode(code) })
	defer t.Stop()
	if c.Write(proto.TBye, proto.Bye{Code: code, Msg: msg, Keys: keys}) == nil {
		time.Sleep(byeGrace)
	}
	conn.CloseWithCode(code)
}

func (sess *session) writer() {
	for {
		select {
		case <-sess.done:
			return
		case <-sess.wake:
		}
		sess.mu.Lock()
		batch := sess.queue
		sess.queue, sess.queued = nil, 0
		sess.mu.Unlock()
		for _, f := range batch {
			if tamperEnabled {
				for _, g := range sess.srv.tamper.out(sess.user.ID, f) {
					if err := sess.codec.WriteRaw(g.t, g.payload); err != nil {
						sess.conn.Close()
						return
					}
				}
				continue
			}
			if err := sess.codec.WriteRaw(f.t, f.payload); err != nil {
				sess.conn.Close()
				return
			}
		}
	}
}

// ---- inbound ----

func (sess *session) readLoop(ctx context.Context) {
	sess.touch()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				sess.recvMu.Lock()
				idle := time.Since(sess.lastRecv)
				sess.recvMu.Unlock()
				if idle > idleTimeout {
					sess.conn.CloseWithCode("idle")
					return
				}
			}
		}
	}()

	for {
		f, err := sess.codec.Read()
		if err != nil {
			switch {
			case errors.Is(err, proto.ErrFrameTooLarge):
				sendBye(sess.codec, sess.conn, proto.ByeFrameTooLarge, "")
			case errors.Is(err, proto.ErrEmptyFrame):
				sendBye(sess.codec, sess.conn, proto.ByeProtocolError, "empty frame")
			}
			return
		}
		sess.touch()
		switch f.Type {
		case proto.TReq:
			var r proto.Req
			if err := f.Decode(&r); err != nil {
				sess.bye(proto.ByeProtocolError, "bad REQ")
				return
			}
			sess.handleReq(ctx, r)
		case proto.TPing:
			sess.push(outFrame{t: proto.TPong, payload: f.Payload})
		case proto.TEphemeral:
			var e proto.Ephemeral
			if f.Decode(&e) == nil {
				sess.srv.handleEphemeral(sess, e)
			}
		case proto.TBye:
			return
		}
	}
}

func (sess *session) touch() {
	sess.recvMu.Lock()
	sess.lastRecv = time.Now()
	sess.recvMu.Unlock()
}

// opErrLevel is how loudly an unexpected op error is logged: an op cut short
// because its session closed or the server is stopping is routine.
func opErrLevel(err error) slog.Level {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return slog.LevelDebug
	}
	return slog.LevelError
}

func (sess *session) respond(id uint32, data any, err error) {
	res := proto.Res{ID: id, OK: err == nil}
	if err != nil {
		var pe *proto.Error
		if !errors.As(err, &pe) {
			sess.srv.log.Log(context.Background(), opErrLevel(err), "op failed", "user", sess.user.Name, "err", err)
			pe = proto.Errf(proto.ErrInternal, "")
		}
		res.Err = pe
	} else if data != nil {
		raw, merr := proto.Marshal(data)
		if merr != nil {
			res.OK, res.Err = false, proto.Errf(proto.ErrInternal, "")
		} else {
			res.Data = raw
		}
	}
	payload, merr := proto.Marshal(res)
	if merr != nil {
		return
	}
	sess.push(outFrame{t: proto.TRes, payload: payload})
}

func (sess *session) handleReq(ctx context.Context, r proto.Req) {
	if r.Op == proto.OpSync {
		if sess.synced {
			sess.respond(r.ID, nil, proto.Errf(proto.ErrBadRequest, "sync already done"))
			return
		}
		res, err := sess.srv.sync(ctx, sess, r.Args)
		sess.respond(r.ID, res, err)
		if err == nil {
			sess.synced = true
			sess.goLive(res.Head)
			sess.srv.sendPresence(sess)
			sess.srv.sendAgentPresence(sess)
			sess.srv.sendHuddles(sess)
		}
		return
	}
	if !sess.synced {
		sess.respond(r.ID, nil, proto.Errf(proto.ErrBadRequest, "sync must be the first request"))
		return
	}
	data, err := sess.srv.dispatch(ctx, sess, r.Op, r.Args)
	sess.respond(r.ID, data, err)
}

func (s *Server) sendPresence(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.presence {
		if payload, err := proto.Marshal(p); err == nil {
			sess.push(outFrame{t: proto.TEphemeral, payload: payload})
		}
	}
}

func (s *Server) handleEphemeral(sess *session, e proto.Ephemeral) {
	if !sess.synced {
		return
	}
	e.User = sess.user.ID
	switch e.Kind {
	case proto.EphKeyNeed:
		s.keyNeed(sess, e)
		return
	case proto.EphAgent:
		s.agentEph(sess, e)
		return
	}
	// Passive sessions only listen; an agent's presence is agent.presence.
	if sess.passive || sess.agent != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Kind {
	case proto.EphTyping:
		if !s.members[e.Chan][e.User] || time.Since(sess.lastTyping[e.Chan]) < typingInterval/2 {
			return
		}
		sess.lastTyping[e.Chan] = time.Now()
		s.ephemeralLocked(proto.Ephemeral{Kind: proto.EphTyping, Chan: e.Chan, User: e.User}, s.members[e.Chan], sess)
	case proto.EphPresence:
		if e.State != proto.PresOnline && e.State != proto.PresAway {
			return
		}
		if !validLine(e.Status, proto.MaxStatus) || time.Since(sess.lastPresence) < presenceEvery {
			return
		}
		p := proto.Ephemeral{Kind: proto.EphPresence, User: e.User, State: e.State, Status: e.Status}
		if old := s.presence[e.User]; old.State == p.State && old.Status == p.Status {
			return
		}
		sess.lastPresence = time.Now()
		s.presence[e.User] = p
		s.ephemeralLocked(p, nil, nil)
	}
}

// ---- sync (PROTOCOL.md §6) ----

func (s *Server) sync(ctx context.Context, sess *session, args proto.Raw) (proto.SyncRes, error) {
	var a proto.SyncArgs
	if err := proto.Unmarshal(args, &a); err != nil {
		return proto.SyncRes{}, proto.Errf(proto.ErrBadRequest, "bad sync args")
	}

	// Taking the head and opening the read snapshot under the write lock
	// gives a consistent view at exactly H; events after H are buffered in
	// sess.pending because the session isn't live yet.
	s.mu.Lock()
	tx, err := s.st.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		s.mu.Unlock()
		return proto.SyncRes{}, err
	}
	defer tx.Rollback()
	h, err := head(ctx, tx)
	s.mu.Unlock()
	if err != nil {
		return proto.SyncRes{}, err
	}

	u := sess.user.ID
	reset := a.Since == 0 || !bytes.Equal(a.ServerID, s.st.serverID) || a.Since > h || h-a.Since > proto.SnapshotGap
	if reset {
		return proto.SyncRes{Head: h, Reset: true}, s.sendSnapshot(ctx, tx, sess)
	}
	for since := a.Since; ; {
		evs, err := visibleEvents(ctx, tx, u, since, h, replayPage, sess.e2e, sess.agentCap)
		if err != nil {
			return proto.SyncRes{}, err
		}
		for _, ev := range evs {
			if proto.IsMsgKind(ev.Kind) && !s.agentSees(sess, ev.Chan) {
				continue
			}
			payload, err := proto.Marshal(ev)
			if err != nil {
				return proto.SyncRes{}, err
			}
			sess.pushSync(outFrame{t: proto.TEvent, payload: payload})
		}
		if len(evs) < replayPage {
			break
		}
		since = evs[len(evs)-1].Seq
	}
	return proto.SyncRes{Head: h}, nil
}

const snapshotPart = 512 << 10

func (s *Server) sendSnapshot(ctx context.Context, tx *sql.Tx, sess *session) error {
	u := sess.user.ID
	users, err := listUsers(ctx, tx)
	if err != nil {
		return err
	}
	chans, err := visibleChannels(ctx, tx, u)
	if err != nil {
		return err
	}
	read, err := readMarkers(ctx, tx, u)
	if err != nil {
		return err
	}
	groups, err := listGroups(ctx, tx)
	if err != nil {
		return err
	}
	first := proto.Snapshot{Users: users, Read: read, Groups: groups}
	if sess.agentCap {
		if first.Agents, err = listAgents(ctx, tx); err != nil {
			return err
		}
		s.markAgentsOnline(first.Agents)
	}
	if err := sess.pushSnapshot(first); err != nil {
		return err
	}
	var cpart []proto.Channel
	csize := 0
	for _, c := range chans {
		n := encodedSize(c)
		if csize+n > snapshotPart && len(cpart) > 0 {
			if err := sess.pushSnapshot(proto.Snapshot{Chans: cpart}); err != nil {
				return err
			}
			cpart, csize = nil, 0
		}
		cpart = append(cpart, c)
		csize += n
	}
	if len(cpart) > 0 {
		if err := sess.pushSnapshot(proto.Snapshot{Chans: cpart}); err != nil {
			return err
		}
	}

	if sess.e2e {
		if err := sess.snapshotDevices(ctx, tx); err != nil {
			return err
		}
		if err := sess.snapshotKeys(ctx, tx, chans); err != nil {
			return err
		}
	}

	var part []proto.Message
	size := 0
	for _, c := range chans {
		if !s.agentSees(sess, c.ID) {
			continue
		}
		msgs, err := scanMessages(ctx, tx, `SELECT * FROM (SELECT `+msgCols+` FROM messages
			WHERE chan = ? ORDER BY id DESC LIMIT ?) ORDER BY id`, c.ID, proto.SnapshotPerChan)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			n := encodedSize(m)
			if size+n > snapshotPart && len(part) > 0 {
				if err := sess.pushSnapshot(proto.Snapshot{Msgs: part}); err != nil {
					return err
				}
				part, size = nil, 0
			}
			part = append(part, m)
			size += n
		}
	}
	if len(part) > 0 {
		return sess.pushSnapshot(proto.Snapshot{Msgs: part})
	}
	return nil
}

func (sess *session) pushSnapshot(sn proto.Snapshot) error {
	payload, err := proto.Marshal(sn)
	if err != nil {
		return err
	}
	sess.pushSync(outFrame{t: proto.TSnapshot, payload: payload})
	return nil
}

// revoke stops all delivery to the session and closes it with BYE revoked.
// Called with srv.mu held.
func (sess *session) revoke() {
	sess.mu.Lock()
	sess.closed = true
	sess.queue, sess.pending = nil, nil
	sess.mu.Unlock()
	sess.bye(proto.ByeRevoked, "")
}

// encodedSize is v's msgpack size, for packing results under the frame limit.
func encodedSize(v any) int {
	b, err := proto.Marshal(v)
	if err != nil {
		return proto.MaxFrame
	}
	return len(b)
}
