package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

type testServer struct {
	*Server
	addr string
	kind string
	stop func() // cancels Serve and waits for it
}

func startServer(t *testing.T, kind string) *testServer {
	t.Helper()
	dir := t.TempDir()
	srv, err := New(Config{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.Listen("127.0.0.1:0", srv.Cert())
	if err != nil {
		t.Fatal(err)
	}
	srv.cfg.PublicAddr = ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.Serve(ctx, ln)
		close(done)
	}()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(func() {
		stop()
		srv.Close()
	})
	return &testServer{Server: srv, addr: ln.Addr().String(), kind: kind, stop: stop}
}

type tclient struct {
	t       *testing.T
	conn    transport.Conn
	codec   *proto.Codec
	frames  chan proto.Frame
	signer  ssh.Signer
	user    proto.User
	nextID  uint32
	held    []proto.Frame
	welcome proto.Welcome
}

func newKey(t *testing.T) ssh.Signer {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (ts *testServer) dial(t *testing.T) (transport.Conn, *proto.Codec) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dial := transport.DialTCP
	if ts.kind == "quic" {
		dial = transport.DialQUIC
	}
	conn, err := dial(ctx, ts.addr, ts.Pin())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, proto.NewCodec(conn)
}

// connect performs the handshake. It returns the client, or the BYE code.
func (ts *testServer) connect(t *testing.T, key ssh.Signer, invite, name string) (*tclient, string) {
	t.Helper()
	return ts.connectCaps(t, key, invite, name, nil)
}

// connectCaps is connect advertising caps in HELLO.
func (ts *testServer) connectCaps(t *testing.T, key ssh.Signer, invite, name string, caps []string) (*tclient, string) {
	t.Helper()
	c, bye := ts.handshakeCaps(t, key, invite, name, caps)
	if c != nil {
		go func() {
			defer close(c.frames)
			for {
				f, err := c.codec.Read()
				if err != nil {
					return
				}
				c.frames <- f
			}
		}()
	}
	return c, bye
}

// handshake is connect without the background reader.
func (ts *testServer) handshake(t *testing.T, key ssh.Signer, invite, name string) (*tclient, string) {
	t.Helper()
	return ts.handshakeCaps(t, key, invite, name, nil)
}

func (ts *testServer) handshakeCaps(t *testing.T, key ssh.Signer, invite, name string, caps []string) (*tclient, string) {
	t.Helper()
	conn, codec := ts.dial(t)
	exp, err := conn.Exporter()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := identity.Sign(key, exp)
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Write(proto.THello, proto.Hello{Client: "test", Caps: caps, Auth: proto.Auth{
		Pubkey: key.PublicKey().Marshal(), Sig: sig, Invite: invite, Name: name}}); err != nil {
		t.Fatal(err)
	}
	f, err := codec.Read()
	if err != nil {
		t.Fatalf("reading welcome: %v", err)
	}
	if f.Type == proto.TBye {
		var b proto.Bye
		f.Decode(&b)
		return nil, b.Code
	}
	var w proto.Welcome
	if f.Type != proto.TWelcome || f.Decode(&w) != nil {
		t.Fatalf("expected WELCOME, got %#x", f.Type)
	}
	codec.SetMaxFrame(proto.MaxFrame)
	return &tclient{t: t, conn: conn, codec: codec, frames: make(chan proto.Frame, 1024), signer: key, user: w.User, welcome: w}, ""
}

func (ts *testServer) register(t *testing.T, name string, admin bool) *tclient {
	t.Helper()
	return ts.registerCaps(t, name, admin, nil)
}

func (ts *testServer) registerCaps(t *testing.T, name string, admin bool, caps []string) *tclient {
	t.Helper()
	code, err := ts.CreateInvite(context.Background(), admin, 0)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := proto.ParseInvite(code)
	if err != nil {
		t.Fatal(err)
	}
	c, bye := ts.connectCaps(t, newKey(t), inv.Token, name, caps)
	if bye != "" {
		t.Fatalf("register %s: BYE %s", name, bye)
	}
	return c
}

// recv returns the next frame: held-back frames first, then the wire.
func (c *tclient) recv(what string) proto.Frame {
	c.t.Helper()
	if len(c.held) > 0 {
		f := c.held[0]
		c.held = c.held[1:]
		return f
	}
	select {
	case f, ok := <-c.frames:
		if !ok {
			c.t.Fatalf("%s: connection closed waiting for %s", c.user.Name, what)
		}
		return f
	case <-time.After(3 * time.Second):
		c.t.Fatalf("%s: timeout waiting for %s", c.user.Name, what)
	}
	panic("unreachable")
}

func (c *tclient) next(want proto.FrameType) proto.Frame {
	c.t.Helper()
	for {
		f := c.recv("frame")
		if f.Type == proto.TEphemeral && want != proto.TEphemeral {
			continue
		}
		if f.Type != want {
			c.t.Fatalf("%s: got frame %#x, want %#x", c.user.Name, f.Type, want)
		}
		return f
	}
}

// quiet asserts no EVENT arrives within d.
func (c *tclient) quiet(d time.Duration) {
	c.t.Helper()
	for _, f := range c.held {
		if f.Type == proto.TEvent {
			c.t.Fatalf("%s: unexpected held event", c.user.Name)
		}
	}
	deadline := time.After(d)
	for {
		select {
		case f := <-c.frames:
			if f.Type == proto.TEvent {
				var ev proto.Event
				f.Decode(&ev)
				c.t.Fatalf("%s: unexpected event %s seq %d", c.user.Name, ev.Kind, ev.Seq)
			}
		case <-deadline:
			return
		}
	}
}

func (c *tclient) req(op string, args any) proto.Res {
	c.t.Helper()
	c.nextID++
	if err := c.codec.Write(proto.TReq, proto.Req{ID: c.nextID, Op: op, Args: proto.MustRaw(args)}); err != nil {
		c.t.Fatal(err)
	}
	return c.res(c.nextID)
}

// res waits for the RES with id; frames that arrive first are held for later.
func (c *tclient) res(id uint32) proto.Res {
	c.t.Helper()
	var skipped []proto.Frame
	defer func() { c.held = append(skipped, c.held...) }()
	for {
		f := c.recv("RES")
		if f.Type != proto.TRes {
			skipped = append(skipped, f)
			continue
		}
		var r proto.Res
		if err := f.Decode(&r); err != nil {
			c.t.Fatal(err)
		}
		if r.ID == id {
			return r
		}
	}
}

func (c *tclient) ok(op string, args any, out any) {
	c.t.Helper()
	r := c.req(op, args)
	if !r.OK {
		c.t.Fatalf("%s %s: %v", c.user.Name, op, r.Err)
	}
	if out != nil {
		if err := proto.Unmarshal(r.Data, out); err != nil {
			c.t.Fatal(err)
		}
	}
}

func (c *tclient) fail(op string, args any, code string) {
	c.t.Helper()
	r := c.req(op, args)
	if r.OK || r.Err == nil || r.Err.Code != code {
		c.t.Fatalf("%s %s: got ok=%v err=%v, want %s", c.user.Name, op, r.OK, r.Err, code)
	}
}

func (c *tclient) event(kind string) proto.Event {
	c.t.Helper()
	for {
		f := c.next(proto.TEvent)
		var ev proto.Event
		if err := f.Decode(&ev); err != nil {
			c.t.Fatal(err)
		}
		if ev.Kind == kind {
			return ev
		}
		if kind == "" {
			return ev
		}
	}
}

// syncFresh does sync{since:0} and collects the snapshot.
func (c *tclient) syncFresh() (proto.Snapshot, proto.SyncRes) {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	c.codec.Write(proto.TReq, proto.Req{ID: id, Op: proto.OpSync, Args: proto.MustRaw(proto.SyncArgs{})})
	var snap proto.Snapshot
	for {
		f := c.recv("snapshot")
		switch f.Type {
		case proto.TSnapshot:
			var part proto.Snapshot
			f.Decode(&part)
			snap.Users = append(snap.Users, part.Users...)
			snap.Chans = append(snap.Chans, part.Chans...)
			snap.Msgs = append(snap.Msgs, part.Msgs...)
			snap.Devs = append(snap.Devs, part.Devs...)
			snap.Revoked = append(snap.Revoked, part.Revoked...)
			snap.RevokedSeqs = append(snap.RevokedSeqs, part.RevokedSeqs...)
			snap.Keys = append(snap.Keys, part.Keys...)
			snap.Groups = append(snap.Groups, part.Groups...)
			if part.Read != nil {
				snap.Read = part.Read
			}
		case proto.TRes:
			var r proto.Res
			f.Decode(&r)
			var sr proto.SyncRes
			proto.Unmarshal(r.Data, &sr)
			if !r.OK || !sr.Reset {
				c.t.Fatalf("fresh sync: ok=%v reset=%v err=%v", r.OK, sr.Reset, r.Err)
			}
			return snap, sr
		}
	}
}

func (c *tclient) send(ch uint64, body string) proto.SendRes {
	c.t.Helper()
	nonce := make([]byte, 16)
	rand.Read(nonce)
	var r proto.SendRes
	c.ok(proto.OpSend, proto.SendArgs{Chan: ch, Body: body, Nonce: nonce}, &r)
	return r
}

func generalID(t *testing.T, snap proto.Snapshot) uint64 {
	for _, c := range snap.Chans {
		if c.Name == "general" {
			return c.ID
		}
	}
	t.Fatal("no #general")
	return 0
}

func forEachTransport(t *testing.T, fn func(t *testing.T, ts *testServer)) {
	for _, kind := range []string{"tcp", "quic"} {
		t.Run(kind, func(t *testing.T) { fn(t, startServer(t, kind)) })
	}
}

func TestChatFlow(t *testing.T) {
	forEachTransport(t, func(t *testing.T, ts *testServer) {
		alice := ts.register(t, "alice", true)
		bob := ts.register(t, "bob", false)
		if !alice.user.Admin || bob.user.Admin {
			t.Fatal("admin flags wrong")
		}
		snapA, _ := alice.syncFresh()
		snapB, _ := bob.syncFresh()
		gen := generalID(t, snapA)
		if generalID(t, snapB) != gen || len(snapB.Users) != 2 {
			t.Fatalf("bob snapshot: %+v", snapB)
		}

		// Sender sees its EVENT before the RES (PROTOCOL.md §5).
		nonce := make([]byte, 16)
		rand.Read(nonce)
		alice.nextID++
		alice.codec.Write(proto.TReq, proto.Req{ID: alice.nextID, Op: proto.OpSend,
			Args: proto.MustRaw(proto.SendArgs{Chan: gen, Body: "hello @bob", Nonce: nonce})})
		ev := alice.event(proto.KMsgNew)
		var sr proto.SendRes
		r := alice.res(alice.nextID)
		proto.Unmarshal(r.Data, &sr)
		if !r.OK || sr.Msg != ev.Seq {
			t.Fatalf("send res %+v, event seq %d", r, ev.Seq)
		}

		got := bob.event(proto.KMsgNew)
		var m proto.Message
		proto.Unmarshal(got.Data, &m)
		if m.Body != "hello @bob" || m.ID != got.Seq || m.Author != alice.user.ID {
			t.Fatalf("bob got %+v", m)
		}

		// Idempotent retry and conflict.
		var again proto.SendRes
		alice.ok(proto.OpSend, proto.SendArgs{Chan: gen, Body: "hello @bob", Nonce: nonce}, &again)
		if again.Msg != sr.Msg {
			t.Fatalf("retry created a new message: %d vs %d", again.Msg, sr.Msg)
		}
		alice.fail(proto.OpSend, proto.SendArgs{Chan: gen, Body: "different", Nonce: nonce}, proto.ErrConflict)
		bob.quiet(150 * time.Millisecond)

		// Edit, react, delete.
		bob.fail(proto.OpEdit, proto.EditArgs{Msg: sr.Msg, Body: "hacked"}, proto.ErrForbidden)
		alice.ok(proto.OpEdit, proto.EditArgs{Msg: sr.Msg, Body: "hello again"}, nil)
		bob.event(proto.KMsgEdit)
		bob.ok(proto.OpReact, proto.ReactArgs{Msg: sr.Msg, Emoji: "👍"}, nil)
		var noop proto.SeqRes
		bob.ok(proto.OpReact, proto.ReactArgs{Msg: sr.Msg, Emoji: "👍"}, &noop)
		if noop.Seq != 0 {
			t.Fatal("duplicate react produced an event")
		}
		alice.event(proto.KReactAdd)

		var hist proto.HistoryRes
		bob.ok(proto.OpHistory, proto.HistoryArgs{Chan: gen}, &hist)
		if len(hist.Msgs) != 1 || hist.Msgs[0].Body != "hello again" || len(hist.Msgs[0].Reactions["👍"]) != 1 {
			t.Fatalf("history: %+v", hist)
		}
		var found proto.MsgsRes
		bob.ok(proto.OpSearch, proto.SearchArgs{Q: "agai"}, &found)
		if len(found.Msgs) != 1 {
			t.Fatalf("search: %+v", found)
		}

		alice.ok(proto.OpDelete, proto.MsgArgs{Msg: sr.Msg}, nil)
		bob.event(proto.KMsgDelete)
		alice.ok(proto.OpDelete, proto.MsgArgs{Msg: sr.Msg}, nil) // set semantics
		bob.fail(proto.OpReact, proto.ReactArgs{Msg: sr.Msg, Emoji: "x"}, proto.ErrNotFound)
	})
}

func TestValidation(t *testing.T) {
	ts := startServer(t, "tcp")
	a := ts.register(t, "alice", true)
	snap, _ := a.syncFresh()
	gen := generalID(t, snap)
	nonce := make([]byte, 16)
	for _, body := range []string{"\x1b[2J", "bell\a", "", "   ", "a\rb", string([]byte{0xff})} {
		rand.Read(nonce)
		a.fail(proto.OpSend, proto.SendArgs{Chan: gen, Body: body, Nonce: nonce}, proto.ErrBadRequest)
	}
	rand.Read(nonce)
	a.fail(proto.OpSend, proto.SendArgs{Chan: gen, Body: strings.Repeat("x", proto.MaxBody+1), Nonce: nonce}, proto.ErrTooLarge)
	a.fail(proto.OpSend, proto.SendArgs{Chan: gen, Body: "x", Nonce: []byte{1}}, proto.ErrBadRequest)
	a.fail(proto.OpChanCreate, proto.ChanCreateArgs{Name: "Bad Name"}, proto.ErrBadRequest)
	a.fail(proto.OpChanTopic, proto.ChanTopicArgs{Chan: gen, Topic: "evil‮"}, proto.ErrBadRequest)
	a.fail("nope", proto.Empty{}, proto.ErrUnknownOp)
}

func TestSyncMustBeFirst(t *testing.T) {
	ts := startServer(t, "tcp")
	a := ts.register(t, "alice", true)
	a.fail(proto.OpUsersList, proto.Empty{}, proto.ErrBadRequest)
	a.syncFresh()
	a.fail(proto.OpSync, proto.SyncArgs{}, proto.ErrBadRequest)
}

func TestPrivateChannelVisibility(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	bob := ts.register(t, "bob", false)
	alice.syncFresh()
	bob.syncFresh()

	var cr proto.ChanRes
	alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "secret", Private: true}, &cr)
	sec := cr.Chan.ID
	alice.send(sec, "before bob joined")
	bob.quiet(150 * time.Millisecond)
	bob.fail(proto.OpHistory, proto.HistoryArgs{Chan: sec}, proto.ErrForbidden)
	bob.fail(proto.OpChanJoin, proto.ChanArgs{Chan: sec}, proto.ErrForbidden)
	bob.fail(proto.OpSend, proto.SendArgs{Chan: sec, Body: "x", Nonce: make([]byte, 16)}, proto.ErrForbidden)
	var found proto.MsgsRes
	bob.ok(proto.OpSearch, proto.SearchArgs{Q: "joined"}, &found)
	if len(found.Msgs) != 0 {
		t.Fatal("search leaked private message")
	}

	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: sec, User: bob.user.ID}, nil)
	j := bob.event(proto.KChanJoin)
	var cj proto.ChanJoin
	proto.Unmarshal(j.Data, &cj)
	if cj.Chan.ID != sec || cj.Chan.Name != "secret" || len(cj.Chan.Members) != 2 {
		t.Fatalf("join event: %+v", cj)
	}
	// Member now: history works (backlog via history, PROTOCOL.md §6).
	var hist proto.HistoryRes
	bob.ok(proto.OpHistory, proto.HistoryArgs{Chan: sec}, &hist)
	if len(hist.Msgs) != 1 {
		t.Fatalf("history after join: %+v", hist)
	}

	// Replay from 0..: pre-join msg.new must not be in bob's event replay.
	c2, _ := ts.connect(t, bob.signer, "", "")
	c2.nextID++
	c2.codec.Write(proto.TReq, proto.Req{ID: c2.nextID, Op: proto.OpSync,
		Args: proto.MustRaw(proto.SyncArgs{Since: 1, ServerID: ts.st.serverID})})
	for {
		f := c2.next(proto.TEvent)
		_ = f
		var ev proto.Event
		f.Decode(&ev)
		if ev.Kind == proto.KMsgNew && ev.Chan == sec {
			t.Fatal("replay leaked a private message from before bob joined")
		}
		if ev.Kind == proto.KChanJoin && ev.Chan == sec {
			break
		}
	}

	// Leave: no more events.
	bob.ok(proto.OpChanLeave, proto.ChanArgs{Chan: sec}, nil)
	bob.event(proto.KChanLeave)
	alice.event(proto.KChanLeave)
	alice.send(sec, "after bob left")
	bob.quiet(150 * time.Millisecond)
}

