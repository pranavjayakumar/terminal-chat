package client

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

// Recovering after a restore (recover.go) against a scripted, lying server.

func recoverCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	t.Cleanup(cancel)
	return ctx
}

// A branch epoch sealed to a device this device knows is revoked is
// refused, whatever seq the server gives it: bob's stolen device b2 was
// revoked (seq 100) and alice's epoch 3 rotated away from it; the server
// withholds both and serves bob's ordinary epoch 3' on 2, sealed to b2,
// with a seq before the revoke (R1).
func TestRecoverRefusesBranchSealedToRevokedDevice(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	b2 := newPeer(t, bob, b1.ssh)
	roster := []uint64{alice, bob}
	devs := []proto.Device{{Cert: b1.cert}, {Cert: b2.cert}}
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)}, Devs: devs}, proto.DevicesListRes{})
	me := f.myKeys(c)
	waitFor(t, "b2 verified", func() bool { _, ok, _ := c.Trust().Device(testSID(), b2.dev.ID); return ok })
	k1, k2, k3 := newK(t), newK(t), newK(t)
	e1 := mkBundle(t, b1.dev, dm, 1, nil, roster, k1, nil, me, b1.dev, b2.dev)
	s1 := f.keyEpoch(dm, bob, e1)
	e2 := mkBundle(t, b1.dev, dm, 2, &e1, roster, k2, &k1, me, b1.dev, b2.dev)
	s2 := f.keyEpoch(dm, bob, e2)
	f.mu.Lock()
	f.seq = 100
	f.mu.Unlock()
	e3 := mkBundle(t, b1.dev, dm, 3, &e2, roster, k3, &k2, me, b1.dev)
	f.keyEpoch(dm, bob, e3)
	waitFor(t, "epoch 3", func() bool { return headOf(t, c, dm) == 3 && c.E2EStatus(dm).State == E2EOK })
	if err := c.Trust().RevokeDeviceAt(testSID(), b2.dev.ID, 100); err != nil {
		t.Fatal(err)
	}
	head, _, _ := c.Trust().ChainHead(testSID(), dm)

	stolen := mkBundle(t, b1.dev, dm, 3, &e2, roster, newK(t), &k2, me, b1.dev, b2.dev)
	f.fc.conn.Close()
	startE2EWith(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(3)}, Devs: devs,
		Keys: []proto.EpochKey{{Bundle: e1, Seq: s1}, {Bundle: e2, Seq: s2}, {Bundle: stolen, Seq: 90}}},
		proto.DevicesListRes{}, func(f *e2eFake) {
			f.addBundle(dm, e1, s1)
			f.addBundle(dm, e2, s2)
			f.addBundle(dm, stolen, 90)
		})
	waitFor(t, "fork alarm", func() bool {
		st := c.E2EStatus(dm)
		return st.State == E2EAlarm && strings.Contains(st.Detail, "fork")
	})
	_, err := c.ChainRecoveryPlan(recoverCtx(t), dm)
	if !errors.Is(err, ErrNotRecoverable) || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("plan for a branch sealed to a revoked device: %v", err)
	}
	if h, _, _ := c.Trust().ChainHead(testSID(), dm); h != head {
		t.Fatal("the refusal moved the head")
	}
}

// The new epoch's seq is the server's word: it must not move the cutoff
// for messages under the shared epoch past the one this device verified
// (the discarded epoch 3 at seq 30). The server answers alice's fresh
// epoch 3' with seq 1000; a message sealed under epoch 2 at id 41 stays
// invalid (R2).
func TestRecoverKeepsEarliestCutoff(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	roster := []uint64{alice, bob}
	devs := []proto.Device{{Cert: b1.cert}}
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)}, Devs: devs}, proto.DevicesListRes{})
	me := f.myKeys(c)
	k1, k2, k3 := newK(t), newK(t), newK(t)
	e1 := mkBundle(t, b1.dev, dm, 1, nil, roster, k1, nil, me, b1.dev)
	s1 := f.keyEpoch(dm, bob, e1)
	e2 := mkBundle(t, b1.dev, dm, 2, &e1, roster, k2, &k1, me, b1.dev)
	s2 := f.keyEpoch(dm, bob, e2)
	f.mu.Lock()
	f.seq = 29
	f.mu.Unlock()
	e3 := mkBundle(t, b1.dev, dm, 3, &e2, roster, k3, &k2, me, b1.dev)
	if s3 := f.keyEpoch(dm, bob, e3); s3 != 30 {
		t.Fatalf("epoch 3 at seq %d", s3)
	}
	waitFor(t, "epoch 3", func() bool { return headOf(t, c, dm) == 3 && c.E2EStatus(dm).State == E2EOK })

	f.fc.conn.Close()
	f2 := startE2EWith(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(2)}, Devs: devs,
		Keys: []proto.EpochKey{{Bundle: e1, Seq: s1}, {Bundle: e2, Seq: s2}}},
		proto.DevicesListRes{}, func(f *e2eFake) {
			f.addBundle(dm, e1, s1)
			f.addBundle(dm, e2, s2)
			f.seq = 39
			f.onRotate = func(f *e2eFake, id uint32, a proto.KeyRotateArgs) {
				f.keyEpoch(a.Chan, alice, a.Bundle)
				f.ok(id, proto.SeqRes{Seq: 1000})
			}
		})
	waitFor(t, "rollback alarm", func() bool {
		st := c.E2EStatus(dm)
		return st.State == E2EAlarm && strings.Contains(st.Detail, "rollback")
	})
	plan, err := c.ChainRecoveryPlan(recoverCtx(t), dm)
	if err != nil || plan.Fork || plan.Ancestor != 2 || plan.Head != 3 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if err := c.RecoverChain(recoverCtx(t), plan); err != nil {
		t.Fatal(err)
	}
	if e, _, _ := c.Trust().Epoch(testSID(), dm, 3); e.Seq != 30 {
		t.Fatalf("epoch 3' pinned at seq %d, want the verified cutoff 30", e.Seq)
	}
	enc := sealAs(t, b1.dev, k2, 2, e2, plainMsg{T: "msg", TS: 1, Body: "under the old key"})
	id := f2.post(proto.Message{Chan: dm, Author: bob, Enc: enc})
	if id <= 30 || id >= 1000 {
		t.Fatalf("message id %d not between the cutoffs", id)
	}
	waitFor(t, "applied", func() bool { return lastSeq(t, c) >= id })
	if m, ok := findMsg(c, id); !ok || m.Sec != SecInvalid {
		t.Fatalf("message under the shared epoch after the cutoff: %+v", m)
	}
}
