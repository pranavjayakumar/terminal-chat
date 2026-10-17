package server

import (
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/proto"
)

// At the exact millisecond an invite expires, redeem still accepts it
// (expires < now is "expired") while the member cap, invite.list and
// invite.revoke already treat it as gone (expires > now is "live"). So for
// that instant a member can hold MaxMemberInvites+1 redeemable invites, and
// a revoke reports not_found for an invite that can still be redeemed.
func TestMemberInviteCap_BreakExpiryBoundary(t *testing.T) {
	clk := newClock()
	ts := startServerCfg(t, "tcp", Config{MemberInvites: true, Now: clk.Now})
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	var invs []proto.Invite
	for range proto.MaxMemberInvites {
		_, inv := createInvite(t, bob, proto.InviteCreateArgs{TTLMs: time.Minute.Milliseconds()})
		invs = append(invs, inv)
	}
	clk.Advance(time.Minute) // now == expires of all five

	extra := bob.req(proto.OpInviteCreate, proto.InviteCreateArgs{})
	revoke := bob.req(proto.OpInviteRevoke, proto.InviteRevokeArgs{ID: ts.inviteID(invs[1])})
	redeemed := 0
	for i, inv := range invs {
		if i == 1 && revoke.OK {
			continue
		}
		if _, bye := ts.connect(t, newKey(t), inv.Token, fmt.Sprintf("j%d", i)); bye == "" {
			redeemed++
		}
	}
	if extra.OK && redeemed > 0 {
		t.Errorf("member cap: a 6th invite was created while %d of the 5 were still redeemable", redeemed)
	}
	if !revoke.OK && redeemed > 0 {
		t.Errorf("invite.revoke said %v for an invite redeem still accepts", revoke.Err)
	}
}

// The cap holds across several concurrent sessions of the same member,
// over the wire.
func TestMemberInviteCap_BreakManySessions(t *testing.T) {
	ts := memberServer(t, "tcp")
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	const sessions = 4
	cs := []*tclient{bob}
	for range sessions - 1 {
		c, bye := ts.connect(t, bob.signer, "", "")
		if bye != "" {
			t.Fatal(bye)
		}
		c.syncFresh()
		cs = append(cs, c)
	}
	var wg sync.WaitGroup
	var won, capped atomic.Int32
	for _, c := range cs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range proto.MaxMemberInvites {
				r := c.req(proto.OpInviteCreate, proto.InviteCreateArgs{Uses: 10})
				switch {
				case r.OK:
					won.Add(1)
				case r.Err != nil && r.Err.Code == proto.ErrConflict:
					capped.Add(1)
				default:
					t.Errorf("create: %v", r.Err)
				}
			}
		}()
	}
	wg.Wait()
	if won.Load() != proto.MaxMemberInvites || int(capped.Load()) != sessions*proto.MaxMemberInvites-proto.MaxMemberInvites {
		t.Fatalf("won %d capped %d", won.Load(), capped.Load())
	}
	if n := len(listInvites(bob)); n != proto.MaxMemberInvites {
		t.Fatalf("listed %d", n)
	}
}

// Concurrent redemptions of one key-bound invite with the right key and
// many wrong keys: exactly one user is made, with the right key.
func TestKeyInvite_BreakConcurrentRightWrong(t *testing.T) {
	ts := memberServer(t, "tcp")
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	right := newKey(t)
	_, inv := createInvite(t, bob, proto.InviteCreateArgs{Keys: [][]byte{right.PublicKey().Marshal()}})
	before := ts.UserCount()
	var wg sync.WaitGroup
	var ok atomic.Int32
	var rightOK atomic.Bool
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := newKey(t)
			if i == 5 {
				key = right
			}
			c, bye := ts.connect(t, key, inv.Token, fmt.Sprintf("racer%d", i))
			if bye == "" {
				ok.Add(1)
				if i == 5 {
					rightOK.Store(true)
				}
				c.conn.Close()
			} else if bye != proto.ByeInviteInvalid {
				t.Errorf("racer %d: %q", i, bye)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || !rightOK.Load() || ts.UserCount() != before+1 {
		t.Fatalf("ok=%d right=%v users %d->%d", ok.Load(), rightOK.Load(), before, ts.UserCount())
	}
}

// user.disable racing a member's creates (several sessions) and
// redemptions of their invites: once disable returns, nothing of theirs
// is left, and no redemption of their invites succeeds afterwards.
func TestMemberInvite_BreakDisableRace(t *testing.T) {
	ts := memberServer(t, "tcp")
	admin := ts.register(t, "admin", true)
	admin.syncFresh()
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	var codes []proto.Invite
	for range 3 {
		_, inv := createInvite(t, bob, proto.InviteCreateArgs{Uses: 10})
		codes = append(codes, inv)
	}
	extra, bye := ts.connect(t, bob.signer, "", "")
	if bye != "" {
		t.Fatal(bye)
	}
	extra.syncFresh()

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, c := range []*tclient{bob, extra} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 10 {
				c.nextID++
				if c.codec.Write(proto.TReq, proto.Req{ID: c.nextID, Op: proto.OpInviteCreate, Args: proto.MustRaw(proto.InviteCreateArgs{})}) != nil {
					return
				}
			}
		}()
	}
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if c, bye := ts.connect(t, newKey(t), codes[i%len(codes)].Token, fmt.Sprintf("r%d", i)); bye == "" {
				c.conn.Close()
			}
		}()
	}
	close(start)
	admin.ok(proto.OpUserDisable, proto.UserArgs{User: bob.user.ID}, nil)
	wg.Wait()
	time.Sleep(100 * time.Millisecond) // let in-flight creates hit the store
	var n int
	ts.st.db.QueryRow(`SELECT COUNT(*) FROM invites WHERE created_by = ?`, bob.user.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d invites of a disabled member survive", n)
	}
	for i, inv := range codes {
		if _, bye := ts.connect(t, newKey(t), inv.Token, fmt.Sprintf("late%d", i)); bye != proto.ByeInviteInvalid {
			t.Fatalf("redeemed after disable: %q", bye)
		}
	}
	if invs := listInvites(admin); len(invs) != 0 {
		t.Fatalf("listed %+v", invs)
	}
}