func TestReconnectReplay(t *testing.T) {
	forEachTransport(t, func(t *testing.T, ts *testServer) {
		alice := ts.register(t, "alice", true)
		bob := ts.register(t, "bob", false)
		snap, _ := alice.syncFresh()
		_, sr := bob.syncFresh()
		gen := generalID(t, snap)
		bob.conn.Close()

		for _, b := range []string{"one", "two", "three"} {
			alice.send(gen, b)
		}
		bob2, _ := ts.connect(t, bob.signer, "", "")
		bob2.nextID++
		bob2.codec.Write(proto.TReq, proto.Req{ID: bob2.nextID, Op: proto.OpSync,
			Args: proto.MustRaw(proto.SyncArgs{Since: sr.Head, ServerID: ts.st.serverID})})
		var bodies []string
		for len(bodies) < 3 {
			ev := bob2.event(proto.KMsgNew)
			var m proto.Message
			proto.Unmarshal(ev.Data, &m)
			bodies = append(bodies, m.Body)
		}
		if strings.Join(bodies, ",") != "one,two,three" {
			t.Fatalf("replay order: %v", bodies)
		}
		r := bob2.res(bob2.nextID)
		var res proto.SyncRes
		proto.Unmarshal(r.Data, &res)
		if !r.OK || res.Reset {
			t.Fatalf("replay sync res: %+v %+v", r, res)
		}
		// Live after replay.
		alice.send(gen, "live")
		bob2.event(proto.KMsgNew)
	})
}

