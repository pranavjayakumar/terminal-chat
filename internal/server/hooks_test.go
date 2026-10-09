package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// startHookServer is startServer (TCP) with the HTTP side enabled; opts
// adjust the server before it starts serving.
func startHookServer(t *testing.T, opts ...func(*Server)) *testServer {
	t.Helper()
	srv, err := New(Config{DataDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opts {
		o(srv)
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
	tr := &http.Transport{TLSClientConfig: transport.HTTPSConfig(ts.HTTPPin())}
	// Each post uses its own transport; close its connection so tests that
	// post many times don't pile up idle connections against the per-source cap.
	defer tr.CloseIdleConnections()
	cl := &http.Client{Timeout: 5 * time.Second, Transport: tr}
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
	if r.ID == 0 || !strings.Contains(r.URL, proto.HookPath) || !strings.HasPrefix(r.Pin, "sha256//") || !strings.HasSuffix(r.Name, proto.BotSuffix) {
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
	if !bot.Bot || bot.Name != "ci-bot" || h.Name != "ci-bot" {
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

// frozenClock stops token buckets from refilling, so limits are exact.
func frozenClock(s *Server) {
	t0 := time.Now()
	s.cfg.Now = func() time.Time { return t0 }
}

// noIPLimit lifts the per-source limit for tests that post a lot from
// 127.0.0.1.
func noIPLimit(s *Server) { s.hk.ips = newIPLimiter(s.cfg.Now, 1e9, 1e9, 10) }

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

	// Bot reused for the same name; bot names get "-bot" so a person's name
	// is never taken; bad or too long names, DMs, non-members rejected.
	if again := alice.createHook(gen, "ci"); again.ID == gh.ID || again.URL == gh.URL || again.Name != "ci-bot" {
		t.Fatal("hook not distinct")
	}
	if bh := alice.createHook(gen, "bob"); bh.Name != "bob-bot" {
		t.Fatalf("bot name %q", bh.Name)
	}
	if ph.Name != "secret-bot" {
		t.Fatalf("suffix doubled: %q", ph.Name)
	}
	alice.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: gen, Name: "Bad Name"}, proto.ErrBadRequest)
	alice.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: gen, Name: strings.Repeat("x", 29)}, proto.ErrBadRequest)
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
		if u.Name == "ci-bot" && u.Bot {
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
	// People can't take bot names, or any name in the bot namespace, but
	// the hook's own name stays free for a person.
	for _, name := range []string{"ci-bot", "deploy-bot"} {
		code, _ = ts.CreateInvite(context.Background(), false, 0)
		inv, _ = proto.ParseInvite(code)
		if _, bye := ts.connect(t, newKey(t), inv.Token, name); bye != proto.ByeProtocolError {
			t.Fatalf("%s: BYE %q", name, bye)
		}
	}
	ts.register(t, "ci", false)
}

func TestHookText(t *testing.T) {
	if txt, msg := hookText("application/json; charset=utf-8", []byte(`{"text":"a\tb\r\n"}`)); msg != "" || txt != "a    b" {
		t.Fatalf("%q %q", txt, msg)
	}
	// The nonce is SHA-256(SHA-256(token) || key), not tied to a row id.
	a, b := tokenHash("token-a"), tokenHash("token-b")
	inner := sha256.Sum256([]byte("token-a"))
	want := sha256.Sum256(append(inner[:], 'k'))
	if string(hookNonce(a, "k")) != string(want[:16]) || string(hookNonce(a, "k")) == string(hookNonce(b, "k")) {
		t.Fatal("nonce must derive from the token hash and key")
	}
	if !validHookToken(newHookToken()) || validHookToken("AAAAAAAAAAAAAAAAAAAAAAAAAA") {
		t.Fatal("token shape")
	}
}

// TestBotTakeover: a bot is reused only by its owner or an admin (M1).
func TestBotTakeover(t *testing.T) {
	ts := startHookServer(t)
	admin := ts.register(t, "root", true)
	admin.syncFresh()
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	gen := generalID(t, snap)

	alice.createHook(gen, "deploy")
	alice.createHook(gen, "deploy") // the owner may reuse it
	bob.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: gen, Name: "deploy"}, proto.ErrConflict)
	bob.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: gen, Name: "deploy-bot"}, proto.ErrConflict)
	admin.createHook(gen, "deploy")
	if h := bob.createHook(gen, "bobs"); h.Name != "bobs-bot" {
		t.Fatalf("name %q", h.Name)
	}
}

