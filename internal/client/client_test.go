package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// ---- fake server ----

const (
	alice   = 1
	bob     = 2
	general = 100
	dm      = 101
)

var (
	serverID = bytes.Repeat([]byte{7}, 16)
	users    = []proto.User{{ID: alice, Name: "alice"}, {ID: bob, Name: "bob"}}
)

type fakeServer struct {
	t        *testing.T
	ln       transport.Listener
	pin      string
	conns    chan *fakeConn
	accepted atomic.Int32
	bye      atomic.Value // string: BYE code to answer HELLO with
	noPong   atomic.Bool
	caps     atomic.Value // []string: WELCOME caps
	hello    atomic.Value // proto.Hello: the last login (non-invite) HELLO
	sid      atomic.Value // []byte: WELCOME server_id

	mu   sync.Mutex
	open []transport.Conn
}

type fakeConn struct {
	t     *testing.T
	conn  transport.Conn
	codec *proto.Codec
	in    chan proto.Frame
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	cert, err := transport.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.Listen("127.0.0.1:0", cert)
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeServer{t: t, ln: ln, pin: transport.Pin(cert), conns: make(chan *fakeConn, 16)}
	fs.bye.Store("")
	fs.caps.Store([]string(nil))
	fs.sid.Store(serverID)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			fs.mu.Lock()
			fs.open = append(fs.open, c)
			fs.mu.Unlock()
			go fs.handle(c)
		}
	}()
	t.Cleanup(func() {
		cancel()
		ln.Close()
		<-done
		fs.mu.Lock()
		for _, c := range fs.open {
			c.Close()
		}
		fs.mu.Unlock()
	})
	return fs
}

func (fs *fakeServer) addr() string { return fs.ln.Addr().String() }

func (fs *fakeServer) handle(c transport.Conn) {
	codec := proto.NewCodec(c)
	f, err := codec.Read()
	if err != nil || f.Type != proto.THello {
		c.Close()
		return
	}
	var h proto.Hello
	exp, _ := c.Exporter()
	if f.Decode(&h) != nil {
		c.Close()
		return
	}
	if _, err := identity.Verify(h.Auth.Pubkey, h.Auth.Sig, exp); err != nil {
		codec.Write(proto.TBye, proto.Bye{Code: proto.ByeAuthFailed})
		c.Close()
		return
	}
	if h.Auth.Invite != "" { // login
		if h.Auth.Invite != "tok" {
			codec.Write(proto.TBye, proto.Bye{Code: proto.ByeInviteInvalid})
		} else {
			codec.Write(proto.TWelcome, proto.Welcome{ServerID: fs.sid.Load().([]byte), User: proto.User{ID: alice, Name: h.Auth.Name}})
		}
		linger(c, codec)
		return
	}
	fs.accepted.Add(1)
	fs.hello.Store(h)
	if code := fs.bye.Load().(string); code != "" {
		codec.Write(proto.TBye, proto.Bye{Code: code})
		linger(c, codec)
		return
	}
	codec.Write(proto.TWelcome, proto.Welcome{Server: "fake", Caps: fs.caps.Load().([]string), ServerID: fs.sid.Load().([]byte), User: users[0], Time: 1})
	codec.SetMaxFrame(proto.MaxFrame)
	fc := &fakeConn{t: fs.t, conn: c, codec: codec, in: make(chan proto.Frame, 256)}
	go func() {
		defer close(fc.in)
		for {
			f, err := codec.Read()
			if err != nil {
				return
			}
			if f.Type == proto.TPing && !fs.noPong.Load() {
				codec.WriteRaw(proto.TPong, f.Payload)
			}
			fc.in <- f
		}
	}()
	fs.conns <- fc
}

// linger lets the client read our last frame before closing: closing a QUIC
// connection right away discards unsent stream data.
func linger(c transport.Conn, codec *proto.Codec) {
	t := time.AfterFunc(2*time.Second, func() { c.Close() })
	defer t.Stop()
	for {
		if _, err := codec.Read(); err != nil {
			c.Close()
			return
		}
	}
}

func (fs *fakeServer) accept() *fakeConn {
	fs.t.Helper()
	select {
	case fc := <-fs.conns:
		return fc
	case <-time.After(5 * time.Second):
		fs.t.Fatal("no connection")
		return nil
	}
}

// next returns the next frame of type t, skipping others.
func (fc *fakeConn) next(t proto.FrameType) proto.Frame {
	fc.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-fc.in:
			if !ok {
				fc.t.Fatalf("connection closed waiting for frame %#x", t)
			}
			if f.Type == t {
				return f
			}
		case <-timeout:
			fc.t.Fatalf("timeout waiting for frame %#x", t)
		}
	}
}