func TestAuthFailures(t *testing.T) {
	ts := startServer(t, "tcp")
	code, _ := ts.CreateInvite(context.Background(), true, 0)
	inv, _ := proto.ParseInvite(code)

	if _, bye := ts.connect(t, newKey(t), "", ""); bye != proto.ByeAuthFailed {
		t.Fatalf("unknown key: %q", bye)
	}
	key := newKey(t)
	if _, bye := ts.connect(t, key, inv.Token, "Bad Name"); bye != proto.ByeProtocolError {
		t.Fatalf("bad name: %q", bye)
	}
	if _, bye := ts.connect(t, key, inv.Token, "alice"); bye != "" {
		t.Fatalf("register: %q", bye)
	}
	if _, bye := ts.connect(t, newKey(t), inv.Token, "eve"); bye != proto.ByeInviteInvalid {
		t.Fatalf("reused invite: %q", bye)
	}
	code2, _ := ts.CreateInvite(context.Background(), false, 0)
	inv2, _ := proto.ParseInvite(code2)
	if _, bye := ts.connect(t, newKey(t), inv2.Token, "alice"); bye != proto.ByeNameTaken {
		t.Fatalf("name taken: %q", bye)
	}
	if _, bye := ts.connect(t, key, "", ""); bye != "" {
		t.Fatalf("login: %q", bye)
	}

	// A signature made for another TLS session is rejected.
	conn, codec := ts.dial(t)
	sig, _ := identity.Sign(key, make([]byte, 32))
	codec.Write(proto.THello, proto.Hello{Auth: proto.Auth{Pubkey: key.PublicKey().Marshal(), Sig: sig}})
	f, err := codec.Read()
	var b proto.Bye
	if err != nil || f.Type != proto.TBye || f.Decode(&b) != nil || b.Code != proto.ByeAuthFailed {
		t.Fatalf("replayed sig: %v %#x %+v", err, f.Type, b)
	}
	_ = conn
}

