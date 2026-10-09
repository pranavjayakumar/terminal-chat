package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// startHookServer is startServer (TCP) with the HTTP side enabled.
func startHookServer(t *testing.T) *testServer {
	t.Helper()
	srv, err := New(Config{DataDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.ListenWith("127.0.0.1:0", srv.Cert(), transport.ListenOpts{HTTP: true, HTTPCert: srv.HTTPCert()})
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
	return &testServer{Server: srv, addr: ln.Addr().String(), kind: "tcp", stop: stop}
}

type hookResp struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (ts *testServer) post(t *testing.T, method, url, ctype, body string, hdr ...string) hookResp {
	t.Helper()
	cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: transport.HTTPSConfig(ts.HTTPPin())}}
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := hookResp{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	json.Unmarshal(raw, &r.body)
	return r
}

func (c *tclient) createHook(ch uint64, name string) proto.HookCreateRes {
	c.t.Helper()
	var r proto.HookCreateRes
	c.ok(proto.OpHookCreate, proto.HookCreateArgs{Chan: ch, Name: name}, &r)
	if r.ID == 0 || !strings.Contains(r.URL, proto.HookPath) || !strings.Contains(r.Curl, r.URL) {
		c.t.Fatalf("bad hook.create result %+v", r)
	}
	return r
}

func (c *tclient) msgNew() proto.Message {
	c.t.Helper()
	ev := c.event(proto.KMsgNew)
	var m proto.Message
	if err := proto.Unmarshal(ev.Data, &m); err != nil {
		c.t.Fatal(err)
	}
	return m
}

// noMsgIn drains frames for d and fails on any event about channel ch.
func (c *tclient) noMsgIn(ch uint64, d time.Duration) {
	c.t.Helper()
	check := func(f proto.Frame) {
		var ev proto.Event
		if f.Type == proto.TEvent && f.Decode(&ev) == nil && ev.Chan == ch {
			c.t.Fatalf("%s: got %s in channel %d", c.user.Name, ev.Kind, ch)
		}
	}
	for _, f := range c.held {
		check(f)
	}
	c.held = nil
	deadline := time.After(d)
	for {
		select {
		case f := <-c.frames:
			check(f)
		case <-deadline:
			return
		}
	}
}

func TestHookPost(t *testing.T) {
	ts := startHookServer(t)
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	gen := generalID(t, snap)

	h := alice.createHook(gen, "ci")
	ev := bob.event(proto.KUserNew)
	var bot proto.User
	proto.Unmarshal(ev.Data, &bot)
	if !bot.Bot || bot.Name != "ci" {
		t.Fatalf("bot user %+v", bot)
	}
	bob.event(proto.KChanJoin)

	// JSON body, ANSI stripped.
	r := ts.post(t, "POST", h.URL, "application/json", `{"text":"\u001b[31mbuild failed\u001b[0m\n"}`)
	if r.status != 200 || r.body["msg"] == nil {
		t.Fatalf("POST: %d %s", r.status, r.raw)
	}
	m := bob.msgNew()
	if m.Author != bot.ID || m.Body != "build failed" || m.Chan != gen {
		t.Fatalf("msg %+v", m)
	}

	// Raw body.
	r = ts.post(t, "POST", h.URL, "text/plain", "deployed v1.2\n")
	if r.status != 200 {
		t.Fatalf("raw POST: %d %s", r.status, r.raw)
	}
	if m := bob.msgNew(); m.Body != "deployed v1.2" || m.Author != bot.ID {
		t.Fatalf("raw msg %+v", m)
	}

	// Invalid bodies.
	for _, c := range []struct{ ctype, body string }{
		{"application/json", `{"txt":"x"}`},
		{"application/json", `not json`},
		{"text/plain", "bad \xff utf8"},
		{"text/plain", " \n\x1b[0m"},
	} {
		if r := ts.post(t, "POST", h.URL, c.ctype, c.body); r.status != 400 || r.body["error"] != "bad_request" {
			t.Fatalf("%q: %d %s", c.body, r.status, r.raw)
		}
	}
	// Too large: over the read cap, and over the body limit after reading.
	if r := ts.post(t, "POST", h.URL, "text/plain", strings.Repeat("x", 3*proto.MaxBody)); r.status != 413 {
		t.Fatalf("huge: %d %s", r.status, r.raw)
	}
	if r := ts.post(t, "POST", h.URL, "text/plain", strings.Repeat("x", proto.MaxBody+1)); r.status != 413 {
		t.Fatalf("oversized: %d %s", r.status, r.raw)
	}

	// Idempotency-Key dedupes.
	a := ts.post(t, "POST", h.URL, "text/plain", "once", "Idempotency-Key", "run-42")
	b := ts.post(t, "POST", h.URL, "text/plain", "once", "Idempotency-Key", "run-42")
	if a.status != 200 || b.status != 200 || a.body["msg"] != b.body["msg"] {
		t.Fatalf("idempotency: %s / %s", a.raw, b.raw)
	}
	if c := ts.post(t, "POST", h.URL, "text/plain", "different", "Idempotency-Key", "run-42"); c.status != 409 {
		t.Fatalf("reused key: %d %s", c.status, c.raw)
	}
	if m := bob.msgNew(); m.Body != "once" {
		t.Fatalf("msg %+v", m)
	}
	bob.quiet(200 * time.Millisecond)
}

func TestHook404s(t *testing.T) {
	ts := startHookServer(t)
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	h := alice.createHook(generalID(t, snap), "ci")

	base := "https://" + ts.addr
	unknown := ts.post(t, "POST", base+"/hooks/aaaaaaaaaaaaaaaaaaaaaaaaaa", "text/plain", "x")
	for _, r := range []hookResp{
		ts.post(t, "GET", h.URL, "", ""),
		ts.post(t, "PUT", h.URL, "text/plain", "x"),
		ts.post(t, "POST", base+"/", "text/plain", "x"),
		ts.post(t, "POST", base+"/hooks/", "text/plain", "x"),
		ts.post(t, "POST", base+"/hooks/../x", "text/plain", "x"),
		ts.post(t, "POST", h.URL+"x", "text/plain", "x"),
	} {
		if r.status != 404 || r.raw != unknown.raw {
			t.Fatalf("got %d %s", r.status, r.raw)
		}
	}
	alice.ok(proto.OpHookDelete, proto.HookArgs{ID: h.ID}, nil)
	deleted := ts.post(t, "POST", h.URL, "text/plain", "x")
	if unknown.status != 404 || deleted.status != 404 || deleted.raw != unknown.raw {
		t.Fatalf("unknown %d %q vs deleted %d %q", unknown.status, unknown.raw, deleted.status, deleted.raw)
	}
}

func TestHookRateLimit(t *testing.T) {
	ts := startHookServer(t)
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	h := alice.createHook(generalID(t, snap), "ci")
	for i := range hookBurst {
		if r := ts.post(t, "POST", h.URL, "text/plain", "m"); r.status != 200 {
			t.Fatalf("post %d: %d %s", i, r.status, r.raw)
		}
	}
	r := ts.post(t, "POST", h.URL, "text/plain", "m")
	if r.status != 429 || r.header.Get("Retry-After") == "" || r.body["error"] != "rate_limited" {
		t.Fatalf("over limit: %d %v %s", r.status, r.header, r.raw)
	}
	// Another hook has its own bucket.
	h2 := alice.createHook(generalID(t, snap), "deploy")
	if r := ts.post(t, "POST", h2.URL, "text/plain", "m"); r.status != 200 {
		t.Fatalf("second hook: %d %s", r.status, r.raw)
	}
}

func TestHookOpsPermissions(t *testing.T) {
	ts := startHookServer(t)
	admin := ts.register(t, "root", true)
	snap, _ := admin.syncFresh()
	alice := ts.register(t, "alice", false)
	alice.syncFresh()
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	gen := generalID(t, snap)

	var priv proto.ChanRes
	alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "secret", Private: true}, &priv)
	ph := alice.createHook(priv.Chan.ID, "secret-bot")
	gh := alice.createHook(gen, "ci")

	// Bot reused for the same name; a person's name conflicts; bad names, DMs, non-members rejected.
	if again := alice.createHook(gen, "ci"); again.ID == gh.ID || again.URL == gh.URL {
		t.Fatal("hook not distinct")
	}
	alice.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: gen, Name: "bob"}, proto.ErrConflict)
	alice.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: gen, Name: "Bad Name"}, proto.ErrBadRequest)
	bob.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: priv.Chan.ID, Name: "x"}, proto.ErrForbidden)
	alice.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: 9999, Name: "x"}, proto.ErrNotFound)
	var dm proto.ChanRes
	alice.ok(proto.OpDMOpen, proto.DMOpenArgs{Users: []uint64{bob.user.ID}}, &dm)
	alice.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: dm.Chan.ID, Name: "x"}, proto.ErrBadRequest)

	list := func(c *tclient) map[uint64]bool {
		var r proto.HooksRes
		c.ok(proto.OpHookList, proto.Empty{}, &r)
		out := map[uint64]bool{}
		for _, h := range r.Hooks {
			out[h.ID] = true
		}
		return out
	}
	if l := list(bob); l[ph.ID] || !l[gh.ID] {
		t.Fatalf("bob sees %v", l)
	}
	if l := list(alice); !l[ph.ID] || !l[gh.ID] {
		t.Fatalf("alice sees %v", l)
	}
	if l := list(admin); !l[ph.ID] || !l[gh.ID] {
		t.Fatalf("admin sees %v", l)
	}

	// The private hook's bot posts only to members.
	if r := ts.post(t, "POST", ph.URL, "text/plain", "psst"); r.status != 200 {
		t.Fatalf("private post: %d %s", r.status, r.raw)
	}
	bob.noMsgIn(priv.Chan.ID, 300*time.Millisecond)

	// Delete: an outsider gets not_found; any member of the channel (not just
	// the creator) and admins may.
	bob.fail(proto.OpHookDelete, proto.HookArgs{ID: ph.ID}, proto.ErrNotFound)
	bob.ok(proto.OpHookDelete, proto.HookArgs{ID: gh.ID}, nil)
	admin.ok(proto.OpHookDelete, proto.HookArgs{ID: ph.ID}, nil)
	alice.fail(proto.OpHookDelete, proto.HookArgs{ID: gh.ID}, proto.ErrNotFound)
}

