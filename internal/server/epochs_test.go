package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// manualClock freezes the server clock; the returned func moves it forward.
// Call before any client connects.
func manualClock(ts *testServer) func(time.Duration) {
	var ns atomic.Int64
	ns.Store(time.Now().UnixNano())
	ts.mu.Lock()
	ts.cfg.Now = func() time.Time { return time.Unix(0, ns.Load()) }
	ts.mu.Unlock()
	return func(d time.Duration) { ns.Add(int64(d)) }
}

// bundle builds a bundle for ch at epoch (after prev, nil for epoch 1), signed by by.
func bundle(t *testing.T, by *e2ecrypto.DeviceKeys, sid e2ecrypto.ServerID, ch uint64, epoch uint32,
	prev *proto.Signed, roster []uint64, to ...*e2ecrypto.DeviceKeys) proto.Signed {
	t.Helper()
	k, err := e2ecrypto.NewEpochKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := identity.EpochParams{ServerID: sid, Chan: ch, Epoch: epoch, Roster: roster, Time: time.Now().UnixMilli(), K: k}
	if prev != nil {
		p.Prev = e2ecrypto.ChainHash(prev.B)
	}
	if epoch > 1 {
		// The server can't open backlinks; any K_prev will do.
		pk, err := e2ecrypto.NewEpochKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		p.PrevK = &pk
	}
	for _, d := range to {
		p.Recipients = append(p.Recipients, identity.Recipient{Dev: d.ID, X25519: d.X25519Pub})
	}
	s, err := identity.NewEpochBundle(rand.Reader, by, p)
	if err != nil {
		t.Fatal(err)
	}
	return proto.Signed(s)
}

func (c *tclient) rotate(ch uint64, b proto.Signed) uint64 {
	c.t.Helper()
	var r proto.SeqRes
	c.ok(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch, Bundle: b}, &r)
	if r.Seq == 0 {
		c.t.Fatal("rotate: seq 0")
	}
	return r.Seq
}

func (c *tclient) createE2E(name string) proto.Channel {
	c.t.Helper()
	var r proto.ChanRes
	c.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: name, Private: true, E2E: true}, &r)
	if !r.Chan.E2E || r.Chan.Epoch != 0 {
		c.t.Fatalf("created %+v", r.Chan)
	}
	return r.Chan
}

// keyEpoch waits for the next key.epoch and returns its seq and bundle.
func (c *tclient) keyEpoch() (uint64, proto.Signed) {
	c.t.Helper()
	ev := c.event(proto.KKeyEpoch)
	var d proto.KeyEpoch
	if err := proto.Unmarshal(ev.Data, &d); err != nil {
		c.t.Fatal(err)
	}
	return ev.Seq, d.Bundle
}

// noKind fails if an EVENT of kind arrives (held or within d). Other frames are dropped.
func (c *tclient) noKind(kind string, d time.Duration) {
	c.t.Helper()
	check := func(f proto.Frame) {
		if f.Type != proto.TEvent {
			return
		}
		var ev proto.Event
		f.Decode(&ev)
		if ev.Kind == kind {
			c.t.Fatalf("%s: unexpected %s seq %d", c.user.Name, kind, ev.Seq)
		}
	}
	for _, f := range c.held {
		check(f)
	}
	c.held = nil
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				return
			}
			check(f)
		case <-deadline:
			return
		}
	}
}

// replayCaps reconnects c's key with caps, syncs from since and returns the
// replayed events.
func (c *tclient) replayCaps(t *testing.T, ts *testServer, since uint64, caps []string) []proto.Event {
	t.Helper()
	c2, bye := ts.connectCaps(t, c.signer, "", "", caps)
	if bye != "" {
		t.Fatal(bye)
	}
	c2.nextID++
	c2.codec.Write(proto.TReq, proto.Req{ID: c2.nextID, Op: proto.OpSync,
		Args: proto.MustRaw(proto.SyncArgs{Since: since, ServerID: ts.st.serverID})})
	var evs []proto.Event
	for {
		f := c2.recv("replay")
		switch f.Type {
		case proto.TEvent:
			var ev proto.Event
			f.Decode(&ev)
			evs = append(evs, ev)
		case proto.TSnapshot:
			t.Fatal("got a snapshot, want a replay")
		case proto.TRes:
			return evs
		}
	}
}

func keyEpochSeqs(evs []proto.Event) []uint64 {
	var out []uint64
	for _, ev := range evs {
		if ev.Kind == proto.KKeyEpoch {
			out = append(out, ev.Seq)
		}
	}
	return out
}

func TestEpochLimitsMatchIdentity(t *testing.T) {
	if proto.MaxBundle != identity.MaxBundleSize || proto.MaxBundleBoxes != identity.MaxBundleBoxes {
		t.Fatal("proto bundle limits drifted from identity/e2ecrypto")
	}
}

func TestKeyRotate(t *testing.T) {
	forEachTransport(t, func(t *testing.T, ts *testServer) {
		advance := manualClock(ts)
		alice := ts.registerCaps(t, "alice", true, e2eCaps)
		bob := ts.registerCaps(t, "bob", false, e2eCaps)
		carol := ts.registerCaps(t, "carol", false, e2eCaps)
		old := ts.register(t, "old", false)
		for _, c := range []*tclient{alice, bob, carol, old} {
			c.syncFresh()
		}
		sid := ts.sid()
		ch := alice.createE2E("sec")
		alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
		alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: old.user.ID}, nil)
		a1, _ := alice.registerDevice(ts)
		b1, _ := bob.registerDevice(ts)
		carol.registerDevice(ts)
		roster := []uint64{alice.user.ID, bob.user.ID, old.user.ID}

		// The rotator's own event comes before its RES.
		e1 := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1)
		seq := alice.rotate(ch.ID, e1)
		if s, got := alice.keyEpoch(); s != seq || !bytes.Equal(got.B, e1.B) || !bytes.Equal(got.Sig, e1.Sig) {
			t.Fatalf("alice key.epoch seq %d (res %d)", s, seq)
		}
		if s, got := bob.keyEpoch(); s != seq || !bytes.Equal(got.B, e1.B) {
			t.Fatalf("bob key.epoch seq %d", s)
		}
		carol.noKind(proto.KKeyEpoch, 100*time.Millisecond) // not a member
		old.noKind(proto.KKeyEpoch, 50*time.Millisecond)    // member without e2e1

		advance(rotateChanEveryMs * time.Millisecond) // nothing changed: past the channel cap
		// Epoch 2 by bob, chained on epoch 1, with the channel's epoch updated.
		e2 := bundle(t, b1, sid, ch.ID, 2, &e1, roster, b1, a1)
		seq2 := bob.rotate(ch.ID, e2)
		if s, _ := alice.keyEpoch(); s != seq2 {
			t.Fatalf("alice epoch 2 seq %d, want %d", s, seq2)
		}
		c, err := getChannel(context.Background(), ts.st.db, ch.ID)
		if err != nil || c.Epoch != 2 || !c.E2E {
			t.Fatalf("channel %+v %v", c, err)
		}

		// Old clients: no ops, no events.
		old.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: e2}, proto.ErrUnknownOp)
		old.fail(proto.OpKeyFetch, proto.KeyFetchArgs{Chan: ch.ID, From: 1, To: 2}, proto.ErrUnknownOp)
		old.noKind(proto.KKeyEpoch, 50*time.Millisecond)
	})
}

