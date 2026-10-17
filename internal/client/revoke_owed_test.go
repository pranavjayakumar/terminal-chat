package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// The revoker's owed rotation is paid only by an epoch it commits itself
// (protocol-guardian, PR #61). A colluding server lets someone else's epoch
// 2 land just before alice's revoke of her laptop (the laptop may hold its
// K by key.share: epoch 2 doesn't box it), backdates the revoke's seq below
// epoch 2's and refuses alice's rotations. Epoch 2 then looks fine by seqs
// and boxes, but alice must not seal under it. Once a rotation of hers goes
// through, the debt is paid and sends work again.
func TestRevokeOwedSurvivesForeignEpoch(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	signer, err := identity.Load(d.c.cfg.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	laptop := newPeer(t, alice, signer)
	d.f.event(proto.KDeviceNew, 0, alice, proto.Device{Cert: laptop.cert})
	waitFor(t, "laptop verified", func() bool {
		_, ok, _ := d.c.Trust().Device(testSID(), laptop.dev.ID)
		return ok
	})

	k2 := newK(t)
	e2 := mkBundle(t, d.b1.dev, dm, 2, &d.e1, dmRoster, k2, &d.k1, d.me, d.b1.dev)
	d.f.mu.Lock()
	d.f.onOp = func(f *e2eFake, r proto.Req) bool {
		if r.Op != proto.OpDeviceRevoke {
			return false
		}
		f.keyEpoch(dm, bob, e2)
		f.ok(r.ID, proto.SeqRes{Seq: d.seq1}) // backdated; the event is withheld
		return true
	}
	d.f.onRotate = func(f *e2eFake, id uint32, a proto.KeyRotateArgs) {
		f.write(proto.TRes, proto.Res{ID: id, Err: proto.Errf(proto.ErrStaleEpoch, "nope")})
	}
	d.f.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := d.c.RevokeDevice(ctx, laptop.dev.ID, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "epoch 2 verified", func() bool { return d.c.E2EStatus(dm).Epoch == 2 })
	if owed, err := d.c.Trust().RotationOwed(testSID(), dm); err != nil || !owed {
		t.Fatalf("owed %v, %v: someone else's epoch paid alice's rotation", owed, err)
	}
	if err := d.c.Send(dm, "after the revoke", 0); err != nil {
		t.Fatal(err)
	}
	fctx, fcancel := context.WithTimeout(ctx, 5*time.Second)
	err = d.c.Flush(fctx)
	fcancel()
	if err == nil || !strings.Contains(err.Error(), rotateReason) {
		t.Fatalf("flush: %v", err)
	}
	select {
	case a := <-d.f.sends:
		h, _ := e2ecrypto.ParseEnc(a.Enc)
		t.Fatalf("sealed under epoch %d, which the revoked laptop may hold", h.Epoch)
	default:
	}

	// The server gives in: the e2e worker makes the owed rotation by itself.
	d.f.mu.Lock()
	d.f.onRotate = nil
	d.f.mu.Unlock()
	waitFor(t, "our own epoch 3 paid the owed rotation", func() bool {
		owed, err := d.c.Trust().RotationOwed(testSID(), dm)
		return err == nil && !owed && d.c.E2EStatus(dm).Epoch == 3
	})
	if ep, _, _ := d.c.Trust().Epoch(testSID(), dm, 3); len(ep.Boxed) == 0 || containsDev(ep.Boxed, laptop.dev.ID) {
		t.Fatalf("epoch 3 boxes %x", ep.Boxed)
	}
	if err := d.c.Send(dm, "after our rotation", 0); err != nil {
		t.Fatal(err)
	}
	a := nextSend(t, d.f)
	if h, err := e2ecrypto.ParseEnc(a.Enc); err != nil || h.Epoch != 3 {
		t.Fatalf("sent under epoch %d (%v)", h.Epoch, err)
	}
}

func containsDev(ds []e2ecrypto.DevID, d e2ecrypto.DevID) bool {
	for _, x := range ds {
		if x == d {
			return true
		}
	}
	return false
}

// A revoke seq past our sync cursor counts as unknown (it would keep every
// epoch until then stale); one implausibly far past it raises a server
// alarm (protocol-guardian, PR #61).
func TestRevokeSeqPastCursor(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c2 := newPeer(t, carol, nil)
	c3 := newPeer(t, carol, c2.ssh)
	r2, err := identity.NewDeviceRevoke(c2.ssh, testSID(), carol, c2.dev.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r3, err := identity.NewDeviceRevoke(c2.ssh, testSID(), carol, c3.dev.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cur := lastSeq(t, d.c)
	d.f.mu.Lock()
	d.f.devs = proto.DevicesListRes{Certs: []proto.Signed{c2.cert, c3.cert},
		Revoked: []proto.Signed{proto.Signed(r2), proto.Signed(r3)}, RevokedSeqs: []uint64{cur + 5, cur + proto.SnapshotGap + 100}}
	d.f.mu.Unlock()
	if err := d.c.RefreshDevices(ctx, carol); err != nil {
		t.Fatal(err)
	}
	for _, id := range []e2ecrypto.DevID{c2.dev.ID, c3.dev.ID} {
		dv, ok, err := d.c.Trust().Device(testSID(), id)
		if err != nil || !ok || !dv.Revoked || dv.RevokedSeq != 0 {
			t.Fatalf("device %x: %+v %v %v; want revoked at an unknown seq", id[:4], dv, ok, err)
		}
	}
	if detail, ok, _ := d.c.Trust().ServerAlarm(testSID()); !ok || !strings.Contains(detail, "far past") {
		t.Fatalf("server alarm %q %v", detail, ok)
	}
}