func TestDisableRevokes(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	bob := ts.register(t, "bob", false)
	alice.syncFresh()
	bob.syncFresh()
	bob.fail(proto.OpUserDisable, proto.UserArgs{User: alice.user.ID}, proto.ErrForbidden)
	alice.ok(proto.OpUserDisable, proto.UserArgs{User: bob.user.ID}, nil)
	for f := range bob.frames {
		if f.Type == proto.TBye {
			var b proto.Bye
			f.Decode(&b)
			if b.Code != proto.ByeRevoked {
				t.Fatalf("bye %q", b.Code)
			}
			break
		}
	}
	if _, bye := ts.connect(t, bob.signer, "", ""); bye != proto.ByeRevoked {
		t.Fatalf("disabled login: %q", bye)
	}
}

func TestDMOpen(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	bob := ts.register(t, "bob", false)
	carol := ts.register(t, "carol", false)
	alice.syncFresh()
	bob.syncFresh()
	carol.syncFresh()
	var a, b proto.ChanRes
	alice.ok(proto.OpDMOpen, proto.DMOpenArgs{Users: []uint64{bob.user.ID}}, &a)
	bob.ok(proto.OpDMOpen, proto.DMOpenArgs{Users: []uint64{alice.user.ID}}, &b)
	if a.Chan.ID != b.Chan.ID || a.Chan.Kind != proto.ChanDM {
		t.Fatalf("dm not reused: %+v %+v", a, b)
	}
	alice.send(a.Chan.ID, "psst")
	bob.event(proto.KMsgNew)
	carol.quiet(150 * time.Millisecond)
	bob.fail(proto.OpChanLeave, proto.ChanArgs{Chan: a.Chan.ID}, proto.ErrForbidden)
	// A DM with only yourself is your private notes; nobody else sees it.
	var notes proto.ChanRes
	alice.ok(proto.OpDMOpen, proto.DMOpenArgs{Users: []uint64{alice.user.ID}}, &notes)
	if len(notes.Chan.Members) != 1 || notes.Chan.Kind != proto.ChanDM {
		t.Fatalf("notes: %+v", notes.Chan)
	}
	alice.send(notes.Chan.ID, "note to self")
	bob.quiet(150 * time.Millisecond)
	tooMany := make([]uint64, proto.MaxDMOthers+1)
	for i := range tooMany {
		tooMany[i] = uint64(100 + i)
	}
	alice.fail(proto.OpDMOpen, proto.DMOpenArgs{Users: tooMany}, proto.ErrBadRequest)
}