func TestKeyRotateRejects(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	carol := ts.registerCaps(t, "carol", false, e2eCaps)
	dave := ts.registerCaps(t, "dave", false, e2eCaps)
	old := ts.register(t, "old", false)
	snap, _ := alice.syncFresh()
	for _, c := range []*tclient{bob, carol, dave, old} {
		c.syncFresh()
	}
	gen := generalID(t, snap)
	sid := ts.sid()
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	var plain proto.ChanRes
	alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "plain", Private: true}, &plain)
	a1, _ := alice.registerDevice(ts)
	a2, _ := alice.registerDevice(ts)
	b1, _ := bob.registerDevice(ts)
	c1, _ := carol.registerDevice(ts)
	d1, _ := dave.registerDevice(ts)
	alice.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, alice.signer, sid, alice.user.ID, a2.ID)}, nil)
	roster := []uint64{alice.user.ID, bob.user.ID}
	ok1 := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1)

	rot := func(c *tclient, chID uint64, b proto.Signed, code string) {
		t.Helper()
		c.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: chID, Bundle: b}, code)
	}
	old.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: ok1}, proto.ErrUnknownOp)
	rot(alice, ch.ID, proto.Signed{B: make([]byte, proto.MaxBundle), Sig: ok1.Sig}, proto.ErrTooLarge)
	rot(alice, ch.ID, proto.Signed{B: []byte{0xc1}, Sig: ok1.Sig}, proto.ErrBadRequest)
	rot(alice, ch.ID, bundle(t, a1, e2ecrypto.ServerID{0xee}, ch.ID, 1, nil, roster, a1, b1), proto.ErrBadRequest)
	// b is canonical: an unknown field is rejected, so extensions need v: 2,
	// which this server doesn't accept either.
	type extended struct {
		rawBundleB `msgpack:",inline"`
		Ext        []byte `msgpack:"ext"`
	}
	rot(alice, ch.ID, signRaw(t, a1, extended{rawOf(t, ok1), []byte{1}}), proto.ErrBadRequest)
	v2 := rawOf(t, ok1)
	v2.V = 2
	rot(alice, ch.ID, signRaw(t, a1, v2), proto.ErrBadRequest)
	sec2 := alice.createE2E("sec2")
	rot(alice, sec2.ID, ok1, proto.ErrBadRequest) // bundle.chan != chan
	// Existence, membership and encryption are checked before b is decoded.
	garbage := proto.Signed{B: []byte{0xc1}, Sig: ok1.Sig}
	rot(carol, ch.ID, garbage, proto.ErrForbidden)
	rot(alice, 999999, garbage, proto.ErrNotFound)
	rot(alice, 999999, ok1, proto.ErrNotFound)
	rot(alice, 999999, bundle(t, a1, sid, 999999, 1, nil, roster, a1, b1), proto.ErrNotFound)
	rot(carol, ch.ID, bundle(t, c1, sid, ch.ID, 1, nil, roster, a1, b1), proto.ErrForbidden) // not a member
	rot(alice, plain.Chan.ID, bundle(t, a1, sid, plain.Chan.ID, 1, nil, []uint64{alice.user.ID}, a1), proto.ErrBadRequest)
	rot(alice, gen, bundle(t, a1, sid, gen, 1, nil, roster, a1, b1), proto.ErrBadRequest)    // not encrypted
	rot(alice, ch.ID, bundle(t, b1, sid, ch.ID, 1, nil, roster, a1, b1), proto.ErrForbidden) // bob's device
	rot(alice, ch.ID, bundle(t, a2, sid, ch.ID, 1, nil, roster, a1, b1), proto.ErrForbidden) // revoked device
	rot(alice, ch.ID, bundle(t, newDevKeys(t), sid, ch.ID, 1, nil, roster, a1, b1), proto.ErrForbidden)
	forged := *a1
	forged.Ed25519 = b1.Ed25519 // by = a1, signed by b1's key
	rot(alice, ch.ID, bundle(t, &forged, sid, ch.ID, 1, nil, roster, a1, b1), proto.ErrBadRequest)

	// CAS on an empty chain.
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 2, &ok1, roster, a1, b1), proto.ErrStaleEpoch)
	// Roster: missing or extra (a disabled member: below).
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, []uint64{alice.user.ID}, a1, b1), proto.ErrStaleEpoch)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, append(slices.Clone(roster), carol.user.ID), a1, b1), proto.ErrStaleEpoch)
	// Boxes outside the members' active devices are client errors: another
	// user's device, an unknown one. A device revoked at epoch 0 may be a
	// race (there's no head to date the client's view by). (Missing boxes
	// are allowed: see TestKeyRotateMissingBoxAllowed.)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1, c1), proto.ErrBadRequest)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1, a2), proto.ErrStaleEpoch)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1, newDevKeys(t)), proto.ErrBadRequest)
	// More than 128 boxes.
	rot(alice, ch.ID, hugeBundle(t, a1, sid, ch.ID), proto.ErrTooLarge)
	bob.noKind(proto.KKeyEpoch, 100*time.Millisecond)

	alice.rotate(ch.ID, ok1)
	bob.keyEpoch()
	// Epoch 2 must carry a backlink.
	noBack := rawOf(t, bundle(t, a1, sid, ch.ID, 2, &ok1, roster, a1, b1))
	noBack.Back = nil
	rot(alice, ch.ID, signRaw(t, a1, noBack), proto.ErrBadRequest)
	// CAS once the chain has a head: replay, skip, wrong prev.
	rot(alice, ch.ID, ok1, proto.ErrStaleEpoch)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1), proto.ErrStaleEpoch)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 3, &ok1, roster, a1, b1), proto.ErrStaleEpoch)
	other := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 2, &other, roster, a1, b1), proto.ErrStaleEpoch)

	// Once there's a head: a device revoked before it was visible to the
	// rotator (bad_request); one revoked after it is a race (stale_epoch).
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 2, &ok1, roster, a1, b1, a2), proto.ErrBadRequest)
	b3, _ := bob.registerDevice(ts)
	bob.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, bob.signer, sid, bob.user.ID, b3.ID)}, nil)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 2, &ok1, roster, a1, b1, b3), proto.ErrStaleEpoch)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 2, &ok1, roster, a1, b1, c1), proto.ErrBadRequest)

	// A disabled member leaves the roster.
	ok2 := bundle(t, a1, sid, ch.ID, 2, &ok1, append(slices.Clone(roster), dave.user.ID), a1, b1, d1)
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: dave.user.ID, Bundle: &ok2}, nil)
	bob.keyEpoch()
	alice.ok(proto.OpUserDisable, proto.UserArgs{User: dave.user.ID}, nil)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 3, &ok2, append(slices.Clone(roster), dave.user.ID), a1, b1), proto.ErrStaleEpoch)
	// Boxing the disabled member's device is a client error: not on the roster.
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 3, &ok2, roster, a1, b1, d1), proto.ErrBadRequest)
	bob.noKind(proto.KKeyEpoch, 100*time.Millisecond)
	alice.rotate(ch.ID, bundle(t, a1, sid, ch.ID, 3, &ok2, roster, a1, b1))
}