func (fc *fakeConn) expectReq(op string, args any) proto.Req {
	fc.t.Helper()
	var r proto.Req
	for {
		if err := fc.next(proto.TReq).Decode(&r); err != nil {
			fc.t.Fatal(err)
		}
		if r.Op == proto.OpRead && op != proto.OpRead {
			continue
		}
		break
	}
	if r.Op != op {
		fc.t.Fatalf("got REQ %s, want %s", r.Op, op)
	}
	if args != nil {
		if err := proto.Unmarshal(r.Args, args); err != nil {
			fc.t.Fatal(err)
		}
	}
	return r
}

func (fc *fakeConn) write(t proto.FrameType, v any) {
	fc.t.Helper()
	if err := fc.codec.Write(t, v); err != nil {
		fc.t.Fatal(err)
	}
}

func (fc *fakeConn) ok(id uint32, data any) {
	fc.write(proto.TRes, proto.Res{ID: id, OK: true, Data: proto.MustRaw(data)})
}

func (fc *fakeConn) fail(id uint32, code string) {
	fc.write(proto.TRes, proto.Res{ID: id, Err: proto.Errf(code, "nope")})
}

func (fc *fakeConn) event(seq uint64, kind string, ch, actor uint64, data any) {
	fc.write(proto.TEvent, proto.Event{Seq: seq, Kind: kind, Chan: ch, Actor: actor, Time: 1, Data: proto.MustRaw(data)})
}

// bootstrap answers the first sync with a reset snapshot at head 20.
func (fc *fakeConn) bootstrap() {
	fc.t.Helper()
	var a proto.SyncArgs
	r := fc.expectReq(proto.OpSync, &a)
	if a.Since != 0 || len(a.ServerID) != 0 {
		fc.t.Fatalf("first sync: since=%d server_id=%x", a.Since, a.ServerID)
	}
	fc.write(proto.TSnapshot, proto.Snapshot{
		Users: users,
		Chans: []proto.Channel{
			{ID: general, Name: "general", Kind: proto.ChanPublic, Members: []uint64{alice, bob}},
			{ID: dm, Kind: proto.ChanDM, Members: []uint64{alice, bob}},
		},
		Read: map[uint64]uint64{general: 10},
	})
	fc.write(proto.TSnapshot, proto.Snapshot{Msgs: []proto.Message{
		{ID: 10, Chan: general, Author: bob, Body: "first"},
		{ID: 11, Chan: general, Author: alice, Body: "mine"},
	}})
	fc.ok(r.ID, proto.SyncRes{Head: 20, Reset: true})
}

// ---- client helpers ----

func writeKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(p, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func testEnv(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
}

// login logs in through the fake server and returns an opened client home.
func login(t *testing.T, fs *fakeServer) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "tc")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u, err := Login(ctx, home, proto.FormatInvite(fs.addr(), "tok", fs.pin), "alice", writeKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != alice || u.Name != "alice" {
		t.Fatalf("login user = %+v", u)
	}
	return home
}

func open(t *testing.T, home string) *Client {
	t.Helper()
	c, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	c.timing.backoffMin = 10 * time.Millisecond
	c.timing.backoffMax = 50 * time.Millisecond
	c.timing.retry = 20 * time.Millisecond
	return c
}

func run(t *testing.T, c *Client) (stopped chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped = make(chan struct{})
	go func() {
		defer close(stopped)
		c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
		c.Close()
	})
	return stopped
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func online(c *Client) func() bool { return func() bool { return c.Status().State == Online } }

func chanView(t *testing.T, c *Client, id uint64) ChannelView {
	t.Helper()
	v, ok := c.Channel(id)
	if !ok {
		t.Fatalf("channel %d not cached", id)
	}
	return v
}

func bodies(ms []Message) string {
	var s []string
	for _, m := range ms {
		b := m.Body
		if m.Pending {
			b += "(pending)"
		}
		if m.Failed != "" {
			b += "(failed)"
		}
		s = append(s, b)
	}
	return strings.Join(s, ",")
}

