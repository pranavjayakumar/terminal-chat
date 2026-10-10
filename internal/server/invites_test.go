package server

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/skailama/tc/internal/proto"
)

func teamInvite(t *testing.T, ts *testServer, o InviteOpts) proto.Invite {
	t.Helper()
	res, err := ts.CreateInviteOpts(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := proto.ParseInvite(res.Code)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// N people can redeem an N-use invite; the N+1st gets invite_invalid.
func TestTeamInviteUses(t *testing.T) {
	ts := startServer(t, "tcp")
	inv := teamInvite(t, ts, InviteOpts{Uses: 3})
	for i := range 3 {
		if _, bye := ts.connect(t, newKey(t), inv.Token, fmt.Sprintf("member%d", i)); bye != "" {
			t.Fatalf("redemption %d: BYE %q", i+1, bye)
		}
	}
	if _, bye := ts.connect(t, newKey(t), inv.Token, "member3"); bye != proto.ByeInviteInvalid {
		t.Fatalf("4th redemption of a 3-use invite: BYE %q", bye)
	}
	if ts.UserCount() != 3 {
		t.Fatalf("users = %d, want 3", ts.UserCount())
	}
}

// A failed redemption (name taken, reserved name, known key) doesn't spend a use.
func TestTeamInviteFailedRedemptionKeepsUse(t *testing.T) {
	ts := startServer(t, "tcp")
	inv := teamInvite(t, ts, InviteOpts{Uses: 2})
	key := newKey(t)
	if _, bye := ts.connect(t, key, inv.Token, "alice"); bye != "" {
		t.Fatal(bye)
	}
	if _, bye := ts.connect(t, newKey(t), inv.Token, "alice"); bye != proto.ByeNameTaken {
		t.Fatalf("taken name: %q", bye)
	}
	if _, bye := ts.connect(t, key, inv.Token, "alice2"); bye != proto.ByeKeyExists {
		t.Fatalf("known key: %q", bye)
	}
	if _, bye := ts.connect(t, newKey(t), inv.Token, "x-bot"); bye != proto.ByeProtocolError {
		t.Fatalf("bot name: %q", bye)
	}
	if _, bye := ts.connect(t, newKey(t), inv.Token, "bob"); bye != "" {
		t.Fatalf("second use after failures: %q", bye)
	}
	if _, bye := ts.connect(t, newKey(t), inv.Token, "carol"); bye != proto.ByeInviteInvalid {
		t.Fatalf("third use: %q", bye)
	}
}

func TestTeamInviteExpiry(t *testing.T) {
	ts := startServer(t, "tcp")
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	ts.mu.Lock()
	ts.cfg.Now = func() time.Time { return time.Unix(0, now.Load()) }
	ts.mu.Unlock()
	inv := teamInvite(t, ts, InviteOpts{Uses: 5, TTL: time.Hour})
	if _, bye := ts.connect(t, newKey(t), inv.Token, "early"); bye != "" {
		t.Fatal(bye)
	}
	now.Add(int64(time.Hour + time.Second))
	if _, bye := ts.connect(t, newKey(t), inv.Token, "late"); bye != proto.ByeInviteInvalid {
		t.Fatalf("expired invite: BYE %q", bye)
	}
}

// Many connections race for a 5-use invite: exactly 5 win.
func TestTeamInviteConcurrentRedemption(t *testing.T) {
	ts := startServer(t, "tcp")
	const uses, racers = 5, 16
	inv := teamInvite(t, ts, InviteOpts{Uses: uses})
	var wg sync.WaitGroup
	var won atomic.Int32
	byes := make(chan string, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, bye := ts.connect(t, newKey(t), inv.Token, fmt.Sprintf("racer%d", i)); c != nil {
				won.Add(1)
			} else {
				byes <- bye
			}
		}()
	}
	wg.Wait()
	close(byes)
	if won.Load() != uses {
		t.Fatalf("%d redemptions won, want %d", won.Load(), uses)
	}
	for b := range byes {
		if b != proto.ByeInviteInvalid {
			t.Fatalf("loser got %q", b)
		}
	}
	if ts.UserCount() != uses {
		t.Fatalf("users = %d", ts.UserCount())
	}
}

func TestCreateInviteOptsLimits(t *testing.T) {
	ts := startServer(t, "tcp")
	ctx := context.Background()
	for _, o := range []InviteOpts{
		{Uses: -1}, {Uses: proto.MaxInviteUses + 1}, {TTL: -time.Hour}, {TTL: time.Second},
		{TTL: proto.MaxInviteTTL + time.Millisecond}, {User: 1, Uses: 2},
	} {
		if _, err := ts.CreateInviteOpts(ctx, o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
	res, err := ts.CreateInviteOpts(ctx, InviteOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uses != 1 || res.Expires-ts.now() > proto.DefaultInviteTTL.Milliseconds() ||
		res.Expires-ts.now() < proto.DefaultInviteTTL.Milliseconds()-60_000 {
		t.Fatalf("defaults: %+v", res)
	}
	if _, err := ts.CreateInviteOpts(ctx, InviteOpts{Uses: proto.MaxInviteUses, TTL: proto.MaxInviteTTL}); err != nil {
		t.Fatalf("max: %v", err)
	}
}

func TestOpInviteCreateTeam(t *testing.T) {
	ts := startServer(t, "tcp")
	admin := ts.register(t, "admin", true)
	admin.syncFresh()
	user := ts.register(t, "user", false)
	user.syncFresh()
	if !containsCap(admin.welcome.Caps, proto.CapTeamInvites) {
		t.Fatalf("WELCOME caps %v lack %s", admin.welcome.Caps, proto.CapTeamInvites)
	}

	user.fail(proto.OpInviteCreate, proto.InviteCreateArgs{Uses: 3}, proto.ErrForbidden)
	for _, a := range []proto.InviteCreateArgs{
		{Uses: -1}, {Uses: 101}, {TTLMs: -1}, {TTLMs: 1000},
		{TTLMs: proto.MaxInviteTTL.Milliseconds() + 1}, {User: user.user.ID, Uses: 2},
	} {
		admin.fail(proto.OpInviteCreate, a, proto.ErrBadRequest)
	}

	var res proto.InviteRes
	admin.ok(proto.OpInviteCreate, proto.InviteCreateArgs{Uses: 2, TTLMs: time.Hour.Milliseconds()}, &res)
	if res.Uses != 2 || res.Expires <= ts.now() || res.Expires > ts.now()+time.Hour.Milliseconds() {
		t.Fatalf("res %+v", res)
	}
	inv, _ := proto.ParseInvite(res.Code)
	for _, name := range []string{"m1", "m2"} {
		c, bye := ts.connect(t, newKey(t), inv.Token, name)
		if bye != "" {
			t.Fatalf("%s: %q", name, bye)
		}
		if c.user.Admin {
			t.Fatalf("op invite made an admin")
		}
	}
	if _, bye := ts.connect(t, newKey(t), inv.Token, "m3"); bye != proto.ByeInviteInvalid {
		t.Fatalf("m3: %q", bye)
	}
	// Defaults: one use.
	admin.ok(proto.OpInviteCreate, proto.InviteCreateArgs{}, &res)
	if res.Uses != 1 {
		t.Fatalf("default uses %d", res.Uses)
	}
}

func containsCap(caps []string, c string) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}

// A database from before team invites gets invites.uses = 1 for existing rows,
// and a used one stays used.
func TestInviteUsesMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "tc.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE invites (token TEXT PRIMARY KEY, admin INTEGER NOT NULL DEFAULT 0,
		user INTEGER, created INTEGER NOT NULL, expires INTEGER NOT NULL, used INTEGER NOT NULL DEFAULT 0);
		INSERT INTO invites (token, created, expires, used) VALUES ('a', 0, 1, 0), ('b', 0, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := openStore(filepath.Join(dir, "tc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	var a, b int
	st.db.QueryRow(`SELECT uses FROM invites WHERE token = 'a'`).Scan(&a)
	st.db.QueryRow(`SELECT uses FROM invites WHERE token = 'b'`).Scan(&b)
	if a != 1 || b != 1 {
		t.Fatalf("uses after migration: a=%d b=%d", a, b)
	}
	if st2, err := openStore(filepath.Join(dir, "tc.db")); err == nil { // idempotent
		st2.close()
	} else {
		t.Fatal(err)
	}
}