// rawBox and rawBundleB mirror EpochBundle.b, to sign bundles that
// identity.NewEpochBundle refuses to build.
type rawBox struct {
	Dev []byte `msgpack:"dev"`
	CT  []byte `msgpack:"ct"`
}

type rawBundleB struct {
	V        uint32   `msgpack:"v"`
	ServerID []byte   `msgpack:"server_id"`
	Chan     uint64   `msgpack:"chan"`
	Epoch    uint32   `msgpack:"epoch"`
	Prev     []byte   `msgpack:"prev"`
	Roster   []uint64 `msgpack:"roster"`
	By       []byte   `msgpack:"by"`
	Time     int64    `msgpack:"time"`
	KC       []byte   `msgpack:"kc"`
	Eph      []byte   `msgpack:"eph"`
	Boxes    []rawBox `msgpack:"boxes"`
	Back     []byte   `msgpack:"back,omitempty"`
}

// rawOf decodes a real bundle's b for tampering.
func rawOf(t *testing.T, s proto.Signed) rawBundleB {
	t.Helper()
	var w rawBundleB
	if err := proto.Unmarshal(s.B, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

// signRaw signs any b as an epoch bundle by device by.
func signRaw(t *testing.T, by *e2ecrypto.DeviceKeys, w any) proto.Signed {
	t.Helper()
	raw := proto.MustRaw(w)
	sig, err := e2ecrypto.SignEd25519(by.Ed25519, identity.CtxEpoch, raw)
	if err != nil {
		t.Fatal(err)
	}
	return proto.Signed{B: raw, Sig: sig}
}

// hugeBundle is a well-formed, signed bundle with 129 boxes.
func hugeBundle(t *testing.T, by *e2ecrypto.DeviceKeys, sid e2ecrypto.ServerID, ch uint64) proto.Signed {
	t.Helper()
	w := rawBundleB{V: 1, ServerID: sid[:], Chan: ch, Epoch: 1, Prev: make([]byte, 32), Roster: []uint64{1},
		By: by.ID[:], KC: make([]byte, 32), Eph: make([]byte, 32)}
	for i := range proto.MaxBundleBoxes + 1 {
		w.Boxes = append(w.Boxes, rawBox{Dev: append(make([]byte, 14), byte(i>>8), byte(i)), CT: make([]byte, 48)})
	}
	s := signRaw(t, by, w)
	if len(s.B)+len(s.Sig) > proto.MaxBundle {
		t.Fatal("huge bundle over the byte limit; it should fail on the box count")
	}
	return s
}

// Two members race to rotate from the same head: exactly one wins, and the
// loser's session gets the winning key.epoch before its stale_epoch RES.
func TestKeyRotateRace(t *testing.T) {
	ts := startServer(t, "tcp")
	advance := manualClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	alice.syncFresh()
	bob.syncFresh()
	sid := ts.sid()
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	a1, _ := alice.registerDevice(ts)
	b1, _ := bob.registerDevice(ts)
	roster := []uint64{alice.user.ID, bob.user.ID}
	head := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1)
	alice.rotate(ch.ID, head)
	alice.keyEpoch()
	bob.keyEpoch()

	wins := map[string]int{}
	for round := uint32(2); round < 12; round++ {
		advance(11 * time.Second) // past the per-user rate cap
		bundles := map[*tclient]proto.Signed{
			alice: bundle(t, a1, sid, ch.ID, round, &head, roster, a1, b1),
			bob:   bundle(t, b1, sid, ch.ID, round, &head, roster, a1, b1),
		}
		ids := map[*tclient]uint32{}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for c, b := range bundles {
			c.nextID++
			ids[c] = c.nextID
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				c.codec.Write(proto.TReq, proto.Req{ID: ids[c], Op: proto.OpKeyRotate,
					Args: proto.MustRaw(proto.KeyRotateArgs{Chan: ch.ID, Bundle: b})})
			}()
		}
		close(start)
		wg.Wait()

		results := map[*tclient]proto.Res{}
		for c := range bundles {
			results[c] = c.res(ids[c])
		}
		var winner, loser *tclient
		for c, r := range results {
			switch {
			case r.OK:
				if winner != nil {
					t.Fatalf("round %d: both rotations won", round)
				}
				winner = c
			case r.Err != nil && r.Err.Code == proto.ErrStaleEpoch:
				loser = c
			default:
				t.Fatalf("round %d: %s got %+v", round, c.user.Name, r.Err)
			}
		}
		if winner == nil || loser == nil {
			t.Fatalf("round %d: winner %v loser %v", round, winner, loser)
		}
		wins[winner.user.Name]++
		var won proto.SeqRes
		proto.Unmarshal(results[winner].Data, &won)

		// c.res held every frame that came before the RES: the loser's
		// held frames must already contain the winner's key.epoch.
		var seen bool
		for _, f := range loser.held {
			var ev proto.Event
			if f.Type == proto.TEvent && f.Decode(&ev) == nil && ev.Kind == proto.KKeyEpoch && ev.Seq == won.Seq {
				seen = true
			}
		}
		if !seen {
			t.Fatalf("round %d: %s's stale_epoch RES came before the winning key.epoch", round, loser.user.Name)
		}
		for _, c := range []*tclient{winner, loser} {
			if s, b := c.keyEpoch(); s != won.Seq || !bytes.Equal(b.B, bundles[winner].B) {
				t.Fatalf("round %d: %s got key.epoch %d, want %d", round, c.user.Name, s, won.Seq)
			}
		}
		head = bundles[winner]
	}
	t.Logf("wins: %v", wins)
}