func lastSeq(t *testing.T, c *Client) uint64 {
	t.Helper()
	v, err := metaUint(c.db, "last_seq")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// ---- tests ----

func TestLoginAndSnapshot(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	home := login(t, fs)
	for _, p := range []string{home, filepath.Join(home, "config.json"), filepath.Join(home, "cache.db")} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %v", p, st.Mode().Perm())
		}
	}

	c := open(t, home)
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	if got := lastSeq(t, c); got != 20 {
		t.Errorf("last_seq = %d, want 20", got)
	}
	if got := bodies(c.Messages(general, 50)); got != "first,mine" {
		t.Errorf("messages = %q", got)
	}
	g := chanView(t, c, general)
	if g.Unread != 0 || g.Mention || !g.Member {
		t.Errorf("general = %+v", g)
	}
	if d := chanView(t, c, dm); d.Display != "bob" {
		t.Errorf("dm display = %q", d.Display)
	}
	if st := c.Status(); st.Transport != "tcp" {
		t.Errorf("status = %+v", st)
	}

	// The cache works offline after a restart.
	c2 := open(t, home)
	defer c2.Close()
	if got := bodies(c2.Messages(general, 50)); got != "first,mine" {
		t.Errorf("reopened messages = %q", got)
	}
	if c2.Me().Name != "alice" || c2.User(bob).Name != "bob" {
		t.Errorf("reopened users: me=%+v bob=%+v", c2.Me(), c2.User(bob))
	}
}

func TestLoginBadInvite(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	// A v0.2.0 server answers invite_invalid to a spent invite even when
	// the key is registered (a join retried after a lost WELCOME), so
	// Login retries as a plain login...
	if u, err := Login(context.Background(), t.TempDir(), proto.FormatInvite(fs.addr(), "bad", fs.pin), "x", writeKey(t)); err != nil || u.ID != users[0].ID {
		t.Fatalf("plain-login fallback: %+v %v", u, err)
	}
	// ...and reports the invite error when that fails too.
	fs.bye.Store(proto.ByeAuthFailed)
	_, err := Login(context.Background(), t.TempDir(), proto.FormatInvite(fs.addr(), "bad", fs.pin), "x", writeKey(t))
	var bye *ByeError
	if !errors.As(err, &bye) || bye.Code != proto.ByeInviteInvalid {
		t.Fatalf("err = %v", err)
	}
	_, err = Login(context.Background(), t.TempDir(), proto.FormatInvite(fs.addr(), "tok", "AAAA"), "x", writeKey(t))
	if !errors.Is(err, transport.ErrPinMismatch) {
		t.Fatalf("wrong pin: err = %v", err)
	}
}

func TestEventsIdempotentAndResume(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	fc.event(25, proto.KMsgNew, general, bob, proto.Message{ID: 25, Chan: general, Author: bob, Body: "hi @alice!"})
	fc.event(25, proto.KMsgNew, general, bob, proto.Message{ID: 25, Chan: general, Author: bob, Body: "dup"})
	fc.event(15, proto.KMsgNew, general, bob, proto.Message{ID: 15, Chan: general, Author: bob, Body: "stale"})
	fc.event(27, proto.KReactAdd, general, bob, proto.Reaction{Msg: 25, Emoji: "+1"})
	fc.event(28, proto.KReactAdd, general, bob, proto.Reaction{Msg: 25, Emoji: "+1"}) // set semantics
	waitFor(t, "seq 28", func() bool { return lastSeq(t, c) == 28 })

	if got := bodies(c.Messages(general, 50)); got != "first,mine,hi @alice!" {
		t.Errorf("messages = %q", got)
	}
	if g := chanView(t, c, general); g.Unread != 1 || !g.Mention {
		t.Errorf("general = %+v", g)
	}
	if r := c.Messages(general, 50)[2].Reactions["+1"]; len(r) != 1 || r[0] != bob {
		t.Errorf("reactions = %v", r)
	}

	// Server drops the connection; the client resumes from last_seq.
	fc.conn.Close()
	fc2 := fs.accept()
	var a proto.SyncArgs
	r := fc2.expectReq(proto.OpSync, &a)
	if a.Since != 28 || !bytes.Equal(a.ServerID, serverID) {
		t.Fatalf("resume sync: since=%d server_id=%x", a.Since, a.ServerID)
	}
	fc2.event(25, proto.KMsgNew, general, bob, proto.Message{ID: 25, Chan: general, Author: bob, Body: "replayed dup"})
	fc2.event(30, proto.KMsgEdit, general, bob, proto.MsgEdit{Msg: 25, Body: "edited", Edited: 5})
	fc2.event(31, proto.KMsgDelete, general, bob, proto.MsgRef{Msg: 10})
	fc2.event(32, proto.KReactRemove, general, bob, proto.Reaction{Msg: 25, Emoji: "+1"})
	fc2.event(33, proto.KUserUpdate, 0, bob, proto.User{ID: bob, Name: "bob", Display: "Bob B"})
	fc2.event(34, proto.KChanNew, 102, bob, proto.Channel{ID: 102, Name: "random", Kind: proto.ChanPrivate, Members: []uint64{bob}})
	fc2.event(35, proto.KChanJoin, 102, bob, proto.ChanJoin{User: alice, Chan: proto.Channel{ID: 102, Name: "random", Kind: proto.ChanPrivate, Members: []uint64{bob, alice}}})
	fc2.event(36, proto.KMsgNew, 102, bob, proto.Message{ID: 36, Chan: 102, Author: bob, Body: "in random"})
	fc2.event(37, proto.KReadUpdate, 0, alice, proto.ReadUpdate{Chan: general, Seq: 25})
	fc2.ok(r.ID, proto.SyncRes{Head: 40})
	waitFor(t, "online", online(c))

	if got := lastSeq(t, c); got != 40 {
		t.Errorf("last_seq = %d, want head 40", got)
	}
	ms := c.Messages(general, 50)
	if got := bodies(ms); got != ",mine,edited" {
		t.Errorf("messages = %q", got)
	}
	if !ms[0].Deleted || ms[2].Edited != 5 || len(ms[2].Reactions) != 0 {
		t.Errorf("messages = %+v", ms)
	}
	if g := chanView(t, c, general); g.Unread != 0 || g.Mention {
		t.Errorf("general after read.update = %+v", g)
	}
	if c.User(bob).Display != "Bob B" {
		t.Errorf("bob = %+v", c.User(bob))
	}
	if rnd := chanView(t, c, 102); !rnd.Member || rnd.Unread != 1 {
		t.Errorf("random = %+v", rnd)
	}

	// Leaving a private channel drops it and its messages.
	fc2.event(45, proto.KChanLeave, 102, alice, proto.UserRef{User: alice})
	waitFor(t, "leave", func() bool { _, ok := c.Channel(102); return !ok })
	if len(c.Messages(102, 10)) != 0 {
		t.Error("messages of left channel still cached")
	}
	// Someone else leaving only updates members.
	fc2.event(46, proto.KChanLeave, general, bob, proto.UserRef{User: bob})
	waitFor(t, "bob left", func() bool { return len(chanView(t, c, general).Members) == 1 })
}

