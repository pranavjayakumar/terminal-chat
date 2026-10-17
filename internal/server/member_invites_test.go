package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/proto"
)

var transports = []string{"quic", "tcp"}

func memberServer(t *testing.T, kind string) *testServer {
	t.Helper()
	return startServerCfg(t, kind, Config{MemberInvites: true})
}

func createInvite(t *testing.T, c *tclient, a proto.InviteCreateArgs) (proto.InviteRes, proto.Invite) {
	t.Helper()
	var res proto.InviteRes
	c.ok(proto.OpInviteCreate, a, &res)
	inv, err := proto.ParseInvite(res.Code)
	if err != nil {
		t.Fatal(err)
	}
	return res, inv
}

func listInvites(c *tclient) []proto.InviteInfo {
	c.t.Helper()
	var res proto.InvitesRes
	c.ok(proto.OpInviteList, nil, &res)
	return res.Invites
}

// inviteID is the id invite.list shows for inv: the first digits of the
// hash it is stored under (boundTokenHash for member and key-bound invites).
func (ts *testServer) inviteID(inv proto.Invite) string {
	return ts.storedHash(inv)[:proto.InviteIDLen]
}

func (ts *testServer) storedHash(inv proto.Invite) string {
	var n int
	ts.st.db.QueryRow(`SELECT COUNT(*) FROM invites WHERE token = ?`, boundTokenHash(inv.Token)).Scan(&n)
	if n > 0 {
		return boundTokenHash(inv.Token)
	}
	return tokenHash(inv.Token)
}

// A member creates user invites within the member limits; the joiner is a
// plain member. Members never mint admin or someone else's add-key invite.
func TestMemberInvites(t *testing.T) {
	for _, kind := range transports {
		t.Run(kind, func(t *testing.T) {
			ts := memberServer(t, kind)
			admin := ts.register(t, "admin", true)
			admin.syncFresh()
			bob := ts.register(t, "bob", false)
			bob.syncFresh()
			for _, c := range []string{proto.CapMemberInvites, proto.CapKeyInvites} {
				if !containsCap(bob.welcome.Caps, c) {
					t.Fatalf("WELCOME caps %v lack %s", bob.welcome.Caps, c)
				}
			}

			// Limits: uses 1-10, ttl at most 7 days.
			bob.fail(proto.OpInviteCreate, proto.InviteCreateArgs{Uses: proto.MaxMemberInviteUses + 1}, proto.ErrBadRequest)
			bob.fail(proto.OpInviteCreate, proto.InviteCreateArgs{TTLMs: (proto.MaxMemberInviteTTL + time.Minute).Milliseconds()}, proto.ErrBadRequest)
			bob.fail(proto.OpInviteCreate, proto.InviteCreateArgs{Uses: -1}, proto.ErrBadRequest)
			// Add-key: only for themselves.
			bob.fail(proto.OpInviteCreate, proto.InviteCreateArgs{User: admin.user.ID}, proto.ErrForbidden)
			bob.fail(proto.OpInviteCreate, proto.InviteCreateArgs{User: 9999}, proto.ErrForbidden)

			res, team := createInvite(t, bob, proto.InviteCreateArgs{Uses: proto.MaxMemberInviteUses, TTLMs: proto.MaxMemberInviteTTL.Milliseconds()})
			if res.Uses != proto.MaxMemberInviteUses {
				t.Fatalf("res %+v", res)
			}
			// Admins keep their own limits.
			_, adminInv := createInvite(t, admin, proto.InviteCreateArgs{Uses: 50, TTLMs: (20 * 24 * time.Hour).Milliseconds()})

			joiner, bye := ts.connect(t, newKey(t), team.Token, "carol")
			if bye != "" {
				t.Fatal(bye)
			}
			if joiner.user.Admin {
				t.Fatal("a member's invite made an admin")
			}

			// Self add-key invite works and counts toward the cap.
			_, selfKey := createInvite(t, bob, proto.InviteCreateArgs{User: bob.user.ID})
			newLaptop := newKey(t)
			if c, bye := ts.connect(t, newLaptop, selfKey.Token, ""); bye != "" || c.user.ID != bob.user.ID {
				t.Fatalf("self add-key: %q", bye)
			}

			// list: bob sees only his; the admin sees everyone's.
			mine := listInvites(bob)
			if len(mine) != 1 || mine[0].ID != ts.inviteID(team) || mine[0].CreatedBy != bob.user.ID || mine[0].UsesLeft != proto.MaxMemberInviteUses-1 {
				t.Fatalf("bob's list %+v", mine)
			}
			for _, inv := range mine {
				if strings.Contains(fmt.Sprintf("%+v", inv), team.Token) {
					t.Fatal("token in list")
				}
			}
			if all := listInvites(admin); len(all) != 2 {
				t.Fatalf("admin's list %+v", all)
			}
			// revoke: someone else's id is not_found (and stays live).
			bob.fail(proto.OpInviteRevoke, proto.InviteRevokeArgs{ID: ts.inviteID(adminInv)}, proto.ErrNotFound)
			if len(listInvites(admin)) != 2 {
				t.Fatal("bob revoked the admin's invite")
			}
			bob.ok(proto.OpInviteRevoke, proto.InviteRevokeArgs{ID: ts.inviteID(team)}, nil)
			if _, bye := ts.connect(t, newKey(t), team.Token, "dave"); bye != proto.ByeInviteInvalid {
				t.Fatalf("revoked member invite: %q", bye)
			}
			// The admin can revoke a member's invite.
			_, again := createInvite(t, bob, proto.InviteCreateArgs{})
			admin.ok(proto.OpInviteRevoke, proto.InviteRevokeArgs{ID: ts.inviteID(again)}, nil)
		})
	}
}