func TestKeyRotateRateCap(t *testing.T) {
	ts := startServer(t, "tcp")
	advance := manualClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	carol := ts.registerCaps(t, "carol", false, e2eCaps)
	alice.syncFresh()
	bob.syncFresh()
	carol.syncFresh()
	sid := ts.sid()
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	a1, _ := alice.registerDevice(ts)
	b1, _ := bob.registerDevice(ts)
	roster := []uint64{alice.user.ID, bob.user.ID}

	e1 := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1)
	alice.rotate(ch.ID, e1)
	// Nothing changed: alice must wait 10 s; bob (another user) only for
	// the channel's 2 s.
	e2 := bundle(t, a1, sid, ch.ID, 2, &e1, roster, a1, b1)
	alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: e2}, proto.ErrRateLimited)
	e2b := bundle(t, b1, sid, ch.ID, 2, &e1, roster, a1, b1)
	advance(1 * time.Second)
	bob.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: e2b}, proto.ErrRateLimited)
	advance(8 * time.Second)
	alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: e2}, proto.ErrRateLimited)
	e2 = e2b
	bob.rotate(ch.ID, e2)
	e3 := bundle(t, b1, sid, ch.ID, 3, &e2, roster, a1, b1)
	bob.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: e3}, proto.ErrRateLimited)
	advance(1 * time.Second) // alice's 10 s are up, the channel's 2 s aren't
	e3 = bundle(t, a1, sid, ch.ID, 3, &e2, roster, a1, b1)
	alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: e3}, proto.ErrRateLimited)
	advance(1 * time.Second)
	alice.rotate(ch.ID, e3)

	// A new device changes the device set: no wait, boxed or not.
	a3, _ := alice.registerDevice(ts)
	e4 := bundle(t, a1, sid, ch.ID, 4, &e3, roster, a1, b1)
	alice.rotate(ch.ID, e4)
	// A revoked device too.
	alice.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, alice.signer, sid, alice.user.ID, a3.ID)}, nil)
	e5 := bundle(t, a1, sid, ch.ID, 5, &e4, roster, a1, b1)
	alice.rotate(ch.ID, e5)
	// And a roster change (the add carries the next epoch).
	roster = []uint64{alice.user.ID, bob.user.ID, carol.user.ID}
	e6 := bundle(t, a1, sid, ch.ID, 6, &e5, roster, a1, b1)
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: carol.user.ID, Bundle: &e6}, nil)
	e7 := bundle(t, a1, sid, ch.ID, 7, &e6, roster, a1, b1)
	alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: e7}, proto.ErrRateLimited)
	advance(10 * time.Second)
	alice.rotate(ch.ID, e7)
}

// The cap's exemption follows the server's record of the roster and
// devices, not what bundles box: a member with a second device that is
// never boxed can't use it to rotate without limit.
func TestKeyRotateRateCapUnboxedDevice(t *testing.T) {
	ts := startServer(t, "tcp")
	advance := manualClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	alice.syncFresh()
	bob.syncFresh()
	sid := ts.sid()
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	a1, _ := alice.registerDevice(ts)
	alice.registerDevice(ts) // a2: never boxed
	b1, _ := bob.registerDevice(ts)
	roster := []uint64{alice.user.ID, bob.user.ID}
	head := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1)
	alice.rotate(ch.ID, head)
	for range 3 {
		advance(3 * time.Second) // past the channel cap, not alice's
		next := bundle(t, a1, sid, ch.ID, 2, &head, roster, a1, b1)
		alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: next}, proto.ErrRateLimited)
	}
	// A real change still goes through at once.
	b2, _ := bob.registerDevice(ts)
	alice.rotate(ch.ID, bundle(t, a1, sid, ch.ID, 2, &head, roster, a1, b1, b2))
}