func TestSendDedupLive(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	if err := c.Send(general, "yo", 0); err != nil {
		t.Fatal(err)
	}
	if got := bodies(c.Messages(general, 50)); got != "first,mine,yo(pending)" {
		t.Errorf("before send = %q", got)
	}
	var a proto.SendArgs
	r := fc.expectReq(proto.OpSend, &a)
	if a.Chan != general || a.Body != "yo" || len(a.Nonce) != 16 {
		t.Fatalf("send args = %+v", a)
	}
	// Per spec the EVENT precedes the RES: the pending copy must vanish
	// in the same step the real one appears.
	fc.event(50, proto.KMsgNew, general, alice, proto.Message{ID: 50, Chan: general, Author: alice, Body: "yo"})
	waitFor(t, "event applied", func() bool { return lastSeq(t, c) == 50 })
	if got := bodies(c.Messages(general, 50)); got != "first,mine,yo" {
		t.Errorf("between event and RES = %q", got)
	}
	fc.ok(r.ID, proto.SendRes{Msg: 50})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := bodies(c.Messages(general, 50)); got != "first,mine,yo" {
		t.Errorf("after RES = %q", got)
	}
}

func TestOutboxSurvivesDisconnectSameNonce(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	home := login(t, fs)
	c := open(t, home)

	// Queued while offline, before Run: must survive the reset snapshot.
	if err := c.Send(general, "offline one", 0); err != nil {
		t.Fatal(err)
	}
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	var a1 proto.SendArgs
	fc.expectReq(proto.OpSend, &a1)
	if a1.Body != "offline one" {
		t.Fatalf("send = %+v", a1)
	}
	// Server commits it, but the connection dies before the RES.
	fc.event(50, proto.KMsgNew, general, alice, proto.Message{ID: 50, Chan: general, Author: alice, Body: "offline one"})
	waitFor(t, "event", func() bool { return lastSeq(t, c) == 50 })
	fc.conn.Close()

	fc2 := fs.accept()
	var sa proto.SyncArgs
	r := fc2.expectReq(proto.OpSync, &sa)
	if sa.Since != 50 {
		t.Fatalf("since = %d", sa.Since)
	}
	fc2.ok(r.ID, proto.SyncRes{Head: 50})
	var a2 proto.SendArgs
	r = fc2.expectReq(proto.OpSend, &a2)
	if !bytes.Equal(a1.Nonce, a2.Nonce) || a2.Body != a1.Body || a2.Chan != a1.Chan {
		t.Fatalf("retry differs: %+v vs %+v", a1, a2)
	}
	if got := bodies(c.Messages(general, 50)); got != "first,mine,offline one" {
		t.Errorf("while retrying = %q", got)
	}
	fc2.ok(r.ID, proto.SendRes{Msg: 50}) // idempotent repeat: no new event
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := bodies(c.Messages(general, 50)); got != "first,mine,offline one" {
		t.Errorf("after = %q", got)
	}
}

