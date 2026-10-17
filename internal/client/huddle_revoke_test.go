package client

import (
	"context"
	"testing"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// Voice follows the text path's revoke rules (v0.3.0): after we revoke a
// device that may hold the channel's K, voice must not seal under any epoch
// until our own rotation lands (trust.RotationOwed), even when the server
// refuses that rotation; and frames from the revoked device are refused.
func TestHuddleKeysRevokeOwed(t *testing.T) {
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

	k := &huddleKeys{c: d.c, sid: testSID(), ch: dm, me: alice, dev: d.me.ID,
		parts: map[uint32]proto.Part{}, cache: map[[2]uint32]cachedKey{}}
	var stream [e2ecrypto.VoiceStreamSize]byte
	k.setParts([]proto.Part{{Src: 7, User: alice, Dev: laptop.dev.ID[:], Stream: stream[:]},
		{Src: 8, User: bob, Dev: d.b1.dev.ID[:], Stream: stream[:]}})
	if !k.fresh() {
		t.Fatal("epoch 1 not fresh before any revoke")
	}
	if _, ok := k.Recv(7, 1); !ok {
		t.Fatal("laptop's frames refused before the revoke")
	}

	d.f.mu.Lock()
	d.f.onRotate = func(f *e2eFake, id uint32, a proto.KeyRotateArgs) {
		f.write(proto.TRes, proto.Res{ID: id, Err: proto.Errf(proto.ErrStaleEpoch, "nope")})
	}
	d.f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := d.c.RevokeDevice(ctx, laptop.dev.ID, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "rotation owed", func() bool {
		owed, err := d.c.Trust().RotationOwed(testSID(), dm)
		return err == nil && owed
	})
	if k.fresh() {
		t.Fatal("voice may seal under epoch 1 while a rotation is owed")
	}
	k.setParts([]proto.Part{{Src: 7, User: alice, Dev: laptop.dev.ID[:], Stream: stream[:]},
		{Src: 8, User: bob, Dev: d.b1.dev.ID[:], Stream: stream[:]}}) // drops cached keys
	if _, ok := k.Recv(7, 1); ok {
		t.Fatal("frames from the revoked laptop accepted")
	}
	// The laptop holds K_1 and frame keys are group-symmetric: it could
	// forge bob's frames, so nothing under epoch 1 is played any more.
	if _, ok := k.Recv(8, 1); ok {
		t.Fatal("frames under an epoch the revoked laptop holds accepted (forgeable as bob)")
	}
	d.c.mu.Lock()
	s := d.c.sess
	d.c.mu.Unlock()
	rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
	if err := k.refresh(rctx, s); err == nil {
		t.Fatal("refresh succeeded while the server refuses our rotation")
	}
	rcancel()

	d.f.mu.Lock()
	d.f.onRotate = nil
	d.f.mu.Unlock()
	waitFor(t, "owed rotation paid", func() bool {
		owed, err := d.c.Trust().RotationOwed(testSID(), dm)
		return err == nil && !owed && k.fresh()
	})
	h, _, _ := d.c.Trust().ChainHead(testSID(), dm)
	if ep, _, _ := d.c.Trust().Epoch(testSID(), dm, h.Epoch); h.Epoch < 2 || containsDev(ep.Boxed, laptop.dev.ID) {
		t.Fatalf("voice head %d boxes %x", h.Epoch, ep.Boxed)
	}
	k.prime() // the watcher does this every second while fresh
	if ep, s, ok := k.Send(); !ok || ep != h.Epoch || s == nil {
		t.Fatalf("Send under %d, %v", ep, ok)
	}
}