// syncFreshAgain reconnects c's key and does a fresh sync.
func (c *tclient) syncFreshAgain(t *testing.T, ts *testServer) (proto.Snapshot, proto.SyncRes) {
	t.Helper()
	c2, bye := ts.connect(t, c.signer, "", "")
	if bye != "" {
		t.Fatal(bye)
	}
	return c2.syncFresh()
}

// replayFrom reconnects c's key, syncs from since and returns every EVENT
// replayed before the sync RES.
func (c *tclient) replayFrom(t *testing.T, ts *testServer, since uint64) []proto.Frame {
	t.Helper()
	c2, bye := ts.connect(t, c.signer, "", "")
	if bye != "" {
		t.Fatal(bye)
	}
	c2.nextID++
	c2.codec.Write(proto.TReq, proto.Req{ID: c2.nextID, Op: proto.OpSync,
		Args: proto.MustRaw(proto.SyncArgs{Since: since, ServerID: ts.st.serverID})})
	var evs []proto.Frame
	for {
		f := c2.recv("replay")
		switch f.Type {
		case proto.TEvent:
			evs = append(evs, f)
		case proto.TSnapshot:
			t.Fatal("got a snapshot, want a replay")
		case proto.TRes:
			var r proto.Res
			f.Decode(&r)
			if !r.OK {
				t.Fatalf("replay sync: %v", r.Err)
			}
			return evs
		}
	}
}