func TestOutboxOrderAndRestart(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	home := login(t, fs)
	c := open(t, home)
	for _, b := range []string{"a", "b", "c"} {
		if err := c.Send(general, b, 0); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()

	c = open(t, home) // outbox persisted across restart
	if got := bodies(c.Messages(general, 50)); got != "a(pending),b(pending),c(pending)" {
		t.Fatalf("after restart = %q", got)
	}
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	for i, want := range []string{"a", "b", "c"} {
		var a proto.SendArgs
		r := fc.expectReq(proto.OpSend, &a)
		if a.Body != want {
			t.Fatalf("send %d = %q, want %q", i, a.Body, want)
		}
		id := uint64(30 + i)
		fc.event(id, proto.KMsgNew, general, alice, proto.Message{ID: id, Chan: general, Author: alice, Body: want})
		fc.ok(r.ID, proto.SendRes{Msg: id})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := bodies(c.Messages(general, 50)); got != "first,mine,a,b,c" {
		t.Errorf("messages = %q", got)
	}
}

func TestSendPermanentFailure(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	c.Send(general, "nope", 0)
	r := fc.expectReq(proto.OpSend, nil)
	fc.fail(r.ID, proto.ErrRateLimited) // retried
	r = fc.expectReq(proto.OpSend, nil)
	fc.fail(r.ID, proto.ErrForbidden) // given up
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Flush(ctx); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("flush = %v", err)
	}
	ms := c.Messages(general, 50)
	last := ms[len(ms)-1]
	if last.Failed == "" || last.Pending {
		t.Fatalf("last = %+v", last)
	}
	if err := c.DiscardFailed(last.Local); err != nil {
		t.Fatal(err)
	}
	if got := bodies(c.Messages(general, 50)); got != "first,mine" {
		t.Errorf("after discard = %q", got)
	}
}

// B4: a row rejected for good stays listed (Failed) but a later
// SendLocal+FlushLocal reports only on its own message.
func TestFlushLocalIgnoresOlderFailures(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bad, err := c.SendLocal(general, "rejected", 0)
	if err != nil {
		t.Fatal(err)
	}
	fc.fail(fc.expectReq(proto.OpSend, nil).ID, proto.ErrBadRequest)
	if err := c.FlushLocal(ctx, bad); err == nil || !strings.Contains(err.Error(), "bad_request") {
		t.Fatalf("flush own rejected = %v", err)
	}

	good, err := c.SendLocal(general, "fine", 0)
	if err != nil || good == bad {
		t.Fatalf("SendLocal = %d, %v", good, err)
	}
	done := make(chan error, 1)
	go func() { done <- c.FlushLocal(ctx, good) }()
	r := fc.expectReq(proto.OpSend, nil)
	fc.ok(r.ID, proto.SendRes{Msg: 60})
	if err := <-done; err != nil {
		t.Fatalf("flush own delivered = %v (an older failure leaked in)", err)
	}
	if err := c.Flush(ctx); err == nil {
		t.Fatal("Flush (whole outbox) should still report the rejected row")
	}
	ms := c.Messages(general, 50)
	if last := ms[len(ms)-1]; last.Failed == "" {
		t.Fatalf("rejected row no longer shown as failed: %+v", ms)
	}
}

func TestRequestsFailOffline(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	ctx := context.Background()
	if err := c.Edit(ctx, 1, "x"); !errors.Is(err, ErrOffline) {
		t.Fatalf("edit before run = %v", err)
	}
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))
	// React/delete need the message cached (its channel decides how).
	for _, id := range []uint64{10, 99} {
		if err := upsertMsg(c.db, proto.Message{ID: id, Chan: general, Author: bob, Body: "x"}, true); err != nil {
			t.Fatal(err)
		}
	}

	errc := make(chan error, 1)
	go func() { errc <- c.React(ctx, 10, "+1") }()
	fc.expectReq(proto.OpReact, nil)
	fc.conn.Close() // in flight when the connection dies
	select {
	case err := <-errc:
		if !errors.Is(err, ErrOffline) {
			t.Fatalf("react = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending request not failed on disconnect")
	}

	// Correlation: answers out of order reach the right callers.
	fc2 := fs.accept()
	r := fc2.expectReq(proto.OpSync, nil)
	fc2.ok(r.ID, proto.SyncRes{Head: 20})
	waitFor(t, "online", online(c))
	inv, del := make(chan any, 1), make(chan error, 1)
	go func() {
		code, err := c.CreateInvite(ctx, 0)
		if err != nil {
			inv <- err
		} else {
			inv <- code
		}
	}()
	r1 := fc2.expectReq(proto.OpInviteCreate, nil)
	go func() { del <- c.Delete(ctx, 99) }()
	r2 := fc2.expectReq(proto.OpDelete, nil)
	fc2.fail(r2.ID, proto.ErrNotFound)
	fc2.ok(r1.ID, proto.InviteRes{Code: "tc://x"})
	if err := <-del; err == nil || err.(*proto.Error).Code != proto.ErrNotFound {
		t.Errorf("delete = %v", err)
	}
	if got := <-inv; got != "tc://x" {
		t.Errorf("invite = %v", got)
	}
}

