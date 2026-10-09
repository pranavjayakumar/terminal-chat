package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"slices"
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
	rot(alice, plain.Chan.ID, ok1, proto.ErrBadRequest) // bundle.chan != chan
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
	// Roster: missing, extra, or holding a disabled member.
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, []uint64{alice.user.ID}, a1, b1), proto.ErrStaleEpoch)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, append(slices.Clone(roster), carol.user.ID), a1, b1), proto.ErrStaleEpoch)
	// Boxes: missing, extra, or to a revoked device.
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, roster, a1), proto.ErrStaleEpoch)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1, c1), proto.ErrStaleEpoch)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1, a2), proto.ErrStaleEpoch)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1, newDevKeys(t)), proto.ErrStaleEpoch)
	// More than 128 boxes.
	rot(alice, ch.ID, hugeBundle(t, a1, sid, ch.ID), proto.ErrTooLarge)
	bob.noKind(proto.KKeyEpoch, 100*time.Millisecond)

	alice.rotate(ch.ID, ok1)
	bob.keyEpoch()
	// CAS once the chain has a head: replay, skip, wrong prev.
	rot(alice, ch.ID, ok1, proto.ErrStaleEpoch)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1), proto.ErrStaleEpoch)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 3, &ok1, roster, a1, b1), proto.ErrStaleEpoch)
	other := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 2, &other, roster, a1, b1), proto.ErrStaleEpoch)

	// A disabled member leaves the roster.
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: dave.user.ID}, nil)
	alice.ok(proto.OpUserDisable, proto.UserArgs{User: dave.user.ID}, nil)
	rot(alice, ch.ID, bundle(t, a1, sid, ch.ID, 2, &ok1, append(slices.Clone(roster), dave.user.ID), a1, b1), proto.ErrStaleEpoch)
	bob.noKind(proto.KKeyEpoch, 100*time.Millisecond)
	alice.rotate(ch.ID, bundle(t, a1, sid, ch.ID, 2, &ok1, roster, a1, b1))
}