// assertScrubbed fails if any replayed frame carries one of gone, either in
// its raw bytes or in a decoded message body.
func assertScrubbed(t *testing.T, frames []proto.Frame, gone ...string) {
	t.Helper()
	for _, f := range frames {
		var ev proto.Event
		if err := f.Decode(&ev); err != nil {
			t.Fatal(err)
		}
		var body string
		switch ev.Kind {
		case proto.KMsgNew:
			var m proto.Message
			proto.Unmarshal(ev.Data, &m)
			body = m.Body
		case proto.KMsgEdit:
			var d proto.MsgEdit
			proto.Unmarshal(ev.Data, &d)
			body = d.Body
		}
		for _, s := range gone {
			if strings.Contains(string(f.Payload), s) || strings.Contains(body, s) {
				t.Errorf("replayed %s seq %d still contains %q", ev.Kind, ev.Seq, s)
			}
		}
	}
}

func searchCount(c *tclient, q string) int {
	var r proto.MsgsRes
	c.ok(proto.OpSearch, proto.SearchArgs{Q: q}, &r)
	return len(r.Msgs)
}

func TestDeletedTextNotReplayed(t *testing.T) {
	forEachTransport(t, func(t *testing.T, ts *testServer) {
		alice := ts.register(t, "alice", false)
		bob := ts.register(t, "bob", false)
		snap, _ := alice.syncFresh()
		_, before := bob.syncFresh()
		gen := generalID(t, snap)

		id := alice.send(gen, "secret").Msg
		bob.event(proto.KMsgNew)
		if n := searchCount(alice, "secret"); n != 1 {
			t.Fatalf("search before delete = %d", n)
		}
		// An edit before the delete must be scrubbed as well.
		alice.ok(proto.OpEdit, proto.EditArgs{Msg: id, Body: "secret v2"}, nil)
		alice.ok(proto.OpDelete, proto.MsgArgs{Msg: id}, nil)

		frames := bob.replayFrom(t, ts, before.Head)
		assertScrubbed(t, frames, "secret")
		var sawNew, sawDelete bool
		for _, f := range frames {
			var ev proto.Event
			f.Decode(&ev)
			switch ev.Kind {
			case proto.KMsgNew:
				var m proto.Message
				proto.Unmarshal(ev.Data, &m)
				if m.ID == id {
					sawNew = true
					if !m.Deleted || m.Body != "" || m.Chan != gen || m.Author != alice.user.ID {
						t.Errorf("replayed msg.new = %+v", m)
					}
				}
			case proto.KMsgDelete:
				sawDelete = true
			}
		}
		if !sawNew || !sawDelete {
			t.Errorf("replay msg.new=%v msg.delete=%v", sawNew, sawDelete)
		}
		if n := searchCount(alice, "secret"); n != 0 {
			t.Errorf("search after delete = %d", n)
		}
		if n := searchCount(bob, "secret"); n != 0 {
			t.Errorf("bob's search after delete = %d", n)
		}
	})
}