func TestKeyFetch(t *testing.T) {
	ts := startServer(t, "tcp")
	advance := manualClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	carol := ts.registerCaps(t, "carol", false, e2eCaps)
	for _, c := range []*tclient{alice, bob, carol} {
		c.syncFresh()
	}
	sid := ts.sid()
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	a1, _ := alice.registerDevice(ts)
	roster := []uint64{alice.user.ID, bob.user.ID}
	var chain []proto.Signed
	var seqs []uint64
	var prev *proto.Signed
	for ep := uint32(1); ep <= 3; ep++ {
		advance(rotateEveryMs * time.Millisecond)
		b := bundle(t, a1, sid, ch.ID, ep, prev, roster, a1)
		seqs = append(seqs, alice.rotate(ch.ID, b))
		chain = append(chain, b)
		prev = &chain[len(chain)-1]
	}
	fetchRes := func(c *tclient, from, to uint32) proto.KeyFetchRes {
		t.Helper()
		var r proto.KeyFetchRes
		c.ok(proto.OpKeyFetch, proto.KeyFetchArgs{Chan: ch.ID, From: from, To: to}, &r)
		if len(r.Seqs) != len(r.Bundles) {
			t.Fatalf("%d seqs for %d bundles", len(r.Seqs), len(r.Bundles))
		}
		return r
	}
	same := func(got proto.KeyFetchRes, want []proto.Signed, wantSeqs []uint64) {
		t.Helper()
		if len(got.Bundles) != len(want) {
			t.Fatalf("got %d bundles, want %d", len(got.Bundles), len(want))
		}
		for i := range want {
			if !bytes.Equal(got.Bundles[i].B, want[i].B) || !bytes.Equal(got.Bundles[i].Sig, want[i].Sig) {
				t.Fatalf("bundle %d differs", i)
			}
		}
		// Each bundle's seq is its key.epoch event's.
		if !slices.Equal(got.Seqs, wantSeqs) {
			t.Fatalf("seqs %v, want %v", got.Seqs, wantSeqs)
		}
	}
	same(fetchRes(bob, 1, 3), chain, seqs)
	same(fetchRes(bob, 2, 2), chain[1:2], seqs[1:2])
	same(fetchRes(bob, 2, 256), chain[1:], seqs[1:])
	same(fetchRes(alice, 4, 9), nil, []uint64{})
	for _, r := range [][2]uint32{{0, 3}, {3, 2}, {1, 257}, {10, 10 + proto.MaxKeyFetch}} {
		bob.fail(proto.OpKeyFetch, proto.KeyFetchArgs{Chan: ch.ID, From: r[0], To: r[1]}, proto.ErrBadRequest)
	}
	carol.fail(proto.OpKeyFetch, proto.KeyFetchArgs{Chan: ch.ID, From: 1, To: 3}, proto.ErrForbidden)
	carol.fail(proto.OpKeyFetch, proto.KeyFetchArgs{Chan: 999999, From: 1, To: 3}, proto.ErrForbidden)

	// Bulky bundles straight into the store: a fetch stops under the frame
	// limit and the client continues from the last epoch + 1.
	for ep := 4; ep <= 100; ep++ {
		blob := proto.MustRaw(proto.Signed{B: make([]byte, proto.MaxBundle-100), Sig: []byte{byte(ep)}})
		if _, err := ts.st.db.Exec(`INSERT INTO epochs (chan, epoch, bundle, hash, by_user, time) VALUES (?, ?, ?, ?, ?, ?)`,
			ch.ID, ep, blob, make([]byte, 32), alice.user.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	got, from, calls := 0, uint32(1), 0
	for from <= 100 {
		r := fetchRes(bob, from, 100)
		bs := r.Bundles
		calls++
		if len(bs) == 0 || encodedSize(r) > proto.MaxResultBytes {
			t.Fatalf("fetch from %d: %d bundles, %d bytes", from, len(bs), encodedSize(r))
		}
		got += len(bs)
		from += uint32(len(bs))
	}
	if got != 100 || calls < 3 {
		t.Fatalf("fetched %d bundles in %d calls", got, calls)
	}
	bob.ok(proto.OpChanLeave, proto.ChanArgs{Chan: ch.ID}, nil)
	bob.fail(proto.OpKeyFetch, proto.KeyFetchArgs{Chan: ch.ID, From: 1, To: 3}, proto.ErrForbidden)
}

// Snapshots carry the latest bundle of each e2e channel the user is in, and
// only for e2e1 sessions.
func TestSnapshotKeys(t *testing.T) {
	ts := startServer(t, "tcp")
	advance := manualClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	carol := ts.registerCaps(t, "carol", false, e2eCaps)
	for _, c := range []*tclient{alice, bob, carol} {
		c.syncFresh()
	}
	sid := ts.sid()
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	alice.createE2E("empty") // epoch 0: no bundle
	var dm proto.ChanRes
	alice.ok(proto.OpDMOpen, proto.DMOpenArgs{Users: []uint64{bob.user.ID}}, &dm)
	a1, _ := alice.registerDevice(ts)
	roster := []uint64{alice.user.ID, bob.user.ID}
	e1 := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1)
	alice.rotate(ch.ID, e1)
	advance(rotateEveryMs * time.Millisecond)
	e2 := bundle(t, a1, sid, ch.ID, 2, &e1, roster, a1)
	s2 := alice.rotate(ch.ID, e2)
	d1 := bundle(t, a1, sid, dm.Chan.ID, 1, nil, roster, a1)
	sd1 := alice.rotate(dm.Chan.ID, d1)

	c, _ := ts.connectCaps(t, bob.signer, "", "", e2eCaps)
	snap, _ := c.syncFresh()
	if len(snap.Keys) != 2 || !bytes.Equal(snap.Keys[0].Bundle.B, e2.B) || !bytes.Equal(snap.Keys[1].Bundle.B, d1.B) {
		t.Fatalf("bob's snapshot keys: %d", len(snap.Keys))
	}
	if snap.Keys[0].Seq != s2 || snap.Keys[1].Seq != sd1 {
		t.Fatalf("snapshot key seqs %d %d, want %d %d", snap.Keys[0].Seq, snap.Keys[1].Seq, s2, sd1)
	}
	for _, sc := range snap.Chans {
		if sc.ID == ch.ID && (!sc.E2E || sc.Epoch != 2) || sc.ID == dm.Chan.ID && (!sc.E2E || sc.Epoch != 1) {
			t.Fatalf("snapshot channel %+v", sc)
		}
	}
	if s, _ := bob.syncFreshAgain(t, ts); len(s.Keys) != 0 {
		t.Fatal("old snapshot has keys")
	}
	c2, _ := ts.connectCaps(t, carol.signer, "", "", e2eCaps)
	if s, _ := c2.syncFresh(); len(s.Keys) != 0 {
		t.Fatal("non-member snapshot has keys")
	}
}

// key.epoch follows membership at its seq, live and on replay alike: after
// chan.join the joiner sees it, after chan.leave the leaver doesn't, and
// sessions without e2e1 never do.
func TestKeyEpochVisibility(t *testing.T) {
	forEachTransport(t, func(t *testing.T, ts *testServer) {
		fastClock(ts)
		alice := ts.registerCaps(t, "alice", true, e2eCaps)
		bob := ts.registerCaps(t, "bob", false, e2eCaps)
		carol := ts.registerCaps(t, "carol", false, e2eCaps)
		old := ts.register(t, "old", false)
		var since uint64
		for _, c := range []*tclient{alice, bob, carol, old} {
			_, sr := c.syncFresh()
			since = max(since, sr.Head)
		}
		sid := ts.sid()
		ch := alice.createE2E("sec")
		alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: old.user.ID}, nil)
		a1, _ := alice.registerDevice(ts)
		b1, _ := bob.registerDevice(ts)
		live := map[*tclient][]uint64{}

		r1 := []uint64{alice.user.ID, old.user.ID}
		e1 := bundle(t, a1, sid, ch.ID, 1, nil, r1, a1)
		live[alice] = append(live[alice], alice.rotate(ch.ID, e1))
		alice.keyEpoch()

		r2 := []uint64{alice.user.ID, bob.user.ID, old.user.ID}
		e2 := bundle(t, a1, sid, ch.ID, 2, &e1, r2, a1, b1)
		alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID, Bundle: &e2}, nil)
		bob.event(proto.KChanJoin)
		s2, _ := alice.keyEpoch()
		live[alice] = append(live[alice], s2)
		if s, _ := bob.keyEpoch(); s != s2 {
			t.Fatalf("bob got key.epoch %d, want %d", s, s2)
		}
		live[bob] = append(live[bob], s2)

		bob.ok(proto.OpChanLeave, proto.ChanArgs{Chan: ch.ID}, nil)
		bob.event(proto.KChanLeave)
		alice.event(proto.KChanLeave)
		e3 := bundle(t, a1, sid, ch.ID, 3, &e2, r1, a1)
		live[alice] = append(live[alice], alice.rotate(ch.ID, e3))
		alice.keyEpoch()
		bob.noKind(proto.KKeyEpoch, 100*time.Millisecond)
		carol.noKind(proto.KKeyEpoch, 50*time.Millisecond)
		old.noKind(proto.KKeyEpoch, 50*time.Millisecond)

		for _, c := range []*tclient{alice, bob, carol} {
			got := keyEpochSeqs(c.replayCaps(t, ts, since, e2eCaps))
			if !slices.Equal(got, live[c]) {
				t.Fatalf("%s: replayed key.epoch %v, live %v", c.user.Name, got, live[c])
			}
		}
		for _, c := range []*tclient{alice, old} { // without e2e1: none
			if got := keyEpochSeqs(c.replayCaps(t, ts, since, nil)); len(got) != 0 {
				t.Fatalf("%s without e2e1 replayed %v", c.user.Name, got)
			}
		}
	})
}