// hugeBundle is a well-formed, signed bundle with 129 boxes.
func hugeBundle(t *testing.T, by *e2ecrypto.DeviceKeys, sid e2ecrypto.ServerID, ch uint64) proto.Signed {
	t.Helper()
	type box struct {
		Dev []byte `msgpack:"dev"`
		CT  []byte `msgpack:"ct"`
	}
	type b struct {
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
		Boxes    []box    `msgpack:"boxes"`
	}
	w := b{V: 1, ServerID: sid[:], Chan: ch, Epoch: 1, Prev: make([]byte, 32), Roster: []uint64{1},
		By: by.ID[:], KC: make([]byte, 32), Eph: make([]byte, 32)}
	for i := range proto.MaxBundleBoxes + 1 {
		w.Boxes = append(w.Boxes, box{Dev: append(make([]byte, 14), byte(i>>8), byte(i)), CT: make([]byte, 48)})
	}
	raw := proto.MustRaw(w)
	sig, _ := e2ecrypto.SignEd25519(by.Ed25519, identity.CtxEpoch, raw)
	s := proto.Signed{B: raw, Sig: sig}
	if len(raw)+len(sig) > proto.MaxBundle {
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
	// Nothing changed: alice must wait 10 s, bob (another user) needn't.
	e2 := bundle(t, a1, sid, ch.ID, 2, &e1, roster, a1, b1)
	alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: e2}, proto.ErrRateLimited)
	advance(9 * time.Second)
	alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: e2}, proto.ErrRateLimited)
	e2 = bundle(t, b1, sid, ch.ID, 2, &e1, roster, a1, b1)
	bob.rotate(ch.ID, e2)
	e3 := bundle(t, b1, sid, ch.ID, 3, &e2, roster, a1, b1)
	bob.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: e3}, proto.ErrRateLimited)
	advance(1 * time.Second)
	e3 = bundle(t, a1, sid, ch.ID, 3, &e2, roster, a1, b1)
	alice.rotate(ch.ID, e3)

	// A new device changes the device set: no wait.
	a3, _ := alice.registerDevice(ts)
	e4 := bundle(t, a1, sid, ch.ID, 4, &e3, roster, a1, b1, a3)
	alice.rotate(ch.ID, e4)
	// A revoked device too.
	alice.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, alice.signer, sid, alice.user.ID, a3.ID)}, nil)
	e5 := bundle(t, a1, sid, ch.ID, 5, &e4, roster, a1, b1)
	alice.rotate(ch.ID, e5)
	// And a roster change.
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: carol.user.ID}, nil)
	roster = []uint64{alice.user.ID, bob.user.ID, carol.user.ID}
	e6 := bundle(t, a1, sid, ch.ID, 6, &e5, roster, a1, b1)
	alice.rotate(ch.ID, e6)
	e7 := bundle(t, a1, sid, ch.ID, 7, &e6, roster, a1, b1)
	alice.fail(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch.ID, Bundle: e7}, proto.ErrRateLimited)
	advance(10 * time.Second)
	alice.rotate(ch.ID, e7)
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
	var prev *proto.Signed
	for ep := uint32(1); ep <= 3; ep++ {
		advance(rotateEveryMs * time.Millisecond)
		b := bundle(t, a1, sid, ch.ID, ep, prev, roster, a1)
		alice.rotate(ch.ID, b)
		chain = append(chain, b)
		prev = &chain[len(chain)-1]
	}
	fetch := func(c *tclient, from, to uint32) []proto.Signed {
		t.Helper()
		var r proto.KeyFetchRes
		c.ok(proto.OpKeyFetch, proto.KeyFetchArgs{Chan: ch.ID, From: from, To: to}, &r)
		return r.Bundles
	}
	same := func(got, want []proto.Signed) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %d bundles, want %d", len(got), len(want))
		}
		for i := range got {
			if !bytes.Equal(got[i].B, want[i].B) || !bytes.Equal(got[i].Sig, want[i].Sig) {
				t.Fatalf("bundle %d differs", i)
			}
		}
	}
	same(fetch(bob, 1, 3), chain)
	same(fetch(bob, 2, 2), chain[1:2])
	same(fetch(bob, 2, 256), chain[1:])
	same(fetch(alice, 4, 9), nil)
	for _, r := range [][2]uint32{{0, 3}, {3, 2}, {1, 257}, {10, 10 + proto.MaxKeyFetch}} {
		bob.fail(proto.OpKeyFetch, proto.KeyFetchArgs{Chan: ch.ID, From: r[0], To: r[1]}, proto.ErrBadRequest)
	}
	carol.fail(proto.OpKeyFetch, proto.KeyFetchArgs{Chan: ch.ID, From: 1, To: 3}, proto.ErrForbidden)
	carol.fail(proto.OpKeyFetch, proto.KeyFetchArgs{Chan: 999999, From: 1, To: 3}, proto.ErrForbidden)
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
	alice.rotate(ch.ID, e2)
	d1 := bundle(t, a1, sid, dm.Chan.ID, 1, nil, roster, a1)
	alice.rotate(dm.Chan.ID, d1)

	c, _ := ts.connectCaps(t, bob.signer, "", "", e2eCaps)
	snap, _ := c.syncFresh()
	if len(snap.Keys) != 2 || !bytes.Equal(snap.Keys[0].B, e2.B) || !bytes.Equal(snap.Keys[1].B, d1.B) {
		t.Fatalf("bob's snapshot keys: %d", len(snap.Keys))
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

		alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
		bob.event(proto.KChanJoin)
		r2 := []uint64{alice.user.ID, bob.user.ID, old.user.ID}
		e2 := bundle(t, a1, sid, ch.ID, 2, &e1, r2, a1, b1)
		s2 := alice.rotate(ch.ID, e2)
		live[alice] = append(live[alice], s2)
		alice.keyEpoch()
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

	// dm.open: encrypted when the caller speaks e2e1, at epoch 0.
	if c := open(alice, bob.user.ID); !c.E2E || c.Epoch != 0 {
		t.Fatalf("e2e1 DM %+v", c)
	}
	if c := open(alice); !c.E2E {
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
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: carol.user.ID}, nil)
	stale(ch.ID, true) // roster grew
	roster = []uint64{alice.user.ID, bob.user.ID, carol.user.ID}
	e3 := bundle(t, a1, sid, ch.ID, 3, &e2, roster, a1, b2)
	alice.rotate(ch.ID, e3)
	stale(ch.ID, false)
	alice.ok(proto.OpUserDisable, proto.UserArgs{User: carol.user.ID}, nil)
	stale(ch.ID, true) // a member was disabled
	alice.rotate(ch.ID, bundle(t, a1, sid, ch.ID, 4, &e3, []uint64{alice.user.ID, bob.user.ID}, a1, b2))
	stale(ch.ID, false)
	bob.ok(proto.OpChanLeave, proto.ChanArgs{Chan: ch.ID}, nil)
	stale(ch.ID, true) // a member left
}
