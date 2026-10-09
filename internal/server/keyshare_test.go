package server

import (
	"bytes"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// keyNeedFrom waits up to d for a key.need ephemeral; ok is false on timeout.
// Other frames are dropped.
func (c *tclient) keyNeedFrom(d time.Duration) (e proto.Ephemeral, ok bool) {
	c.t.Helper()
	c.held = nil
	deadline := time.After(d)
	for {
		select {
		case f, open := <-c.frames:
			if !open {
				return e, false
			}
			if f.Type == proto.TEphemeral && f.Decode(&e) == nil && e.Kind == proto.EphKeyNeed {
				return e, true
			}
		case <-deadline:
			return e, false
		}
	}
}

func (c *tclient) sendEph(e proto.Ephemeral) {
	c.t.Helper()
	if err := c.codec.Write(proto.TEphemeral, e); err != nil {
		c.t.Fatal(err)
	}
}

func TestKeyNeedRelay(t *testing.T) {
	ts := startServer(t, "tcp")
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
	c1, _ := carol.registerDevice(ts)
	need := proto.Ephemeral{Kind: proto.EphKeyNeed, Chan: ch.ID, Epoch: 1, Dev: b1.ID[:]}

	// A fresh session of bob: each has its own rate-limit slot per channel.
	bobSession := func() *tclient {
		bs, bye := ts.connectCaps(t, bob.signer, "", "", e2eCaps)
		if bye != "" {
			t.Fatal(bye)
		}
		bs.syncFresh()
		return bs
	}
	// Epoch 0: nothing to ask for yet.
	bobSession().sendEph(need)
	if _, ok := alice.keyNeedFrom(200 * time.Millisecond); ok {
		t.Fatal("key.need relayed for epoch 0")
	}
	alice.rotate(ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, []uint64{alice.user.ID, bob.user.ID, old.user.ID}, a1, b1))

	// Second session of alice (her other device): gets it too.
	alice2, bye := ts.connectCaps(t, alice.signer, "", "", e2eCaps)
	if bye != "" {
		t.Fatal(bye)
	}
	alice2.syncFresh()

	// Someone else's device, a future epoch, a garbage dev: dropped. Each
	// from its own session (a dropped key.need still uses up the session's
	// slot for the channel).
	for _, bad := range []proto.Ephemeral{
		{Kind: proto.EphKeyNeed, Chan: ch.ID, Epoch: 1, Dev: a1.ID[:]},
		{Kind: proto.EphKeyNeed, Chan: ch.ID, Epoch: 2, Dev: b1.ID[:]},
		{Kind: proto.EphKeyNeed, Chan: ch.ID, Epoch: 1, Dev: []byte{1, 2}},
	} {
		bobSession().sendEph(bad)
	}
	// A non-member asking.
	carol.sendEph(proto.Ephemeral{Kind: proto.EphKeyNeed, Chan: ch.ID, Epoch: 1, Dev: c1.ID[:]})
	if e, ok := alice.keyNeedFrom(200 * time.Millisecond); ok {
		t.Fatalf("bad key.need relayed: %+v", e)
	}

	bob.sendEph(need)
	for _, c := range []*tclient{alice, alice2} {
		e, ok := c.keyNeedFrom(2 * time.Second)
		if !ok || e.User != bob.user.ID || e.Chan != ch.ID || e.Epoch != 1 || !bytes.Equal(e.Dev, b1.ID[:]) {
			t.Fatalf("%s: key.need = %+v %v", c.user.Name, e, ok)
		}
	}
	if _, ok := carol.keyNeedFrom(100 * time.Millisecond); ok {
		t.Fatal("non-member got key.need")
	}
	if _, ok := old.keyNeedFrom(50 * time.Millisecond); ok {
		t.Fatal("session without e2e1 got key.need")
	}
	if _, ok := bob.keyNeedFrom(50 * time.Millisecond); ok {
		t.Fatal("key.need echoed to its sender")
	}
	// ≤ 1 per channel per 30 s.
	bob.sendEph(need)
	if _, ok := alice.keyNeedFrom(200 * time.Millisecond); ok {
		t.Fatal("rate limit: second key.need relayed")
	}
}

