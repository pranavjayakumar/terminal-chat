package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
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

	synced       bool
	lastTyping   map[uint64]time.Time
	lastPresence time.Time
	lastRecv     time.Time
	recvMu       sync.Mutex
}

func (s *Server) handle(ctx context.Context, conn transport.Conn) {
	sess := &session{
		srv: s, conn: conn, codec: proto.NewCodec(conn),
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		lastTyping: map[uint64]time.Time{},
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
		sendBye(sess.codec, conn, code, msg)
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
	if err := sess.codec.Write(proto.TWelcome, proto.Welcome{
		Server: Version, Caps: []string{proto.CapHooks}, ServerID: s.st.serverID, User: user, Time: s.now(),
	}); err != nil {
		return
	}
	go sess.writer()
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
	exp, err := sess.conn.Exporter()
	if err != nil {
		return proto.User{}, proto.ByeProtocolError, "no TLS exporter"
	}
	pk, err := identity.Verify(h.Auth.Pubkey, h.Auth.Sig, exp)
	if err != nil {
		return proto.User{}, proto.ByeAuthFailed, ""
	}
	fp := identity.Fingerprint(pk)

	if h.Auth.Invite == "" {
		u, err := userByKey(ctx, s.st.db, fp)
		if err != nil {
			return proto.User{}, proto.ByeAuthFailed, "unknown key"
		}
		if u.Disabled {
			return proto.User{}, proto.ByeRevoked, ""
		}
		return u, "", ""
	}
	return s.redeem(ctx, h.Auth, pk.Marshal(), fp)
}

var errBye = errors.New("bye")

func (s *Server) redeem(ctx context.Context, a proto.Auth, pubkey []byte, fp string) (proto.User, string, string) {
	var user proto.User
	byeCode, byeMsg := "", ""
	fail := func(code, msg string) error { byeCode, byeMsg = code, msg; return errBye }

	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		now := s.now()
		var admin bool
		var target sql.NullInt64
		var expires int64
		var used bool
		err := tx.QueryRowContext(ctx, `SELECT admin, user, expires, used FROM invites WHERE token = ?`,
			tokenHash(a.Invite)).Scan(&admin, &target, &expires, &used)
		if err != nil || used || expires < now {
			return nil, fail(proto.ByeInviteInvalid, "invite is invalid, used or expired")
		}
		if _, err := userByKey(ctx, tx, fp); err == nil {
			return nil, fail(proto.ByeKeyExists, "this key is already registered; log in without an invite")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invites SET used = 1 WHERE token = ?`, tokenHash(a.Invite)); err != nil {
			return nil, err
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
			var n int
			tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE name = ?`, a.Name).Scan(&n)
			if n > 0 {
				return nil, fail(proto.ByeNameTaken, "")
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO users (name, admin, created) VALUES (?, ?, ?)`, a.Name, admin, now)
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

func sendBye(c *proto.Codec, conn transport.Conn, code, msg string) {
	// A peer that isn't reading (slow consumer) would block the write forever.
	t := time.AfterFunc(time.Second, func() { conn.CloseWithCode(code) })
	defer t.Stop()
	if c.Write(proto.TBye, proto.Bye{Code: code, Msg: msg}) == nil {
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

func (sess *session) respond(id uint32, data any, err error) {
	res := proto.Res{ID: id, OK: err == nil}
	if err != nil {
		var pe *proto.Error
		if !errors.As(err, &pe) {
			sess.srv.log.Error("op failed", "user", sess.user.Name, "err", err)
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
		if s.presence[e.User] == p {
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
		evs, err := visibleEvents(ctx, tx, u, since, h, replayPage)
		if err != nil {
			return proto.SyncRes{}, err
		}
		for _, ev := range evs {
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
	if err := sess.pushSnapshot(proto.Snapshot{Users: users, Read: read}); err != nil {
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

	var part []proto.Message
	size := 0
	for _, c := range chans {
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