func TestEditedTextNotReplayed(t *testing.T) {
	forEachTransport(t, func(t *testing.T, ts *testServer) {
		alice := ts.register(t, "alice", false)
		bob := ts.register(t, "bob", false)
		snap, _ := alice.syncFresh()
		_, before := bob.syncFresh()
		gen := generalID(t, snap)

		id := alice.send(gen, "secret alpha").Msg
		alice.ok(proto.OpEdit, proto.EditArgs{Msg: id, Body: "secret bravo"}, nil)
		alice.ok(proto.OpEdit, proto.EditArgs{Msg: id, Body: "final charlie"}, nil)

		frames := bob.replayFrom(t, ts, before.Head)
		assertScrubbed(t, frames, "secret", "alpha", "bravo")
		var got []string
		for _, f := range frames {
			var ev proto.Event
			f.Decode(&ev)
			switch ev.Kind {
			case proto.KMsgNew:
				var m proto.Message
				proto.Unmarshal(ev.Data, &m)
				if m.ID == id {
					if m.Edited == 0 || m.Deleted {
						t.Errorf("replayed msg.new = %+v", m)
					}
					got = append(got, "new:"+m.Body)
				}
			case proto.KMsgEdit:
				var d proto.MsgEdit
				proto.Unmarshal(ev.Data, &d)
				got = append(got, "edit:"+d.Body)
			}
		}
		if s := strings.Join(got, ","); s != "new:final charlie,edit:,edit:final charlie" {
			t.Errorf("replay = %q", s)
		}
		if n := searchCount(alice, "alpha"); n != 0 {
			t.Errorf("search old text = %d", n)
		}
		if n := searchCount(alice, "charlie"); n != 1 {
			t.Errorf("search new text = %d", n)
		}
	})
}