// The per-session rate limit comes before any database check: a key.need
// that is then dropped as invalid still uses up the slot, so a session can't
// make the server query for every frame of a flood.
func TestKeyNeedRateLimitFirst(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	alice.syncFresh()
	bob.syncFresh()
	sid := ts.sid()
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	a1, _ := alice.registerDevice(ts)
	b1, _ := bob.registerDevice(ts)
	alice.rotate(ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, []uint64{alice.user.ID, bob.user.ID}, a1, b1))
	bob.sendEph(proto.Ephemeral{Kind: proto.EphKeyNeed, Chan: ch.ID, Epoch: 1, Dev: a1.ID[:]}) // not bob's: dropped
	bob.sendEph(proto.Ephemeral{Kind: proto.EphKeyNeed, Chan: ch.ID, Epoch: 1, Dev: b1.ID[:]})
	if e, ok := alice.keyNeedFrom(300 * time.Millisecond); ok {
		t.Fatalf("relayed within the rate limit window: %+v", e)
	}
}

func TestKeyShare(t *testing.T) {
	forEachTransport(t, func(t *testing.T, ts *testServer) {
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
		alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
		alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: old.user.ID}, nil)
		a1, _ := alice.registerDevice(ts)
		b1, _ := bob.registerDevice(ts)
		c1, _ := carol.registerDevice(ts)
		share := proto.KeyShareArgs{Chan: ch.ID, Epoch: 1, Dev: b1.ID[:], Eph: make([]byte, 32), CT: make([]byte, 48)}

		alice.fail(proto.OpKeyShare, share, proto.ErrBadRequest) // epoch 0
		alice.rotate(ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, []uint64{alice.user.ID, bob.user.ID, old.user.ID}, a1, b1))

		for name, c := range map[string]struct {
			args proto.KeyShareArgs
			code string
		}{
			"short ct":   {proto.KeyShareArgs{Chan: ch.ID, Epoch: 1, Dev: b1.ID[:], Eph: make([]byte, 32), CT: make([]byte, 47)}, proto.ErrBadRequest},
			"short eph":  {proto.KeyShareArgs{Chan: ch.ID, Epoch: 1, Dev: b1.ID[:], Eph: make([]byte, 31), CT: make([]byte, 48)}, proto.ErrBadRequest},
			"epoch 0":    {proto.KeyShareArgs{Chan: ch.ID, Epoch: 0, Dev: b1.ID[:], Eph: make([]byte, 32), CT: make([]byte, 48)}, proto.ErrBadRequest},
			"future":     {proto.KeyShareArgs{Chan: ch.ID, Epoch: 2, Dev: b1.ID[:], Eph: make([]byte, 32), CT: make([]byte, 48)}, proto.ErrBadRequest},
			"unknown":    {proto.KeyShareArgs{Chan: ch.ID, Epoch: 1, Dev: make([]byte, 16), Eph: make([]byte, 32), CT: make([]byte, 48)}, proto.ErrNotFound},
			"non-member": {proto.KeyShareArgs{Chan: ch.ID, Epoch: 1, Dev: c1.ID[:], Eph: make([]byte, 32), CT: make([]byte, 48)}, proto.ErrBadRequest},
			"no chan":    {proto.KeyShareArgs{Chan: 9999, Epoch: 1, Dev: b1.ID[:], Eph: make([]byte, 32), CT: make([]byte, 48)}, proto.ErrNotFound},
		} {
			t.Run(name, func(t *testing.T) { alice.fail(proto.OpKeyShare, c.args, c.code) })
		}
		carol.fail(proto.OpKeyShare, share, proto.ErrForbidden)
		old.fail(proto.OpKeyShare, share, proto.ErrUnknownOp)

		alice.ok(proto.OpKeyShare, share, nil)
		for _, c := range []*tclient{alice, bob} {
			ev := c.event(proto.KKeyShare)
			var got proto.KeyShareArgs
			if err := proto.Unmarshal(ev.Data, &got); err != nil || ev.Chan != ch.ID || ev.Actor != alice.user.ID ||
				!bytes.Equal(got.Dev, b1.ID[:]) || got.Epoch != 1 {
				t.Fatalf("%s: key.share event %+v %+v %v", c.user.Name, ev, got, err)
			}
		}
		carol.noKind(proto.KKeyShare, 100*time.Millisecond)
		old.noKind(proto.KKeyShare, 50*time.Millisecond)

		// Replay matches live: members with e2e1 only.
		count := func(evs []proto.Event) int {
			n := 0
			for _, e := range evs {
				if e.Kind == proto.KKeyShare {
					n++
				}
			}
			return n
		}
		for c, want := range map[*tclient]int{bob: 1, carol: 0} {
			if got := count(c.replayCaps(t, ts, since, e2eCaps)); got != want {
				t.Fatalf("%s replayed %d key.share, want %d", c.user.Name, got, want)
			}
		}
		if got := count(bob.replayCaps(t, ts, since, nil)); got != 0 {
			t.Fatalf("replay without e2e1: %d key.share", got)
		}
	})
}