func TestMarkRead(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	fc.event(30, proto.KMsgNew, dm, bob, proto.Message{ID: 30, Chan: dm, Author: bob, Body: "psst"})
	fc.event(31, proto.KMsgNew, general, bob, proto.Message{ID: 31, Chan: general, Author: bob, Body: "email bob@alice.com"})
	waitFor(t, "events", func() bool { return lastSeq(t, c) == 31 })
	if d := chanView(t, c, dm); d.Unread != 1 || !d.Mention {
		t.Errorf("dm = %+v", d)
	}
	if g := chanView(t, c, general); g.Unread != 1 || g.Mention {
		t.Errorf("general = %+v", g)
	}
	if err := c.MarkRead(dm, 30); err != nil {
		t.Fatal(err)
	}
	if d := chanView(t, c, dm); d.Unread != 0 || d.Mention {
		t.Errorf("dm after read = %+v", d)
	}
	var ra proto.ReadArgs
	fc.expectReq(proto.OpRead, &ra)
	if ra.Chan != dm || ra.Seq != 30 {
		t.Errorf("read = %+v", ra)
	}
}

func TestMentions(t *testing.T) {
	for body, want := range map[string]bool{
		"@alice":           true,
		"hey @Alice, hi":   true,
		"(@alice)":         true,
		"@alice.":          true,
		"@alicex":          false,
		"@alice-b":         false,
		"bob@alice.com":    false,
		"x @alicey @alice": true,
		"alice":            false,
	} {
		if got := mentions(body, "alice"); got != want {
			t.Errorf("mentions(%q) = %v", body, got)
		}
	}
}

func TestByeRevokedStops(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	stopped := run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	fs.bye.Store(proto.ByeRevoked)
	fc.write(proto.TBye, proto.Bye{Code: proto.ByeSlowConsumer}) // reconnects
	fc.conn.Close()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept going after revoked")
	}
	st := c.Status()
	if !st.Stopped || st.State != Offline || !strings.Contains(st.Err, "revoked") {
		t.Fatalf("status = %+v", st)
	}
	n := fs.accepted.Load()
	time.Sleep(100 * time.Millisecond)
	if fs.accepted.Load() != n || n != 2 {
		t.Fatalf("connections = %d", fs.accepted.Load())
	}
}

func TestKeepaliveDropsDeadConn(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	c.timing.pingIdle = 30 * time.Millisecond
	c.timing.tick = 10 * time.Millisecond
	c.timing.recvTimeout = 300 * time.Millisecond
	fs.noPong.Store(true)
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))
	fc.next(proto.TPing)
	// Silent server: the client must give up on it and reconnect.
	fc2 := fs.accept()
	fc2.expectReq(proto.OpSync, nil)
}

func TestUpdatesCoalesce(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))
	for i := range uint64(5) {
		fc.event(30+i, proto.KMsgNew, general, bob, proto.Message{ID: 30 + i, Chan: general, Author: bob, Body: "x"})
	}
	// The Update for an event is posted just after its transaction commits,
	// so keep draining until all five arrive rather than stopping at the
	// first empty read after last_seq moves.
	var merged Update
	deadline := time.After(5 * time.Second)
	for len(merged.New) < 5 {
		select {
		case u := <-c.Updates():
			merged = merged.merge(u)
		case <-deadline:
			t.Fatalf("merged = %+v", merged)
		}
	}
	if !merged.Msgs[general] || len(merged.New) < 5 {
		t.Fatalf("merged = %+v", merged)
	}
}