// TestBotOwnerMigration: bots from before owners existed belong to the
// creator of their first hook.
func TestBotOwnerMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "tc.db"))
	if err != nil {
		t.Fatal(err)
	}
	// The pre-owner shape of users and hooks.
	for _, q := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, display TEXT NOT NULL DEFAULT '',
			admin INTEGER NOT NULL DEFAULT 0, disabled INTEGER NOT NULL DEFAULT 0, created INTEGER NOT NULL,
			bot INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE hooks (id INTEGER PRIMARY KEY, token TEXT NOT NULL UNIQUE, chan INTEGER NOT NULL,
			bot INTEGER NOT NULL, name TEXT NOT NULL, created_by INTEGER NOT NULL, created INTEGER NOT NULL)`,
		`INSERT INTO users (id, name, created) VALUES (1, 'alice', 0), (2, 'bob', 0)`,
		`INSERT INTO users (id, name, created, bot) VALUES (3, 'ci', 0, 1), (4, 'lost', 0, 1)`,
		`INSERT INTO hooks (token, chan, bot, name, created_by, created) VALUES ('t1', 1, 3, 'ci', 2, 0), ('t2', 1, 3, 'ci', 1, 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	st, err := openStore(filepath.Join(dir, "tc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	// hooks is rebuilt with AUTOINCREMENT, rows kept.
	var ddl string
	var n int
	st.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'hooks'`).Scan(&ddl)
	st.db.QueryRow(`SELECT COUNT(*) FROM hooks`).Scan(&n)
	if !strings.Contains(ddl, "AUTOINCREMENT") || n != 2 {
		t.Fatalf("hooks after migration: %d rows, %s", n, ddl)
	}
	for id, want := range map[int]int{3: 2, 4: 0} {
		var owner int
		if err := st.db.QueryRow(`SELECT bot_owner FROM users WHERE id = ?`, id).Scan(&owner); err != nil || owner != want {
			t.Fatalf("bot %d owner %d (%v), want %d", id, owner, err, want)
		}
	}
	// A bot without hooks is retired by the migration; one with hooks isn't.
	for id, want := range map[int]bool{3: false, 4: true} {
		var retired bool
		if err := st.db.QueryRow(`SELECT bot_retired FROM users WHERE id = ?`, id).Scan(&retired); err != nil || retired != want {
			t.Fatalf("bot %d retired=%v (%v), want %v", id, retired, err, want)
		}
	}
}

// TestHookCaps: hooks per creator and per channel, and bots per server, are
// capped (M2).
func TestHookCaps(t *testing.T) {
	ts := startHookServer(t)
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	carol := ts.register(t, "carol", false)
	carol.syncFresh()
	gen := generalID(t, snap)
	var other proto.ChanRes
	alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "other"}, &other)

	for range maxHooksPerUser {
		alice.createHook(gen, "a")
	}
	alice.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: other.Chan.ID, Name: "a"}, proto.ErrConflict)
	for range maxHooksPerChan - maxHooksPerUser {
		bob.createHook(gen, "b")
	}
	carol.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: gen, Name: "c"}, proto.ErrConflict)
	carol.ok(proto.OpChanJoin, proto.ChanArgs{Chan: other.Chan.ID}, nil)
	carol.createHook(other.Chan.ID, "c")

	// Server-wide bot cap: fill it up directly, then only existing bots work.
	var bots int
	ts.st.db.QueryRow(`SELECT COUNT(*) FROM users WHERE bot = 1`).Scan(&bots)
	for i := bots; i < maxBots; i++ {
		if _, err := ts.st.db.Exec(`INSERT INTO users (name, created, bot, bot_owner) VALUES (?, 0, 1, 0)`,
			fmt.Sprintf("filler%d-bot", i)); err != nil {
			t.Fatal(err)
		}
	}
	carol.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: other.Chan.ID, Name: "new"}, proto.ErrConflict)
	carol.createHook(other.Chan.ID, "c")
}

// botRow is a bot user's id and retired flag, looked up by name.
func botRow(t *testing.T, ts *testServer, name string) (id uint64, retired bool) {
	t.Helper()
	if err := ts.st.db.QueryRow(`SELECT id, bot_retired FROM users WHERE name = ? AND bot = 1`, name).Scan(&id, &retired); err != nil {
		t.Fatalf("bot %s: %v", name, err)
	}
	return id, retired
}

func activeBots(t *testing.T, ts *testServer) int {
	t.Helper()
	var n int
	if err := ts.st.db.QueryRow(`SELECT COUNT(*) FROM users WHERE bot = 1 AND bot_retired = 0`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestBotRetireReactivate: deleting a bot's last hook retires it (it keeps
// its id and name, and stops counting toward the caps); only its owner or an
// admin can bring it back by creating a hook with that name.
func TestBotRetireReactivate(t *testing.T) {
	ts := startHookServer(t)
	admin := ts.register(t, "root", true)
	admin.syncFresh()
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	gen := generalID(t, snap)

	h1 := alice.createHook(gen, "ci")
	h2 := alice.createHook(gen, "ci")
	id, retired := botRow(t, ts, "ci-bot")
	if retired || activeBots(t, ts) != 1 {
		t.Fatalf("new bot retired=%v, active=%d", retired, activeBots(t, ts))
	}
	if r := ts.post(t, "POST", h1.URL, "text/plain", "hello"); r.status != 200 {
		t.Fatalf("post: %d %s", r.status, r.raw)
	}

	// One hook left: still active.
	alice.ok(proto.OpHookDelete, proto.HookArgs{ID: h1.ID}, nil)
	if _, retired := botRow(t, ts, "ci-bot"); retired {
		t.Fatal("bot retired while it still has a hook")
	}
	// Last hook gone: retired, but the user (and so its old messages) stays.
	alice.ok(proto.OpHookDelete, proto.HookArgs{ID: h2.ID}, nil)
	gotID, retired := botRow(t, ts, "ci-bot")
	if !retired || gotID != id || activeBots(t, ts) != 0 {
		t.Fatalf("after last delete: id %d (was %d), retired=%v, active=%d", gotID, id, retired, activeBots(t, ts))
	}
	if u, err := getUser(context.Background(), ts.st.db, id); err != nil || u.Name != "ci-bot" || !u.Bot {
		t.Fatalf("retired bot user: %+v, %v", u, err)
	}

	// Someone else can't take the name over; the bot stays retired.
	bob.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: gen, Name: "ci"}, proto.ErrConflict)
	if _, retired := botRow(t, ts, "ci-bot"); !retired {
		t.Fatal("bob reactivated alice's bot")
	}
	// An admin can; the bot keeps its id and owner.
	h3 := admin.createHook(gen, "ci")
	gotID, retired = botRow(t, ts, "ci-bot")
	if retired || gotID != id || activeBots(t, ts) != 1 {
		t.Fatalf("after admin reactivation: id %d, retired=%v, active=%d", gotID, retired, activeBots(t, ts))
	}
	var owner uint64
	ts.st.db.QueryRow(`SELECT bot_owner FROM users WHERE id = ?`, id).Scan(&owner)
	if owner != alice.user.ID {
		t.Fatalf("owner %d after admin reactivation, want alice (%d)", owner, alice.user.ID)
	}
	if r := ts.post(t, "POST", h3.URL, "text/plain", "back"); r.status != 200 {
		t.Fatalf("post after reactivation: %d %s", r.status, r.raw)
	}

	// The owner can too.
	admin.ok(proto.OpHookDelete, proto.HookArgs{ID: h3.ID}, nil)
	if _, retired := botRow(t, ts, "ci-bot"); !retired {
		t.Fatal("bot not retired again")
	}
	alice.createHook(gen, "ci")
	if _, retired := botRow(t, ts, "ci-bot"); retired || activeBots(t, ts) != 1 {
		t.Fatalf("owner reactivation: retired=%v active=%d", retired, activeBots(t, ts))
	}
}

// TestBotRetiredWhenOwnerDisabled: disabling a person deletes their hooks,
// which retires the bots left without any.
func TestBotRetiredWhenOwnerDisabled(t *testing.T) {
	ts := startHookServer(t)
	admin := ts.register(t, "root", true)
	admin.syncFresh()
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	alice.createHook(generalID(t, snap), "ci")
	admin.ok(proto.OpUserDisable, proto.UserArgs{User: alice.user.ID}, nil)
	if _, retired := botRow(t, ts, "ci-bot"); !retired || activeBots(t, ts) != 0 {
		t.Fatalf("retired=%v active=%d after owner disabled", retired, activeBots(t, ts))
	}
}

// TestBotCapsCountActiveOnly: 10 active bots per owner and 100 per server;
// retired bots count toward neither, and reactivating one needs a free slot.
func TestBotCapsCountActiveOnly(t *testing.T) {
	ts := startHookServer(t)
	admin := ts.register(t, "root", true)
	admin.syncFresh()
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	gen := generalID(t, snap)
	noWait := func() {
		ts.mu.Lock()
		delete(ts.wlimits, alice.user.ID)
		delete(ts.wlimits, admin.user.ID)
		ts.mu.Unlock()
	}

	hooks := make([]proto.HookCreateRes, maxBotsPerUser)
	for i := range hooks {
		noWait()
		hooks[i] = alice.createHook(gen, fmt.Sprintf("b%d", i))
	}
	// Retire one, take its slot with another, then try to bring it back.
	noWait()
	alice.ok(proto.OpHookDelete, proto.HookArgs{ID: hooks[0].ID}, nil)
	noWait()
	alice.createHook(gen, "extra")
	noWait()
	admin.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: gen, Name: "b0"}, proto.ErrConflict)
	if _, retired := botRow(t, ts, "b0-bot"); !retired {
		t.Fatal("reactivated past the owner's cap")
	}
	noWait()
	alice.ok(proto.OpHookDelete, proto.HookArgs{ID: hooks[1].ID}, nil)
	noWait()
	admin.createHook(gen, "b0")
	if _, retired := botRow(t, ts, "b0-bot"); retired {
		t.Fatal("b0 not reactivated with a free slot")
	}

	// Server-wide: fill with active bots owned by nobody in particular.
	for i := activeBots(t, ts); i < maxBots; i++ {
		if _, err := ts.st.db.Exec(`INSERT INTO users (name, created, bot, bot_owner) VALUES (?, 0, 1, ?)`,
			fmt.Sprintf("filler%d-bot", i), 1000+i); err != nil {
			t.Fatal(err)
		}
	}
	// A pile of retired bots changes nothing.
	for i := range 2 * maxBots {
		if _, err := ts.st.db.Exec(`INSERT INTO users (name, created, bot, bot_owner, bot_retired) VALUES (?, 0, 1, 1, 1)`,
			fmt.Sprintf("old%d-bot", i)); err != nil {
			t.Fatal(err)
		}
	}
	noWait()
	admin.fail(proto.OpHookCreate, proto.HookCreateArgs{Chan: gen, Name: "new"}, proto.ErrConflict)
	if _, err := ts.st.db.Exec(`UPDATE users SET bot_retired = 1 WHERE name = 'filler50-bot'`); err != nil {
		t.Fatal(err)
	}
	noWait()
	admin.createHook(gen, "new")
}

// TestHookSendsChargeCreator: many hooks by one person share that person's
// send budget (M2).
func TestHookSendsChargeCreator(t *testing.T) {
	ts := startHookServer(t, frozenClock, noIPLimit)
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	gen := generalID(t, snap)
	h1, h2 := alice.createHook(gen, "one"), alice.createHook(gen, "two")
	ok := 0
	for i := range 2 * hookBurst {
		h := h1
		if i%2 == 1 {
			h = h2
		}
		switch r := ts.post(t, "POST", h.URL, "text/plain", "m"); r.status {
		case 200:
			ok++
		case 429:
		default:
			t.Fatalf("post %d: %d %s", i, r.status, r.raw)
		}
	}
	if ok != 30 { // alice's send burst
		t.Fatalf("%d hook sends got through, want 30", ok)
	}
}

// TestHookGlobalLimit: all hook sends share a server-wide bucket (M2).
func TestHookGlobalLimit(t *testing.T) {
	ts := startHookServer(t, frozenClock, noIPLimit)
	var hooks []proto.HookCreateRes
	for i := range 3 {
		c := ts.register(t, fmt.Sprintf("u%d", i), false)
		snap, _ := c.syncFresh()
		hooks = append(hooks, c.createHook(generalID(t, snap), "h"+strconv.Itoa(i)))
	}
	// 3 hooks × 17 sends: each hook and creator is under its own limit.
	ok, limited := 0, 0
	for range 17 {
		for _, h := range hooks {
			switch r := ts.post(t, "POST", h.URL, "text/plain", "m"); r.status {
			case 200:
				ok++
			case 429:
				limited++
				if r.header.Get("Retry-After") == "" {
					t.Fatal("no Retry-After")
				}
			default:
				t.Fatalf("%d %s", r.status, r.raw)
			}
		}
	}
	if ok != hookGlobalBurst || limited != 51-hookGlobalBurst {
		t.Fatalf("ok %d limited %d, want %d/%d", ok, limited, hookGlobalBurst, 51-hookGlobalBurst)
	}
}

// TestHookListLimited: hook.list counts as a write op (M2).
func TestHookListLimited(t *testing.T) {
	ts := startHookServer(t, frozenClock, noIPLimit)
	alice := ts.register(t, "alice", false)
	alice.syncFresh()
	for range 60 {
		alice.ok(proto.OpHookList, proto.Empty{}, nil)
	}
	alice.fail(proto.OpHookList, proto.Empty{}, proto.ErrRateLimited)
}

// TestHookIDsNotReused: a deleted hook's id isn't handed out again, and an
// Idempotency-Key used on a deleted hook doesn't collide with a new one (L1).
func TestHookIDsNotReused(t *testing.T) {
	ts := startHookServer(t)
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	gen := generalID(t, snap)
	alice.createHook(gen, "ci")
	h2 := alice.createHook(gen, "ci")
	if r := ts.post(t, "POST", h2.URL, "text/plain", "first", "Idempotency-Key", "k1"); r.status != 200 {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	alice.ok(proto.OpHookDelete, proto.HookArgs{ID: h2.ID}, nil)
	h3 := alice.createHook(gen, "ci") // same bot
	if h3.ID <= h2.ID {
		t.Fatalf("hook id %d reused (had %d)", h3.ID, h2.ID)
	}
	if r := ts.post(t, "POST", h3.URL, "text/plain", "second", "Idempotency-Key", "k1"); r.status != 200 {
		t.Fatalf("new hook, same key: %d %s", r.status, r.raw)
	}
}

// TestHookIPLimit: requests from one source are limited before any token
// lookup, whatever the path (L2).
func TestHookIPLimit(t *testing.T) {
	ts := startHookServer(t, frozenClock)
	bogus := "https://" + ts.addr + "/hooks/aaaaaaaaaaaaaaaaaaaaaaaaaa"
	for i := range ipBurst {
		if r := ts.post(t, "POST", bogus, "text/plain", "x"); r.status != 404 {
			t.Fatalf("request %d: %d %s", i, r.status, r.raw)
		}
	}
	r := ts.post(t, "POST", bogus, "text/plain", "x")
	if r.status != 429 || r.header.Get("Retry-After") == "" {
		t.Fatalf("over the per-IP limit: %d %v %s", r.status, r.header, r.raw)
	}
}

func TestIPLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := newIPLimiter(func() time.Time { return now }, 2, 1, 3)
	for _, a := range []string{"10.0.0.1:1", "10.0.0.1:2"} {
		if ok, _ := l.allow(a); !ok {
			t.Fatal("within burst")
		}
	}
	if ok, wait := l.allow("10.0.0.1:3"); ok || wait != time.Second {
		t.Fatalf("over burst: %v %v", ok, wait)
	}
	// IPv6 sources share a bucket per /64; v4-mapped equals v4.
	l.allow("[2001:db8::1]:1")
	l.allow("[2001:db8::2]:1")
	if ok, _ := l.allow("[2001:db8::3]:1"); ok {
		t.Fatal("same /64 should share a bucket")
	}
	if ok, _ := l.allow("[::ffff:10.0.0.1]:1"); ok {
		t.Fatal("v4-mapped should share the v4 bucket")
	}
	// Bounded: a fourth source evicts the least recently used one.
	l.allow("10.0.0.2:1")
	l.allow("10.0.0.3:1")
	if n := l.len(); n != 3 {
		t.Fatalf("%d entries, want 3", n)
	}
	// Idle entries expire.
	now = now.Add(ipTTL + time.Second)
	l.allow("10.0.0.9:1")
	if n := l.len(); n != 1 {
		t.Fatalf("%d entries after TTL, want 1", n)
	}
}

// TestHTTPConnCap: connections beyond the cap are closed unserved, and the
// idle timeout is short (L2).
func TestHTTPConnCap(t *testing.T) {
	const max = 3
	ts := startHookServer(t, func(s *Server) { s.hk.conns.max = max })
	if hs := ts.newHTTPServer(context.Background()); hs.IdleTimeout > 10*time.Second {
		t.Fatalf("IdleTimeout %v", hs.IdleTimeout)
	}
	dial := func() *tls.Conn {
		c, err := tls.Dial("tcp", ts.addr, transport.HTTPSConfig(ts.HTTPPin()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	for range max {
		dial()
	}
	deadline := time.Now().Add(5 * time.Second)
	for ts.hk.conns.active.Load() < max {
		if time.Now().After(deadline) {
			t.Fatalf("only %d connections open", ts.hk.conns.active.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	extra := dial()
	extra.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("extra connection not closed: %v", err)
	}
}

func TestValidPublicAddr(t *testing.T) {
	for _, a := range []string{"chat.example.com:7443", "10.0.0.1:7443", "[::1]:7443", ":7443", "localhost:1"} {
		if err := ValidPublicAddr(a); err != nil {
			t.Errorf("%q: %v", a, err)
		}
	}
	for _, a := range []string{"x", "host", "h'x:7443", "a b:7443", `h"x:1`, "h:0", "h:99999", "h:07443", "h:port",
		"[zz]:1", "[fe80::1%eth0]:1", "h$(id):1", "h;x:1", "h/x:1"} {
		if err := ValidPublicAddr(a); err == nil {
			t.Errorf("%q accepted", a)
		}
	}
	if _, err := New(Config{DataDir: t.TempDir(), PublicAddr: "evil'host:7443"}); err == nil {
		t.Fatal("New accepted a public address with a quote")
	}
}

// TestHookURLEmptyHost: a public address without a host gives localhost
// URLs, not "https://:7443/...".
func TestHookURLEmptyHost(t *testing.T) {
	ts := startHookServer(t)
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	ts.cfg.PublicAddr = ":7443"
	h := alice.createHook(generalID(t, snap), "ci")
	if !strings.HasPrefix(h.URL, "https://localhost:7443/hooks/") {
		t.Fatalf("url %q", h.URL)
	}
}

// TestBotCapNotOneUsers (verifier, M2): bots are never deleted and the
// per-creator cap counts hooks, not bots, so one member can create and
// delete hooks under fresh names until the server-wide bot cap is used up,
// after which nobody else can make a webhook bot ever again.
func TestBotCapNotOneUsers(t *testing.T) {
	ts := startHookServer(t)
	mallory := ts.register(t, "mallory", false)
	snap, _ := mallory.syncFresh()
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	gen := generalID(t, snap)

	for i := range maxBots {
		// The attacker just waits out the write bucket; skip the wait.
		ts.mu.Lock()
		delete(ts.wlimits, mallory.user.ID)
		ts.mu.Unlock()
		res := mallory.req(proto.OpHookCreate, proto.HookCreateArgs{Chan: gen, Name: fmt.Sprintf("m%d", i)})
		if !res.OK {
			break // refused: a per-person bot cap exists
		}
		var r proto.HookCreateRes
		if err := proto.Unmarshal(res.Data, &r); err != nil {
			t.Fatal(err)
		}
		mallory.ok(proto.OpHookDelete, proto.HookArgs{ID: r.ID}, nil)
		mallory.held = nil
	}
	if res := bob.req(proto.OpHookCreate, proto.HookCreateArgs{Chan: gen, Name: "ci"}); !res.OK {
		t.Fatalf("one member exhausted the server-wide bot cap; bob can't create a bot: %v", res.Err)
	}
}

// fakeConn is a net.Conn with a given remote address, for connCap.
type fakeConn struct {
	net.Conn
	remote net.Addr
	closed bool
}

func (c *fakeConn) RemoteAddr() net.Addr { return c.remote }
func (c *fakeConn) Close() error         { c.closed = true; return nil }

// TestHTTPConnCapPerSource (verifier, L2): the connection cap is global
// only, so a single source holding maxHTTPConns idle connections (each
// lives ~10 s handshake + 10 s header timeout, then reconnect) locks every
// other webhook caller out.
func TestHTTPConnCapPerSource(t *testing.T) {
	c := &connCap{max: maxHTTPConns}
	for i := range maxHTTPConns {
		c.state(&fakeConn{remote: &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 10000 + i}}, http.StateNew)
	}
	other := &fakeConn{remote: &net.TCPAddr{IP: net.ParseIP("198.51.100.1"), Port: 443}}
	c.state(other, http.StateNew)
	if other.closed {
		t.Fatal("one source used up the whole HTTP connection cap; another source's connection was closed")
	}
}