func TestE2EChannelCreation(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	old := ts.register(t, "old", false)
	for _, c := range []*tclient{alice, bob, old} {
		c.syncFresh()
	}
	alice.fail(proto.OpChanCreate, proto.ChanCreateArgs{Name: "pub", E2E: true}, proto.ErrBadRequest)
	old.fail(proto.OpChanCreate, proto.ChanCreateArgs{Name: "x", Private: true, E2E: true}, proto.ErrBadRequest)
	alice.createE2E("sec")
	var r proto.ChanRes
	// Fresh results each time: omitted fields don't overwrite a reused struct.
	open := func(c *tclient, users ...uint64) proto.Channel {
		t.Helper()
		var r proto.ChanRes
		c.ok(proto.OpDMOpen, proto.DMOpenArgs{Users: users}, &r)
		return r.Chan
	}
	alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "plain", Private: true}, &r)
	if r.Chan.E2E {
		t.Fatal("plain private channel is e2e")
	}
	// The chan.new event carries the flag too.
	ev := alice.event(proto.KChanNew)
	var c proto.Channel
	proto.Unmarshal(ev.Data, &c)
	if c.Name != "sec" || !c.E2E {
		t.Fatalf("chan.new %+v", c)
	}

	// dm.open: encrypted iff the caller speaks e2e1 (PROTOCOL §13.3), at epoch 0.
	if c := open(alice, bob.user.ID); !c.E2E || c.Epoch != 0 {
		t.Fatalf("e2e1 DM %+v", c)
	}
	// Private notes stay plaintext: the server writes reminders into them.
	if c := open(alice); c.E2E {
		t.Fatalf("notes %+v", c)
	}
	if c := open(old, bob.user.ID); c.E2E {
		t.Fatalf("old client's DM is e2e: %+v", c)
	}
	// An existing DM is returned as it is.
	if c := open(bob, old.user.ID); c.E2E {
		t.Fatal("reopening changed the DM")
	}
	oldDM := open(old, alice.user.ID)
	if c := open(alice, old.user.ID); c.E2E || c.ID != oldDM.ID {
		t.Fatal("reopening changed the DM")
	}
	if c := open(old, bob.user.ID, alice.user.ID); c.E2E {
		t.Fatal("old client's group DM is e2e")
	}
	if c := open(bob, old.user.ID, alice.user.ID); c.E2E {
		t.Fatal("reopening changed the group DM")
	}

	// Old clients still chat in DMs: nothing about this step changes sends.
	nonce := make([]byte, 16)
	rand.Read(nonce)
	old.ok(proto.OpSend, proto.SendArgs{Chan: open(alice, old.user.ID).ID, Body: "hi", Nonce: nonce}, nil)
}

func TestEpochStale(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	ctx := context.Background()
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	carol := ts.registerCaps(t, "carol", false, e2eCaps)
	snap, _ := alice.syncFresh()
	bob.syncFresh()
	carol.syncFresh()
	sid := ts.sid()
	stale := func(ch uint64, want bool) {
		t.Helper()
		got, err := ts.EpochStale(ctx, ch)
		if err != nil || got != want {
			t.Fatalf("EpochStale(%d) = %v, %v; want %v", ch, got, err, want)
		}
	}
	stale(generalID(t, snap), false)
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	stale(ch.ID, true) // epoch 0
	a1, _ := alice.registerDevice(ts)
	b1, _ := bob.registerDevice(ts)
	roster := []uint64{alice.user.ID, bob.user.ID}
	e1 := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1)
	alice.rotate(ch.ID, e1)
	stale(ch.ID, false)
	b2, _ := bob.registerDevice(ts) // new devices get the key by key.share, not a rotation
	stale(ch.ID, false)
	bob.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, bob.signer, sid, bob.user.ID, b1.ID)}, nil)
	stale(ch.ID, true) // boxes a revoked device
	e2 := bundle(t, a1, sid, ch.ID, 2, &e1, roster, a1, b2)
	alice.rotate(ch.ID, e2)
	stale(ch.ID, false)
	roster = []uint64{alice.user.ID, bob.user.ID, carol.user.ID}
	e3 := bundle(t, a1, sid, ch.ID, 3, &e2, roster, a1, b2)
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: carol.user.ID, Bundle: &e3}, nil)
	stale(ch.ID, false) // the add carried the next epoch
	alice.ok(proto.OpUserDisable, proto.UserArgs{User: carol.user.ID}, nil)
	stale(ch.ID, true) // a member was disabled
	alice.rotate(ch.ID, bundle(t, a1, sid, ch.ID, 4, &e3, []uint64{alice.user.ID, bob.user.ID}, a1, b2))
	stale(ch.ID, false)
	bob.ok(proto.OpChanLeave, proto.ChanArgs{Chan: ch.ID}, nil)
	stale(ch.ID, true) // a member left
}

// Clients seal only to devices they've verified, so a bundle may skip some
// member devices (they get the key via key.share), but not every device of
// a member; it may never box a device outside the members' active devices.
func TestKeyRotateMissingBoxAllowed(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	carol := ts.registerCaps(t, "carol", false, e2eCaps)
	alice.syncFresh()
	bob.syncFresh()
	carol.syncFresh()
	sid := ts.sid()
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: carol.user.ID}, nil)
	a1, _ := alice.registerDevice(ts)
	alice.registerDevice(ts) // a2, not verified by the rotator yet
	bob.registerDevice(ts)   // b1
	// carol has no device: nothing to box.
	roster := []uint64{alice.user.ID, bob.user.ID, carol.user.ID}
	// bob has a device but no box. At epoch 1 there is no head the rotator
	// must have seen bob's device by: it may be racing its device.new.
	alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: bundle(t, a1, sid, ch.ID, 1, nil, roster, a1)}, proto.ErrStaleEpoch)
	b2, _ := bob.registerDevice(ts)
	// One box per user is enough: a2 and b1 get K via key.share.
	e1 := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b2)
	alice.ok(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: e1}, nil)

	// carol's first device registers after epoch 1: a bundle built before
	// its device.new reached the rotator is a race (#127), and the retry
	// that boxes it succeeds.
	c1, _ := carol.registerDevice(ts)
	alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: bundle(t, a1, sid, ch.ID, 2, &e1, roster, a1, b2)}, proto.ErrStaleEpoch)
	// bob's devices predate epoch 1: leaving him out is the rotator's error.
	alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: bundle(t, a1, sid, ch.ID, 2, &e1, roster, a1, c1)}, proto.ErrBadRequest)
	// Even when carol is left out too: bob's case can't be a race.
	alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: bundle(t, a1, sid, ch.ID, 2, &e1, roster, a1)}, proto.ErrBadRequest)
	alice.ok(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: bundle(t, a1, sid, ch.ID, 2, &e1, roster, a1, b2, c1)}, nil)
}