func TestQUIC(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "")
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fs.accept().bootstrap()
	waitFor(t, "online", online(c))
	if c.Status().Transport != "quic" {
		t.Errorf("transport = %q", c.Status().Transport)
	}
}

func TestQueuedNotHiddenByOldTwin(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	if err := c.Send(general, "mine", 0); err != nil { // same text as snapshot msg 11
		t.Fatal(err)
	}
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	r := fc.expectReq(proto.OpSend, nil)
	if got := bodies(c.Messages(general, 50)); got != "first,mine,mine(pending)" {
		t.Fatalf("after snapshot = %q", got)
	}
	fc.event(30, proto.KMsgNew, general, alice, proto.Message{ID: 30, Chan: general, Author: alice, Body: "mine"})
	fc.ok(r.ID, proto.SendRes{Msg: 30})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := bodies(c.Messages(general, 50)); got != "first,mine,mine" {
		t.Fatalf("after send = %q", got)
	}
}

func TestCloseStopsEverything(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	done := make(chan struct{})
	go func() { defer close(done); c.Run(context.Background()) }()
	fs.accept().bootstrap()
	waitFor(t, "online", online(c))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run still running after Close")
	}
	for range c.Updates() { // drained, then closed
	}
}

func TestScrubbedEditIgnored(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	// Replay of a superseded edit (body scrubbed), then the latest one.
	fc.event(21, proto.KMsgEdit, general, bob, proto.MsgEdit{Msg: 10, Body: "", Edited: 5})
	waitFor(t, "scrubbed edit", func() bool { return lastSeq(t, c) == 21 })
	if got := bodies(c.Messages(general, 50)); got != "first,mine" {
		t.Errorf("after scrubbed edit = %q", got)
	}
	fc.event(22, proto.KMsgEdit, general, bob, proto.MsgEdit{Msg: 10, Body: "first!", Edited: 6})
	waitFor(t, "edit", func() bool { return lastSeq(t, c) == 22 })
	if got := bodies(c.Messages(general, 50)); got != "first!,mine" {
		t.Errorf("after edit = %q", got)
	}
}

