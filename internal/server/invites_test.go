package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

// A failed redemption (name taken, reserved name) doesn't spend a use, and
// a known key logs in without spending one.
func TestTeamInviteFailedRedemptionKeepsUse(t *testing.T) {
	ts := startServer(t, "tcp")
	inv := teamInvite(t, ts, InviteOpts{Uses: 2})
	key := newKey(t)
	alice, bye := ts.connect(t, key, inv.Token, "alice")
	if bye != "" {
		t.Fatal(bye)
	}
	if _, bye := ts.connect(t, newKey(t), inv.Token, "alice"); bye != proto.ByeNameTaken {
		t.Fatalf("taken name: %q", bye)
	}
	if c, bye := ts.connect(t, key, inv.Token, "alice2"); bye != "" || c.user.ID != alice.user.ID {
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
		{Admin: true, Uses: 2}, {Admin: true, Uses: proto.MaxInviteUses},
		{Admin: true, TTL: proto.MaxAdminInviteTTL + time.Millisecond}, {Admin: true, TTL: proto.MaxInviteTTL},
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

// A leaked admin invite must not mint many admins or live for weeks: single
// use, at most 24 h, and 24 h by default.
func TestAdminInviteSingleUseShortLived(t *testing.T) {
	ts := startServer(t, "tcp")
	ctx := context.Background()
	res, err := ts.CreateInviteOpts(ctx, InviteOpts{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	if left := res.Expires - ts.now(); res.Uses != 1 || left > proto.MaxAdminInviteTTL.Milliseconds() || left < proto.MaxAdminInviteTTL.Milliseconds()-60_000 {
		t.Fatalf("admin defaults: %+v (expires in %v)", res, time.Duration(left)*time.Millisecond)
	}
	if _, err := ts.CreateInviteOpts(ctx, InviteOpts{Admin: true, Uses: 1, TTL: proto.MaxAdminInviteTTL}); err != nil {
		t.Fatalf("24h admin invite: %v", err)
	}
	inv, _ := proto.ParseInvite(res.Code)
	c, bye := ts.connect(t, newKey(t), inv.Token, "boss")
	if bye != "" || !c.user.Admin {
		t.Fatalf("admin redemption: %q %+v", bye, c.user)
	}
	if _, bye := ts.connect(t, newKey(t), inv.Token, "boss2"); bye != proto.ByeInviteInvalid {
		t.Fatalf("second redemption: %q", bye)
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

// invite.list shows live invites without their tokens; invite.revoke kills
// one at once. Both are admin-only.
func TestInviteListRevoke(t *testing.T) {
	ts := startServer(t, "tcp")
	ctx := context.Background()
	admin := ts.register(t, "admin", true)
	admin.syncFresh()
	user := ts.register(t, "user", false)
	user.syncFresh()
	if !containsCap(admin.welcome.Caps, proto.CapInviteManage) {
		t.Fatalf("WELCOME caps %v lack %s", admin.welcome.Caps, proto.CapInviteManage)
	}

	var team proto.InviteRes
	admin.ok(proto.OpInviteCreate, proto.InviteCreateArgs{Uses: 5, TTLMs: time.Hour.Milliseconds()}, &team)
	key := teamInvite(t, ts, InviteOpts{User: user.user.ID})
	boss := teamInvite(t, ts, InviteOpts{Admin: true})
	// Not listed: used up, or expired.
	used := teamInvite(t, ts, InviteOpts{})
	if _, bye := ts.connect(t, newKey(t), used.Token, "early"); bye != "" {
		t.Fatal(bye)
	}
	gone := teamInvite(t, ts, InviteOpts{TTL: time.Minute})
	ts.st.db.Exec(`UPDATE invites SET expires = ? WHERE token = ?`, ts.now()-1, tokenHash(gone.Token))

	user.fail(proto.OpInviteList, nil, proto.ErrForbidden)
	user.fail(proto.OpInviteRevoke, proto.InviteRevokeArgs{ID: "000000000000"}, proto.ErrForbidden)

	var res proto.InvitesRes
	admin.ok(proto.OpInviteList, nil, &res)
	if len(res.Invites) != 3 {
		t.Fatalf("listed %+v", res.Invites)
	}
	byKind := map[string]proto.InviteInfo{}
	teamInv, _ := proto.ParseInvite(team.Code)
	for _, inv := range res.Invites {
		byKind[inv.Kind] = inv
		if !proto.ValidInviteID(inv.ID) {
			t.Fatalf("bad id %q", inv.ID)
		}
		for _, tok := range []string{teamInv.Token, key.Token, boss.Token} {
			if strings.Contains(inv.ID, tok) || strings.Contains(fmt.Sprintf("%+v", inv), tok) {
				t.Fatal("token leaked in invite.list")
			}
		}
	}
	tm := byKind[proto.InviteKindUser]
	if tm.UsesLeft != 5 || tm.CreatedBy != admin.user.ID || tm.Expires != team.Expires || tm.ID != tokenHash(teamInv.Token)[:proto.InviteIDLen] {
		t.Fatalf("team invite %+v", tm)
	}
	if k := byKind[proto.InviteKindKey]; k.User != user.user.ID || k.CreatedBy != 0 || k.UsesLeft != 1 {
		t.Fatalf("key invite %+v", k)
	}
	if _, ok := byKind[proto.InviteKindAdmin]; !ok {
		t.Fatalf("admin invite missing: %+v", res.Invites)
	}

	// One redemption shows up as one use fewer.
	if _, bye := ts.connect(t, newKey(t), teamInv.Token, "m1"); bye != "" {
		t.Fatal(bye)
	}
	admin.ok(proto.OpInviteList, nil, &res)
	for _, inv := range res.Invites {
		if inv.ID == tm.ID && inv.UsesLeft != 4 {
			t.Fatalf("uses left %d after a redemption", inv.UsesLeft)
		}
	}

	admin.fail(proto.OpInviteRevoke, proto.InviteRevokeArgs{ID: "nothex"}, proto.ErrBadRequest)
	admin.fail(proto.OpInviteRevoke, proto.InviteRevokeArgs{ID: "%"}, proto.ErrBadRequest)
	admin.fail(proto.OpInviteRevoke, proto.InviteRevokeArgs{ID: "000000000000"}, proto.ErrNotFound)
	admin.ok(proto.OpInviteRevoke, proto.InviteRevokeArgs{ID: tm.ID}, nil)
	if _, bye := ts.connect(t, newKey(t), teamInv.Token, "m2"); bye != proto.ByeInviteInvalid {
		t.Fatalf("revoked invite redeemed: %q", bye)
	}
	admin.fail(proto.OpInviteRevoke, proto.InviteRevokeArgs{ID: tm.ID}, proto.ErrNotFound)
	admin.ok(proto.OpInviteList, nil, &res)
	if len(res.Invites) != 2 {
		t.Fatalf("after revoke: %+v", res.Invites)
	}

	// The server-host API used by `tc server invite --revoke`.
	if err := ts.RevokeInvite(ctx, byKind[proto.InviteKindAdmin].ID); err != nil {
		t.Fatal(err)
	}
	if _, bye := ts.connect(t, newKey(t), boss.Token, "boss"); bye != proto.ByeInviteInvalid {
		t.Fatalf("revoked admin invite redeemed: %q", bye)
	}
	if err := ts.RevokeInvite(ctx, "zz"); !errors.Is(err, ErrInviteNotFound) {
		t.Fatalf("bad id: %v", err)
	}
}

// Invites from before created_by existed list as made on the server host.
func TestInviteCreatedByMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "tc.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE invites (token TEXT PRIMARY KEY, admin INTEGER NOT NULL DEFAULT 0,
		user INTEGER, created INTEGER NOT NULL, expires INTEGER NOT NULL, used INTEGER NOT NULL DEFAULT 0,
		uses INTEGER NOT NULL DEFAULT 1);
		INSERT INTO invites (token, created, expires) VALUES ('abc', 0, 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := openStore(filepath.Join(dir, "tc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	var by int64
	if err := st.db.QueryRow(`SELECT created_by FROM invites WHERE token = 'abc'`).Scan(&by); err != nil || by != 0 {
		t.Fatalf("created_by %d %v", by, err)
	}
}