// A device stored before devices.created_seq existed (NULL) counts as
// old: leaving its user out is never a race.
func TestKeyRotateMissingBoxLegacyDevice(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	alice.syncFresh()
	bob.syncFresh()
	sid := ts.sid()
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	a1, _ := alice.registerDevice(ts)
	b1, _ := bob.registerDevice(ts)
	if _, err := ts.st.db.Exec(`UPDATE devices SET created_seq = NULL WHERE dev = ?`, b1.ID[:]); err != nil {
		t.Fatal(err)
	}
	roster := []uint64{alice.user.ID, bob.user.ID}
	alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: bundle(t, a1, sid, ch.ID, 1, nil, roster, a1)}, proto.ErrBadRequest)
	alice.ok(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1)}, nil)
}

// Databases from before epochs.seq / devices.revoked_seq get them from events.
func TestBackfillSeqs(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	alice.syncFresh()
	sid := ts.sid()
	ch := alice.createE2E("sec")
	a1, _ := alice.registerDevice(ts)
	roster := []uint64{alice.user.ID}
	e1 := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1)
	s1 := alice.rotate(ch.ID, e1)
	a2, _ := alice.registerDevice(ts) // a change: no rate cap
	s2 := alice.rotate(ch.ID, bundle(t, a1, sid, ch.ID, 2, &e1, roster, a1, a2))
	var rs proto.SeqRes
	alice.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, alice.signer, sid, alice.user.ID, a2.ID)}, &rs)

	db := ts.st.db
	if _, err := db.Exec(`UPDATE epochs SET seq = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE devices SET revoked_seq = NULL`); err != nil {
		t.Fatal(err)
	}
	if err := backfillSeqs(db); err != nil {
		t.Fatal(err)
	}
	var got1, got2 uint64
	db.QueryRow(`SELECT seq FROM epochs WHERE chan = ? AND epoch = 1`, ch.ID).Scan(&got1)
	db.QueryRow(`SELECT seq FROM epochs WHERE chan = ? AND epoch = 2`, ch.ID).Scan(&got2)
	if got1 != s1 || got2 != s2 {
		t.Fatalf("epoch seqs %d %d, want %d %d", got1, got2, s1, s2)
	}
	var rev sql.NullInt64
	var live sql.NullInt64
	db.QueryRow(`SELECT revoked_seq FROM devices WHERE dev = ?`, a2.ID[:]).Scan(&rev)
	db.QueryRow(`SELECT revoked_seq FROM devices WHERE dev = ?`, a1.ID[:]).Scan(&live)
	if !rev.Valid || uint64(rev.Int64) != rs.Seq || live.Valid {
		t.Fatalf("revoked_seq %v (want %d), active %v", rev, rs.Seq, live)
	}
}

// Only a previous-roster user may extend the chain; epoch 1 only the
// creator (any member of a DM).
func TestKeyRotateAuthorization(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	carol := ts.registerCaps(t, "carol", false, e2eCaps)
	snap, _ := alice.syncFresh()
	bob.syncFresh()
	carol.syncFresh()
	sid := ts.sid()
	a1, _ := alice.registerDevice(ts)
	b1, _ := bob.registerDevice(ts)
	c1, _ := carol.registerDevice(ts)

	ch := alice.createE2E("sec")
	if ch.Creator != alice.user.ID {
		t.Fatalf("creator %d, want %d", ch.Creator, alice.user.ID)
	}
	for _, c := range snap.Chans {
		if c.Name == "general" && c.Creator != 0 {
			t.Fatalf("#general creator %d", c.Creator)
		}
	}
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	r1 := []uint64{alice.user.ID, bob.user.ID}
	bob.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: bundle(t, b1, sid, ch.ID, 1, nil, r1, a1, b1)}, proto.ErrForbidden)
	e1 := bundle(t, a1, sid, ch.ID, 1, nil, r1, a1, b1)
	alice.rotate(ch.ID, e1)

	// carol is a member who isn't on epoch 1's roster (as after a chan.add
	// from before adds carried bundles), so she can't sign epoch 2.
	ts.mu.Lock()
	if _, err := ts.st.db.Exec(`INSERT INTO membership (chan, user, join_seq) VALUES (?, ?, 1)`, ch.ID, carol.user.ID); err != nil {
		t.Fatal(err)
	}
	ts.members[ch.ID][carol.user.ID] = true
	ts.mu.Unlock()
	r2 := []uint64{alice.user.ID, bob.user.ID, carol.user.ID}
	carol.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: bundle(t, c1, sid, ch.ID, 2, &e1, r2, a1, b1, c1)}, proto.ErrForbidden)
	e2 := bundle(t, b1, sid, ch.ID, 2, &e1, r2, a1, b1, c1)
	bob.rotate(ch.ID, e2)
	// Now she is.
	carol.rotate(ch.ID, bundle(t, c1, sid, ch.ID, 3, &e2, r2, a1, b1, c1))

	// DMs have no single owner: anyone in it may create epoch 1.
	var dm proto.ChanRes
	alice.ok(proto.OpDMOpen, proto.DMOpenArgs{Users: []uint64{bob.user.ID}}, &dm)
	if dm.Chan.Creator != alice.user.ID {
		t.Fatalf("DM creator %d", dm.Chan.Creator)
	}
	bob.rotate(dm.Chan.ID, bundle(t, b1, sid, dm.Chan.ID, 1, nil, r1, a1, b1))

	// The creator is returned everywhere a Channel is.
	c, err := getChannel(context.Background(), ts.st.db, ch.ID)
	if err != nil || c.Creator != alice.user.ID {
		t.Fatalf("stored creator %+v %v", c, err)
	}
	c2, _ := ts.connectCaps(t, carol.signer, "", "", e2eCaps)
	s2, _ := c2.syncFresh()
	var found bool
	for _, sc := range s2.Chans {
		if sc.ID == ch.ID {
			found = sc.Creator == alice.user.ID
		}
	}
	if !found {
		t.Fatal("snapshot channel lacks creator")
	}
}