// Non-canonical encodings of an otherwise valid key are refused, so one
// key can't be bound under a fingerprint that auth never produces.
func TestKeyInvite_BreakNonCanonical(t *testing.T) {
	ts := memberServer(t, "tcp")
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	rsaPub := rsaKey(t, 2048).PublicKey().Marshal()
	var w struct {
		Name string
		E    []byte
		N    []byte
	}
	if err := ssh.Unmarshal(rsaPub, &w); err != nil {
		t.Fatal(err)
	}
	padded := func(b []byte) []byte { return append([]byte{0}, b...) }
	str := func(b []byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
		return append(out, b...)
	}
	enc := func(e, n []byte) []byte {
		out := str([]byte(ssh.KeyAlgoRSA))
		return append(append(out, str(e)...), str(n)...)
	}
	ed := newKey(t).PublicKey().Marshal()
	for name, k := range map[string][]byte{
		"rsa padded e": enc(padded(w.E), w.N),
		"rsa padded n": enc(w.E, padded(w.N)),
		"ed25519 long": append(append([]byte{}, ed[:len(ed)-36]...), append(binary.BigEndian.AppendUint32(nil, 33), append(ed[len(ed)-32:], 0)...)...),
		"dsa":          dsaLike(),
	} {
		r := bob.req(proto.OpInviteCreate, proto.InviteCreateArgs{Keys: [][]byte{k}})
		if r.OK || r.Err == nil || r.Err.Code != proto.ErrBadRequest {
			t.Errorf("%s: ok=%v err=%v", name, r.OK, r.Err)
		}
	}
	if n := len(listInvites(bob)); n != 0 {
		t.Fatalf("%d invites stored", n)
	}
}

// dsaLike is a syntactically plausible ssh-dss blob.
func dsaLike() []byte {
	str := func(b []byte) []byte { return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...) }
	out := str([]byte("ssh-dss"))
	for _, n := range [][]byte{{0x7f, 1}, {0x7f, 3}, {2}, {5}} {
		out = append(out, str(n)...)
	}
	return out
}

// A member's invite redeemed for a new user never grants admin and the
// joiner can't use member limits to reach admin-only ops.
func TestMemberInvite_BreakNoEscalation(t *testing.T) {
	ts := memberServer(t, "tcp")
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	_, inv := createInvite(t, bob, proto.InviteCreateArgs{Uses: 2})
	carol, bye := ts.connect(t, newKey(t), inv.Token, "carol")
	if bye != "" {
		t.Fatal(bye)
	}
	carol.syncFresh()
	if carol.user.Admin {
		t.Fatal("admin via member invite")
	}
	carol.fail(proto.OpUserDisable, proto.UserArgs{User: bob.user.ID}, proto.ErrForbidden)
	// carol can't see or revoke bob's invite.
	if n := len(listInvites(carol)); n != 0 {
		t.Fatalf("carol sees %d invites", n)
	}
	carol.fail(proto.OpInviteRevoke, proto.InviteRevokeArgs{ID: ts.inviteID(inv)}, proto.ErrNotFound)
	// Nor make an add-key invite for bob (account takeover).
	carol.fail(proto.OpInviteCreate, proto.InviteCreateArgs{User: bob.user.ID}, proto.ErrForbidden)
	carol.fail(proto.OpInviteCreate, proto.InviteCreateArgs{User: bob.user.ID, Keys: [][]byte{newKey(t).PublicKey().Marshal()}}, proto.ErrForbidden)
	var n int
	ts.st.db.QueryRow(`SELECT COUNT(*) FROM invites WHERE admin = 1 OR user IS NOT NULL`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d admin/add-key invites exist", n)
	}
}