func connFrom(addr string, port int) *fakeConn {
	return &fakeConn{remote: &net.TCPAddr{IP: net.ParseIP(addr), Port: port}}
}

// TestHTTPConnCapPerSourceLimit: one source gets maxHTTPConnsPerSource
// connections; the rest are closed unserved without costing a global slot,
// and closing admitted ones frees slots again.
func TestHTTPConnCapPerSourceLimit(t *testing.T) {
	c := &connCap{max: maxHTTPConns}
	var admitted []*fakeConn
	for i := range maxHTTPConnsPerSource {
		fc := connFrom("203.0.113.7", 10000+i)
		c.state(fc, http.StateNew)
		if fc.closed {
			t.Fatalf("connection %d closed under the cap", i)
		}
		admitted = append(admitted, fc)
	}
	extra := connFrom("203.0.113.7", 20000)
	c.state(extra, http.StateNew)
	if !extra.closed {
		t.Fatal("connection over the per-source cap served")
	}
	// net/http reports StateClosed for the refused one; it must not free a slot.
	c.state(extra, http.StateClosed)
	if n := c.active.Load(); n != maxHTTPConnsPerSource {
		t.Fatalf("active %d, want %d", n, maxHTTPConnsPerSource)
	}
	again := connFrom("203.0.113.7", 20001)
	c.state(again, http.StateNew)
	if !again.closed {
		t.Fatal("refused close freed a slot for its source")
	}
	// Closing an admitted one frees exactly one slot.
	c.state(admitted[0], http.StateClosed)
	c.state(admitted[0], http.StateClosed) // repeated report: no double free
	next := connFrom("203.0.113.7", 20002)
	c.state(next, http.StateNew)
	if next.closed {
		t.Fatal("slot not freed")
	}
	if over := connFrom("203.0.113.7", 20003); func() bool { c.state(over, http.StateNew); return !over.closed }() {
		t.Fatal("double StateClosed freed two slots")
	}
}