// At most MaxMemberInvites live invites per member; spent, expired and
// revoked ones free a slot.
func TestMemberInviteCap(t *testing.T) {
	ts := memberServer(t, "tcp")
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	var invs []proto.Invite
	for range proto.MaxMemberInvites {
		_, inv := createInvite(t, bob, proto.InviteCreateArgs{})
		invs = append(invs, inv)
	}
	bob.fail(proto.OpInviteCreate, proto.InviteCreateArgs{}, proto.ErrConflict)
	// Spending one frees a slot.
	if _, bye := ts.connect(t, newKey(t), invs[0].Token, "joiner0"); bye != "" {
		t.Fatal(bye)
	}
	_, inv := createInvite(t, bob, proto.InviteCreateArgs{})
	bob.fail(proto.OpInviteCreate, proto.InviteCreateArgs{}, proto.ErrConflict)
	// Expiry frees one.
	ts.st.db.Exec(`UPDATE invites SET expires = ? WHERE token = ?`, ts.now()-1, ts.storedHash(inv))
	_, inv = createInvite(t, bob, proto.InviteCreateArgs{})
	// Revoking frees one.
	bob.ok(proto.OpInviteRevoke, proto.InviteRevokeArgs{ID: ts.inviteID(inv)}, nil)
	createInvite(t, bob, proto.InviteCreateArgs{})
	bob.fail(proto.OpInviteCreate, proto.InviteCreateArgs{}, proto.ErrConflict)
	// The cap is per member, and admins have none.
	eve := ts.register(t, "eve", false)
	eve.syncFresh()
	createInvite(t, eve, proto.InviteCreateArgs{})
	admin := ts.register(t, "admin", true)
	admin.syncFresh()
	for range proto.MaxMemberInvites + 2 {
		createInvite(t, admin, proto.InviteCreateArgs{})
	}
}

