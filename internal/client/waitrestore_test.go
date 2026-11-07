package client

import (
	"testing"

	"github.com/skailama/tc/internal/proto"
)

// #198: while a channel is in a rollback or fork alarm, a message sealed
// under an epoch this device hasn't verified (another bundle of a pinned
// epoch, or one above the pinned head) shows as waiting for /alarm-recover,
// not as invalid. A genuinely bad message stays invalid, alarm or not, and
// without a chain alarm another bundle is invalid as before.
func TestWaitRestoreUnderChainAlarm(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	msg := func(body string) plainMsg { return plainMsg{T: "msg", TS: 1, Body: body} }

	// Another bundle for epoch 1 (a member who accepted a restore and
	// started a new key there): never presented as a key.epoch.
	k1b := newK(t)
	e1b := mkBundle(t, d.b1.dev, dm, 1, nil, dmRoster, k1b, nil, d.me, d.b1.dev)
	forked := d.f.post(proto.Message{Chan: dm, Author: bob, Enc: sealAs(t, d.b1.dev, k1b, 1, e1b, msg("under the new chain"))})
	// Bad: our bundle of epoch 1, sealed with the wrong key.
	bad := d.f.post(proto.Message{Chan: dm, Author: bob, Enc: sealAs(t, d.b1.dev, newK(t), 1, d.e1, msg("forged"))})
	// Above the pinned head (epoch 1).
	above := d.f.post(proto.Message{Chan: dm, Author: bob, Enc: sealAs(t, d.b1.dev, newK(t), 2, d.e1, msg("epoch 2"))})

	waitFor(t, "judged", func() bool {
		s1, _, ok1 := waitOf(d.c, forked)
		s2, _, ok2 := waitOf(d.c, bad)
		s3, w3, ok3 := waitOf(d.c, above)
		return ok1 && ok2 && ok3 && s1 == SecInvalid && s2 == SecInvalid && s3 == SecWaiting && w3 == WaitKey
	})

	sid := testSID()
	// Not a chain alarm: nothing changes.
	if err := d.c.trust.RaiseAlarm(sid, dm, "bad bundle: forged signature"); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := waitOf(d.c, forked); s != SecInvalid {
		t.Fatalf("other bundle under a non-chain alarm = %v, want invalid", s)
	}
	if _, w, _ := waitOf(d.c, above); w != WaitKey {
		t.Fatalf("above the head under a non-chain alarm = %v, want key", w)
	}
	if err := d.c.trust.ClearChannelAlarm(sid, dm, alice); err != nil {
		t.Fatal(err)
	}

	if err := d.c.trust.RaiseAlarm(sid, dm, "fork: a second bundle for epoch 1"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id   uint64
		name string
	}{{forked, "other bundle"}, {above, "above the head"}} {
		m, ok := findMsg(d.c, c.id)
		if !ok || m.Sec != SecWaiting || m.Wait != WaitRestore || m.Body != "" {
			t.Fatalf("%s under a fork alarm = %v %v %q, want waiting for the restore", c.name, m.Sec, m.Wait, m.Body)
		}
	}
	if m, _ := findMsg(d.c, bad); m.Sec != SecInvalid || m.Body != "" {
		t.Fatalf("bad message under a fork alarm = %v %q, want invalid", m.Sec, m.Body)
	}
	// Threads and other views read through the same path.
	for _, m := range d.c.Thread(forked) {
		if m.ID == forked && (m.Sec != SecWaiting || m.Wait != WaitRestore) {
			t.Fatalf("thread view = %v %v", m.Sec, m.Wait)
		}
	}
}