// TestHTTPConnCapSourceKey: the per-source cap uses the rate limiter's key,
// so IPv6 sources share a /64 and v4-mapped addresses share the v4 key.
func TestHTTPConnCapSourceKey(t *testing.T) {
	c := &connCap{max: maxHTTPConns}
	for i := range maxHTTPConnsPerSource {
		c.state(connFrom(fmt.Sprintf("2001:db8:1:2::%x", i+1), 1000+i), http.StateNew)
	}
	same := connFrom("2001:db8:1:2:ffff::1", 1) // same /64
	c.state(same, http.StateNew)
	if !same.closed {
		t.Fatal("same /64 not counted as one source")
	}
	other := connFrom("2001:db8:1:3::1", 1) // different /64
	c.state(other, http.StateNew)
	if other.closed {
		t.Fatal("different /64 refused")
	}
	for i := range maxHTTPConnsPerSource {
		c.state(connFrom("10.0.0.1", 2000+i), http.StateNew)
	}
	mapped := connFrom("::ffff:10.0.0.1", 1)
	c.state(mapped, http.StateNew)
	if !mapped.closed {
		t.Fatal("v4-mapped address not counted with its v4 address")
	}
}

// TestHTTPConnCapGlobalStillApplies: many sources can't exceed the global cap.
func TestHTTPConnCapGlobalStillApplies(t *testing.T) {
	c := &connCap{max: 4}
	for i := range 4 {
		fc := connFrom(fmt.Sprintf("198.51.100.%d", i+1), 1)
		c.state(fc, http.StateNew)
		if fc.closed {
			t.Fatalf("source %d refused under the global cap", i)
		}
	}
	extra := connFrom("198.51.100.99", 1)
	c.state(extra, http.StateNew)
	if !extra.closed {
		t.Fatal("global cap not enforced")
	}
}