// Concurrent creates can't race past the member cap.
func TestMemberInviteCapRace(t *testing.T) {
	ts := memberServer(t, "tcp")
	bob := ts.register(t, "bob", false)
	var wg sync.WaitGroup
	var won atomic.Int32
	for range 4 * proto.MaxMemberInvites {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ts.CreateInviteOpts(context.Background(), InviteOpts{CreatedBy: bob.user.ID, Member: true}); err == nil {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != proto.MaxMemberInvites {
		t.Fatalf("%d member invites created, cap %d", won.Load(), proto.MaxMemberInvites)
	}
}

// With --member-invites=false members get exactly v0.2.0's behaviour.
func TestMemberInvitesOff(t *testing.T) {
	for _, kind := range transports {
		t.Run(kind, func(t *testing.T) {
			ts := startServer(t, kind)
			bob := ts.register(t, "bob", false)
			bob.syncFresh()
			if containsCap(bob.welcome.Caps, proto.CapMemberInvites) {
				t.Fatal("member_invites advertised while off")
			}
			bob.fail(proto.OpInviteCreate, proto.InviteCreateArgs{}, proto.ErrForbidden)
			bob.fail(proto.OpInviteCreate, proto.InviteCreateArgs{User: bob.user.ID}, proto.ErrForbidden)
			bob.fail(proto.OpInviteList, nil, proto.ErrForbidden)
			bob.fail(proto.OpInviteRevoke, proto.InviteRevokeArgs{ID: "000000000000"}, proto.ErrForbidden)
		})
	}
}

// Turning the setting off stops members' outstanding invites (they stop
// being listed or redeemable) without touching admins'; turning it back on
// revives the unexpired ones.
func TestMemberInvitesTurnedOff(t *testing.T) {
	dir := t.TempDir()
	on := startServerCfg(t, "tcp", Config{DataDir: dir, MemberInvites: true})
	admin := on.register(t, "admin", true)
	admin.syncFresh()
	bob := on.register(t, "bob", false)
	bob.syncFresh()
	_, memberInv := createInvite(t, bob, proto.InviteCreateArgs{Uses: 2})
	_, adminInv := createInvite(t, admin, proto.InviteCreateArgs{Uses: 2})
	on.stop()
	on.Close()

	off := startServerCfg(t, "tcp", Config{DataDir: dir})
	if _, bye := off.connect(t, newKey(t), memberInv.Token, "x1"); bye != proto.ByeInviteInvalid {
		t.Fatalf("member invite redeemed while off: %q", bye)
	}
	if _, bye := off.connect(t, newKey(t), adminInv.Token, "x2"); bye != "" {
		t.Fatalf("admin invite while off: %q", bye)
	}
	invs, _ := off.ListInvites(context.Background())
	if len(invs) != 1 || invs[0].ID != off.inviteID(adminInv) {
		t.Fatalf("listed while off: %+v", invs)
	}
	off.stop()
	off.Close()

	back := startServerCfg(t, "tcp", Config{DataDir: dir, MemberInvites: true})
	if _, bye := back.connect(t, newKey(t), memberInv.Token, "x3"); bye != "" {
		t.Fatalf("member invite after turning back on: %q", bye)
	}
}

// Disabling a user deletes the invites they made, admin or member.
func TestDisableDeletesInvites(t *testing.T) {
	ts := memberServer(t, "tcp")
	root := ts.register(t, "root", true)
	root.syncFresh()
	admin2 := ts.register(t, "admin2", true)
	admin2.syncFresh()
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	_, bobInv := createInvite(t, bob, proto.InviteCreateArgs{Uses: 3})
	_, a2Inv := createInvite(t, admin2, proto.InviteCreateArgs{Uses: 3})
	_, rootInv := createInvite(t, root, proto.InviteCreateArgs{})
	root.ok(proto.OpUserDisable, proto.UserArgs{User: bob.user.ID}, nil)
	root.ok(proto.OpUserDisable, proto.UserArgs{User: admin2.user.ID}, nil)
	for _, inv := range []proto.Invite{bobInv, a2Inv} {
		if _, bye := ts.connect(t, newKey(t), inv.Token, "late"); bye != proto.ByeInviteInvalid {
			t.Fatalf("invite of a disabled user redeemed: %q", bye)
		}
	}
	if invs := listInvites(root); len(invs) != 1 || invs[0].ID != ts.inviteID(rootInv) {
		t.Fatalf("listed %+v", invs)
	}
	var n int
	ts.st.db.QueryRow(`SELECT COUNT(*) FROM invites WHERE created_by IN (?, ?)`, bob.user.ID, admin2.user.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d invites of disabled users left", n)
	}
}

func mustECDSA(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func rsaKey(t *testing.T, bits int) ssh.Signer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A key-bound invite can only be redeemed with one of its keys; a wrong
// key spends nothing.
func TestKeyInvites(t *testing.T) {
	alice1, alice2 := newKey(t), rsaKey(t, 2048)
	for _, kind := range transports {
		t.Run(kind, func(t *testing.T) {
			ts := memberServer(t, kind)
			admin := ts.register(t, "admin", true)
			admin.syncFresh()
			keys := [][]byte{alice1.PublicKey().Marshal(), alice2.PublicKey().Marshal(), alice1.PublicKey().Marshal()}
			res, inv := createInvite(t, admin, proto.InviteCreateArgs{Keys: keys, KeySrc: "github:alice"})
			if res.Keys != 2 || res.Uses != 1 {
				t.Fatalf("res %+v", res)
			}
			invs := listInvites(admin)
			if len(invs) != 1 || invs[0].Keys != 2 || invs[0].KeySrc != "github:alice" || invs[0].Kind != proto.InviteKindUser {
				t.Fatalf("listed %+v", invs)
			}

			// A leaked invite alone is useless.
			if _, bye := ts.connect(t, newKey(t), inv.Token, "mallory"); bye != proto.ByeInviteInvalid {
				t.Fatalf("other key: %q", bye)
			}
			if _, bye := ts.connect(t, rsaKey(t, 1024), inv.Token, "mallory"); bye != proto.ByeInviteInvalid {
				t.Fatalf("other key: %q", bye)
			}
			if invs := listInvites(admin); len(invs) != 1 || invs[0].UsesLeft != 1 {
				t.Fatalf("wrong key spent a use: %+v", invs)
			}
			c, bye := ts.connect(t, alice2, inv.Token, "alice")
			if bye != "" || c.user.Name != "alice" || c.user.Admin {
				t.Fatalf("right key: %q", bye)
			}
			if _, bye := ts.connect(t, alice1, inv.Token, "alice2"); bye != proto.ByeInviteInvalid {
				t.Fatalf("key invite redeemed twice: %q", bye)
			}

			// Members can bind theirs too.
			bob := ts.register(t, "bob", false)
			bob.syncFresh()
			k := newKey(t)
			_, inv = createInvite(t, bob, proto.InviteCreateArgs{Keys: [][]byte{k.PublicKey().Marshal()}, KeySrc: "github:dan-1"})
			if _, bye := ts.connect(t, newKey(t), inv.Token, "dan"); bye != proto.ByeInviteInvalid {
				t.Fatalf("member key invite, other key: %q", bye)
			}
			if _, bye := ts.connect(t, k, inv.Token, "dan"); bye != "" {
				t.Fatalf("member key invite: %q", bye)
			}

			// Bound add-key invites (admin, for a user).
			k2 := newKey(t)
			_, inv = createInvite(t, admin, proto.InviteCreateArgs{User: bob.user.ID, Keys: [][]byte{k2.PublicKey().Marshal()}})
			if _, bye := ts.connect(t, newKey(t), inv.Token, ""); bye != proto.ByeInviteInvalid {
				t.Fatalf("bound add-key, other key: %q", bye)
			}
			if c, bye := ts.connect(t, k2, inv.Token, ""); bye != "" || c.user.ID != bob.user.ID {
				t.Fatalf("bound add-key: %q", bye)
			}
		})
	}
}

// Bad keys and key sources are refused before anything is stored.
func TestKeyInviteValidation(t *testing.T) {
	ts := memberServer(t, "tcp")
	admin := ts.register(t, "admin", true)
	admin.syncFresh()
	good := newKey(t).PublicKey().Marshal()
	many := make([][]byte, proto.MaxInviteKeys+1)
	for i := range many {
		many[i] = newKey(t).PublicKey().Marshal()
	}
	ecdsa := []byte(nil)
	if s, err := ssh.NewSignerFromKey(mustECDSA(t)); err == nil {
		ecdsa = s.PublicKey().Marshal()
	}
	for name, a := range map[string]proto.InviteCreateArgs{
		"garbage":       {Keys: [][]byte{[]byte("not a key")}},
		"empty key":     {Keys: [][]byte{{}}},
		"trailing junk": {Keys: [][]byte{append(append([]byte{}, good...), 0)}},
		"rsa 1024":      {Keys: [][]byte{rsaKey(t, 1024).PublicKey().Marshal()}},
		"too many":      {Keys: many},
		"too long":      {Keys: [][]byte{make([]byte, proto.MaxInviteKeyLen+1)}},
		"multi-use":     {Keys: [][]byte{good}, Uses: 2},
		"bad src":       {Keys: [][]byte{good}, KeySrc: "github:a b"},
		"other src":     {Keys: [][]byte{good}, KeySrc: "gitlab:alice"},
		"long login":    {Keys: [][]byte{good}, KeySrc: "github:" + strings.Repeat("a", 40)},
		"ansi src":      {Keys: [][]byte{good}, KeySrc: "github:\x1b[31m"},
		"src no keys":   {KeySrc: "github:alice"},
	} {
		r := admin.req(proto.OpInviteCreate, a)
		if r.OK || r.Err == nil || r.Err.Code != proto.ErrBadRequest {
			t.Errorf("%s: ok=%v err=%v", name, r.OK, r.Err)
		}
	}
	var n int
	ts.st.db.QueryRow(`SELECT COUNT(*) FROM invites WHERE created_by = ?`, admin.user.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d invites stored", n)
	}
	// Exactly MaxInviteKeys and an ECDSA key are fine.
	createInvite(t, admin, proto.InviteCreateArgs{Keys: many[:proto.MaxInviteKeys]})
	if ecdsa != nil {
		createInvite(t, admin, proto.InviteCreateArgs{Keys: [][]byte{ecdsa}})
	}
}

// The server host can bind admin invites too (tc server invite --admin --key).
func TestHostKeyBoundAdminInvite(t *testing.T) {
	ts := startServer(t, "quic")
	k := newKey(t)
	inv := teamInvite(t, ts, InviteOpts{Admin: true, Keys: [][]byte{k.PublicKey().Marshal()}, KeySrc: "github:boss"})
	if _, bye := ts.connect(t, newKey(t), inv.Token, "eve"); bye != proto.ByeInviteInvalid {
		t.Fatalf("other key: %q", bye)
	}
	c, bye := ts.connect(t, k, inv.Token, "boss")
	if bye != "" || !c.user.Admin {
		t.Fatalf("bound admin invite: %q %+v", bye, c)
	}
	if _, err := ts.CreateInviteOpts(context.Background(), InviteOpts{Uses: 3, Keys: [][]byte{k.PublicKey().Marshal()}}); err == nil {
		t.Fatal("multi-use key invite accepted")
	}
}

// Key-bound and member invites aren't stored under the plain token hash,
// so a pre-key_invites server (after a downgrade), which looks invites up
// by tokenHash alone, can't redeem them unbound. Plain invites keep it.
func TestBoundInvitesHiddenFromOldServers(t *testing.T) {
	ts := memberServer(t, "tcp")
	admin := ts.register(t, "admin", true)
	admin.syncFresh()
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	_, plain := createInvite(t, admin, proto.InviteCreateArgs{Uses: 2})
	_, bound := createInvite(t, admin, proto.InviteCreateArgs{Keys: [][]byte{newKey(t).PublicKey().Marshal()}})
	_, member := createInvite(t, bob, proto.InviteCreateArgs{})
	oldLookup := func(inv proto.Invite) bool {
		var n int
		ts.st.db.QueryRow(`SELECT COUNT(*) FROM invites WHERE token = ?`, tokenHash(inv.Token)).Scan(&n)
		return n == 1
	}
	if !oldLookup(plain) || oldLookup(bound) || oldLookup(member) {
		t.Fatalf("old-server lookups: plain %v bound %v member %v", oldLookup(plain), oldLookup(bound), oldLookup(member))
	}
	// This server still redeems all of them.
	if _, bye := ts.connect(t, newKey(t), member.Token, "m1"); bye != "" {
		t.Fatalf("member invite: %q", bye)
	}
	if _, bye := ts.connect(t, newKey(t), plain.Token, "p1"); bye != "" {
		t.Fatalf("plain invite: %q", bye)
	}
}

// Invites expired for over a day are deleted (with their keys) when the
// next one is created, so short-lived invites can't pile up.
func TestExpiredInvitesCollected(t *testing.T) {
	ts := memberServer(t, "tcp")
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	_, old := createInvite(t, bob, proto.InviteCreateArgs{Keys: [][]byte{newKey(t).PublicKey().Marshal()}})
	_, recent := createInvite(t, bob, proto.InviteCreateArgs{})
	ts.st.db.Exec(`UPDATE invites SET expires = ? WHERE token = ?`, ts.now()-inviteGrace.Milliseconds()-1, ts.storedHash(old))
	ts.st.db.Exec(`UPDATE invites SET expires = ? WHERE token = ?`, ts.now()-1, ts.storedHash(recent))
	createInvite(t, bob, proto.InviteCreateArgs{})
	var invs, keys, kept int
	ts.st.db.QueryRow(`SELECT COUNT(*) FROM invites WHERE token = ?`, boundTokenHash(old.Token)).Scan(&invs)
	ts.st.db.QueryRow(`SELECT COUNT(*) FROM invite_keys`).Scan(&keys)
	ts.st.db.QueryRow(`SELECT COUNT(*) FROM invites WHERE token = ?`, boundTokenHash(recent.Token)).Scan(&kept)
	if invs != 0 || keys != 0 || kept != 1 {
		t.Fatalf("after collection: old %d keys %d recent %d", invs, keys, kept)
	}
}