// TestHookRevocation: a hook stops posting once its creator loses access,
// and the remaining members can revoke it (H1).
func TestHookRevocation(t *testing.T) {
	ts := startHookServer(t)
	admin := ts.register(t, "root", true)
	admin.syncFresh()
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	gen := generalID(t, snap)

	var inc proto.ChanRes
	alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "incident", Private: true}, &inc)
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: inc.Chan.ID, User: bob.user.ID}, nil)
	ih := alice.createHook(inc.Chan.ID, "pager")
	gh := alice.createHook(gen, "ci")
	if r := ts.post(t, "POST", ih.URL, "text/plain", "up"); r.status != 200 {
		t.Fatalf("before leave: %d %s", r.status, r.raw)
	}
	notFound := ts.post(t, "POST", "https://"+ts.addr+"/hooks/aaaaaaaaaaaaaaaaaaaaaaaaaa", "text/plain", "x")

	// The creator leaves: the hook goes dormant (same 404 as unknown) and
	// a remaining member can delete it.
	alice.ok(proto.OpChanLeave, proto.ChanArgs{Chan: inc.Chan.ID}, nil)
	if r := ts.post(t, "POST", ih.URL, "text/plain", "still here"); r.status != 404 || r.raw != notFound.raw {
		t.Fatalf("after creator left: %d %s", r.status, r.raw)
	}
	bob.ok(proto.OpHookDelete, proto.HookArgs{ID: ih.ID}, nil)

	// The creator is disabled: their hooks are deleted at once.
	if r := ts.post(t, "POST", gh.URL, "text/plain", "ok"); r.status != 200 {
		t.Fatalf("before disable: %d %s", r.status, r.raw)
	}
	admin.ok(proto.OpUserDisable, proto.UserArgs{User: alice.user.ID}, nil)
	if r := ts.post(t, "POST", gh.URL, "text/plain", "zombie"); r.status != 404 || r.raw != notFound.raw {
		t.Fatalf("after disable: %d %s", r.status, r.raw)
	}
	var l proto.HooksRes
	admin.ok(proto.OpHookList, proto.Empty{}, &l)
	if len(l.Hooks) != 0 {
		t.Fatalf("hooks left after disable: %+v", l.Hooks)
	}
}