// channels.creator is backfilled from chan.new for databases from before it.
func TestMigrateCreator(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	alice.syncFresh()
	ch := alice.createE2E("sec")
	db := ts.st.db
	if _, err := db.Exec(`ALTER TABLE channels DROP COLUMN creator`); err != nil {
		t.Fatal(err)
	}
	if err := migrateCreator(db); err != nil {
		t.Fatal(err)
	}
	var creator, general uint64
	db.QueryRow(`SELECT creator FROM channels WHERE id = ?`, ch.ID).Scan(&creator)
	db.QueryRow(`SELECT creator FROM channels WHERE name = 'general'`).Scan(&general)
	if creator != alice.user.ID || general != 0 {
		t.Fatalf("backfilled creator %d, general %d", creator, general)
	}
	if err := migrateCreator(db); err != nil { // idempotent
		t.Fatal(err)
	}
}

// EpochStale runs on every encrypted send (step 7): its queries must use
// indexes, never scan devices or membership.
func TestEpochStaleQueryPlans(t *testing.T) {
	ts := startServer(t, "tcp")
	plan := func(q string, args ...any) string {
		t.Helper()
		rows, err := ts.st.db.Query("EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			out = append(out, detail)
		}
		return strings.Join(out, "; ")
	}
	for _, c := range []struct {
		q    string
		args []any
	}{
		{revokedBoxesQuery(1), []any{[]byte{1}}},
		{revokedBoxesQuery(proto.MaxBundleBoxes), make([]any, proto.MaxBundleBoxes)},
		{rosterQuery, []any{1}},
		{revokedSinceQuery, []any{1, 0}},
	} {
		p := plan(c.q, c.args...)
		if strings.Contains(p, "SCAN devices") || strings.Contains(p, "SCAN m") || strings.Contains(p, "SCAN membership") {
			t.Errorf("full scan in %q: %s", c.q, p)
		}
		t.Log(p)
	}
}

// A revoke whose seq can't be backfilled is left out of devices.list and the
// snapshot: clients read seq 0 as "unknown", so the server never sends it.
func TestRevokeWithoutSeqOmitted(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	alice.syncFresh()
	sid := ts.sid()
	_, _ = alice.registerDevice(ts)
	a2, _ := alice.registerDevice(ts)
	alice.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, alice.signer, sid, alice.user.ID, a2.ID)}, nil)
	db := ts.st.db
	if _, err := db.Exec(`UPDATE devices SET revoked_seq = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM events WHERE kind = ?`, proto.KDeviceRevoke); err != nil {
		t.Fatal(err)
	}
	if err := backfillSeqs(db); err != nil {
		t.Fatal(err)
	}
	var list proto.DevicesListRes
	alice.ok(proto.OpDevicesList, proto.DevicesListArgs{}, &list)
	if len(list.Certs) != 2 || len(list.Revoked) != 0 || len(list.RevokedSeqs) != 0 {
		t.Fatalf("list: %d certs, revoked %d, seqs %v", len(list.Certs), len(list.Revoked), list.RevokedSeqs)
	}
	again, bye := ts.connectCaps(t, alice.signer, "", "", e2eCaps)
	if bye != "" {
		t.Fatal(bye)
	}
	sn, _ := again.syncFresh()
	if len(sn.Devs) != 2 || len(sn.Revoked) != 0 || slices.Contains(sn.RevokedSeqs, 0) {
		t.Fatalf("snapshot: %d devs, revoked %d, seqs %v", len(sn.Devs), len(sn.Revoked), sn.RevokedSeqs)
	}
}

// A rotation by a user disabled after dispatch checked them (in flight)
// doesn't commit: they'd hold the new K though no roster includes them
// (step 12 security pass). Called below dispatch to hit that window.
func TestKeyRotateDisabledInFlight(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	alice.syncFresh()
	bob.syncFresh()
	sid := ts.sid()
	a1, _ := alice.registerDevice(ts)
	b1, _ := bob.registerDevice(ts)
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	e1 := bundle(t, a1, sid, ch.ID, 1, nil, []uint64{alice.user.ID, bob.user.ID}, a1, b1)
	alice.rotate(ch.ID, e1)
	alice.ok(proto.OpUserDisable, proto.UserArgs{User: bob.user.ID}, nil)

	_, err := ts.opKeyRotate(context.Background(), bob.user,
		proto.KeyRotateArgs{Chan: ch.ID, Bundle: bundle(t, b1, sid, ch.ID, 2, &e1, []uint64{alice.user.ID}, a1)})
	if err == nil {
		t.Fatal("a disabled user's rotation committed")
	}
	var cur uint32
	if err := ts.st.db.QueryRow(`SELECT epoch FROM channels WHERE id = ?`, ch.ID).Scan(&cur); err != nil || cur != 1 {
		t.Fatalf("epoch %d %v", cur, err)
	}
	// The admin's own rotation goes through.
	alice.rotate(ch.ID, bundle(t, a1, sid, ch.ID, 2, &e1, []uint64{alice.user.ID}, a1))
}

// B5: a device registered after the current epoch gets K by key.share, not
// a box. Revoking it makes the epoch stale all the same, and the rotation
// away from it isn't rate capped although the roster's active devices are
// what they were when the epoch was made.
func TestEpochStaleAfterSharedDeviceRevoked(t *testing.T) {
	ts := startServer(t, "tcp")
	ctx := context.Background()
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	alice.syncFresh()
	bob.syncFresh()
	sid := ts.sid()
	stale := func(ch uint64, want bool) {
		t.Helper()
		got, err := ts.EpochStale(ctx, ch)
		if err != nil || got != want {
			t.Fatalf("EpochStale(%d) = %v, %v; want %v", ch, got, err, want)
		}
	}
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	a1, _ := alice.registerDevice(ts)
	b1, _ := bob.registerDevice(ts)
	roster := []uint64{alice.user.ID, bob.user.ID}
	e1 := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1)
	alice.rotate(ch.ID, e1)
	b2, _ := bob.registerDevice(ts) // gets K by key.share
	stale(ch.ID, false)
	bob.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, bob.signer, sid, bob.user.ID, b2.ID)}, nil)
	stale(ch.ID, true)
	e2 := bundle(t, a1, sid, ch.ID, 2, &e1, roster, a1, b1)
	alice.rotate(ch.ID, e2) // within the 2 s channel cap: allowed
	stale(ch.ID, false)
	alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: bundle(t, a1, sid, ch.ID, 3, &e2, roster, a1, b1)},
		proto.ErrRateLimited) // nothing changed since: capped again
}