func flush(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func outboxLen(t *testing.T, c *Client) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIdenticalSendsEachOnce(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	for range 2 {
		if err := c.Send(general, "ok", 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := bodies(c.Messages(general, 50)); got != "first,mine,ok(pending),ok(pending)" {
		t.Fatalf("queued = %q", got)
	}
	var nonces [][]byte
	for i, want := range []string{"first,mine,ok,ok(pending)", "first,mine,ok,ok"} {
		var a proto.SendArgs
		r := fc.expectReq(proto.OpSend, &a)
		nonces = append(nonces, a.Nonce)
		id := uint64(50 + i)
		fc.event(id, proto.KMsgNew, general, alice, proto.Message{ID: id, Chan: general, Author: alice, Body: "ok"})
		waitFor(t, "event", func() bool { return lastSeq(t, c) == id })
		if got := bodies(c.Messages(general, 50)); got != want {
			t.Errorf("send %d, between event and RES = %q, want %q", i, got, want)
		}
		fc.ok(r.ID, proto.SendRes{Msg: id})
		waitFor(t, "RES", func() bool { return outboxLen(t, c) == 1-i })
		if got := bodies(c.Messages(general, 50)); got != want {
			t.Errorf("send %d, after RES = %q, want %q", i, got, want)
		}
	}
	if bytes.Equal(nonces[0], nonces[1]) {
		t.Error("identical sends share a nonce")
	}
	flush(t, c)
	if got := bodies(c.Messages(general, 50)); got != "first,mine,ok,ok" {
		t.Errorf("final = %q", got)
	}
}

// The connection drops after the EVENT but before the RES; the retry's RES
// (same nonce, no new event) settles it. The text repeats an older message
// of ours, which must not interfere.
func TestLostResRetriedShowsOnce(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	if err := c.Send(general, "mine", 0); err != nil {
		t.Fatal(err)
	}
	var a1 proto.SendArgs
	fc.expectReq(proto.OpSend, &a1)
	fc.event(50, proto.KMsgNew, general, alice, proto.Message{ID: 50, Chan: general, Author: alice, Body: "mine"})
	waitFor(t, "event", func() bool { return lastSeq(t, c) == 50 })
	fc.conn.Close()
	waitFor(t, "offline", func() bool { return c.Status().State != Online })
	if got := bodies(c.Messages(general, 50)); got != "first,mine,mine" {
		t.Errorf("offline, RES lost = %q", got)
	}

	fc2 := fs.accept()
	r := fc2.expectReq(proto.OpSync, nil)
	fc2.ok(r.ID, proto.SyncRes{Head: 50})
	var a2 proto.SendArgs
	r = fc2.expectReq(proto.OpSend, &a2)
	if !bytes.Equal(a1.Nonce, a2.Nonce) {
		t.Fatal("retry used a new nonce")
	}
	if got := bodies(c.Messages(general, 50)); got != "first,mine,mine" {
		t.Errorf("while retrying = %q", got)
	}
	fc2.ok(r.ID, proto.SendRes{Msg: 50})
	flush(t, c)
	if n := outboxLen(t, c); n != 0 {
		t.Errorf("outbox has %d entries", n)
	}
	if got := bodies(c.Messages(general, 50)); got != "first,mine,mine" {
		t.Errorf("after retry = %q", got)
	}
}

// The connection drops before the EVENT too; sync replays it, and the
// pending copy must already be hidden when the retry goes out.
func TestLostEventReplayedShowsOnce(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	if err := c.Send(general, "hi", 0); err != nil {
		t.Fatal(err)
	}
	fc.expectReq(proto.OpSend, nil)
	fc.conn.Close()

	fc2 := fs.accept()
	r := fc2.expectReq(proto.OpSync, nil)
	fc2.event(50, proto.KMsgNew, general, alice, proto.Message{ID: 50, Chan: general, Author: alice, Body: "hi"})
	fc2.ok(r.ID, proto.SyncRes{Head: 50})
	r = fc2.expectReq(proto.OpSend, nil)
	if got := bodies(c.Messages(general, 50)); got != "first,mine,hi" {
		t.Errorf("after replay = %q", got)
	}
	fc2.ok(r.ID, proto.SendRes{Msg: 50})
	flush(t, c)
	if got := bodies(c.Messages(general, 50)); got != "first,mine,hi" || outboxLen(t, c) != 0 {
		t.Errorf("after retry = %q, outbox %d", got, outboxLen(t, c))
	}
}

// A message of ours from elsewhere can tentatively hide the in-flight entry,
// but a failed RES shows it again.
func TestWrongMatchUndoneByFailedRes(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	if err := c.Send(general, "a", 0); err != nil {
		t.Fatal(err)
	}
	r := fc.expectReq(proto.OpSend, nil)
	fc.event(50, proto.KMsgNew, general, alice, proto.Message{ID: 50, Chan: general, Author: alice, Body: "other device"})
	fc.fail(r.ID, proto.ErrRateLimited)
	waitFor(t, "unhidden", func() bool {
		return bodies(c.Messages(general, 50)) == "first,mine,other device,a(pending)"
	})
	r = fc.expectReq(proto.OpSend, nil)
	fc.event(51, proto.KMsgNew, general, alice, proto.Message{ID: 51, Chan: general, Author: alice, Body: "a"})
	fc.ok(r.ID, proto.SendRes{Msg: 51})
	flush(t, c)
	if got := bodies(c.Messages(general, 50)); got != "first,mine,other device,a" {
		t.Errorf("final = %q", got)
	}
}

// TestBotMessagesNeverMention: a webhook bot's @mention or DM counts as
// unread, never as a mention (L5).
func TestBotMessagesNeverMention(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	const ci = 9
	fc.event(30, proto.KUserNew, 0, alice, proto.User{ID: ci, Name: "ci-bot", Bot: true})
	fc.event(31, proto.KMsgNew, general, ci, proto.Message{ID: 31, Chan: general, Author: ci, Body: "fix-@alice failed"})
	fc.event(32, proto.KMsgNew, dm, ci, proto.Message{ID: 32, Chan: dm, Author: ci, Body: "hi"})
	waitFor(t, "events", func() bool { return lastSeq(t, c) == 32 })
	if g := chanView(t, c, general); g.Unread != 1 || g.Mention {
		t.Errorf("general = %+v", g)
	}
	if d := chanView(t, c, dm); d.Unread != 1 || d.Mention {
		t.Errorf("dm = %+v", d)
	}
	// A person's message still is.
	fc.event(33, proto.KMsgNew, general, bob, proto.Message{ID: 33, Chan: general, Author: bob, Body: "@alice look"})
	waitFor(t, "events", func() bool { return lastSeq(t, c) == 33 })
	if g := chanView(t, c, general); g.Unread != 2 || !g.Mention {
		t.Errorf("general after bob = %+v", g)
	}
}