// TestHookDeletedInFlight: a request that looked its hook up before the
// hook was deleted doesn't post (L6).
func TestHookDeletedInFlight(t *testing.T) {
	ts := startHookServer(t)
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	h := alice.createHook(generalID(t, snap), "ci")
	ctx := context.Background()
	live, err := ts.hookByToken(ctx, h.URL[strings.LastIndex(h.URL, "/")+1:])
	if err != nil {
		t.Fatal(err)
	}
	alice.ok(proto.OpHookDelete, proto.HookArgs{ID: h.ID}, nil)
	_, err = ts.send(ctx, live.bot, proto.SendArgs{Chan: live.ch, Body: "late", Nonce: make([]byte, 16)}, &live)
	if pe, ok := err.(*proto.Error); !ok || pe.Code != proto.ErrNotFound {
		t.Fatalf("send through deleted hook: %v", err)
	}
}

func TestBotCannotLogIn(t *testing.T) {
	ts := startHookServer(t)
	admin := ts.register(t, "root", true)
	snap, _ := admin.syncFresh()
	admin.createHook(generalID(t, snap), "ci")
	var users proto.UsersRes
	admin.ok(proto.OpUsersList, proto.Empty{}, &users)
	var botID uint64
	for _, u := range users.Users {
		if u.Name == "ci" && u.Bot {
			botID = u.ID
		}
	}
	if botID == 0 {
		t.Fatal("bot not listed")
	}
	// No add-key invite for a bot, over the op or a forged one.
	admin.fail(proto.OpInviteCreate, proto.InviteCreateArgs{User: botID}, proto.ErrBadRequest)
	code, err := ts.CreateInvite(context.Background(), false, botID)
	if err != nil {
		t.Fatal(err)
	}
	inv, _ := proto.ParseInvite(code)
	if _, bye := ts.connect(t, newKey(t), inv.Token, ""); bye != proto.ByeInviteInvalid {
		t.Fatalf("bot add-key invite: BYE %q", bye)
	}
	// A new user can't take the bot's name either.
	code, _ = ts.CreateInvite(context.Background(), false, 0)
	inv, _ = proto.ParseInvite(code)
	if _, bye := ts.connect(t, newKey(t), inv.Token, "ci"); bye != proto.ByeNameTaken {
		t.Fatalf("bot name: BYE %q", bye)
	}
}

func TestHookText(t *testing.T) {
	if txt, msg := hookText("application/json; charset=utf-8", []byte(`{"text":"a\tb\r\n"}`)); msg != "" || txt != "a    b" {
		t.Fatalf("%q %q", txt, msg)
	}
	if hookNonce(1, "k")[0] == hookNonce(2, "k")[0] && string(hookNonce(1, "k")) == string(hookNonce(2, "k")) {
		t.Fatal("nonce ignores hook id")
	}
	if !validHookToken(newHookToken()) || validHookToken("AAAAAAAAAAAAAAAAAAAAAAAAAA") {
		t.Fatal("token shape")
	}
}
