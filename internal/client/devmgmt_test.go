package client

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// A verified revoke of a device we already trusted is reported once in
// Update.Revoked; revokes that arrive with the device itself (a first sync,
// old history) are not.
func TestDeviceRevokeNotice(t *testing.T) {
	cp := newPeer(t, carol, nil)
	old, err := identity.NewDeviceRevoke(cp.ssh, testSID(), carol, cp.dev.ID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	notices := make(chan DeviceRef, 8)
	newOwn := make(chan DeviceRef, 8)
	d := newEncDMWith(t, proto.Snapshot{
		Devs:        []proto.Device{{Cert: cp.cert}},
		Revoked:     []proto.Signed{proto.Signed(old)},
		RevokedSeqs: []uint64{1},
	}, func(c *Client) {
		go func() {
			for u := range c.Updates() {
				for _, r := range u.Revoked {
					notices <- r
				}
				for _, r := range u.NewOwn {
					newOwn <- r
				}
			}
		}()
	})
	waitFor(t, "carol's old revoke applied", func() bool {
		r, ok, _ := d.c.Trust().Device(testSID(), cp.dev.ID)
		return ok && r.Revoked
	})

	rev, err := identity.NewDeviceRevoke(d.b1.ssh, testSID(), bob, d.b1.dev.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d.f.event(proto.KDeviceRevoke, 0, bob, proto.DeviceRevokeArgs{Revoke: proto.Signed(rev)})
	select {
	case r := <-notices:
		if r.User != bob || r.Dev != d.b1.dev.ID {
			t.Fatalf("notice %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notice for bob's revoked device")
	}
	select {
	case r := <-notices:
		t.Fatalf("extra notice %+v", r)
	case <-time.After(300 * time.Millisecond):
	}

	select {
	case r := <-newOwn:
		t.Fatalf("own device announced on first sync: %+v", r)
	default:
	}
	signer, err := identity.Load(d.c.cfg.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	laptop := newPeer(t, alice, signer)
	d.f.event(proto.KDeviceNew, 0, alice, proto.Device{Cert: laptop.cert})
	select {
	case r := <-newOwn:
		if r.User != alice || r.Dev != laptop.dev.ID {
			t.Fatalf("new own device %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a new device of mine wasn't announced")
	}

	bd := d.c.Devices(bob)
	if len(bd) != 1 || !bd[0].Revoked || !bd[0].Verified || bd[0].This || bd[0].RevokedAt.IsZero() {
		t.Fatalf("bob's devices: %+v", bd)
	}
	md := d.c.Devices(alice)
	if len(md) != 2 || !md[0].This || md[0].ID != d.me.ID || md[1].ID != laptop.dev.ID {
		t.Fatalf("my devices: %+v", md)
	}
	if got, err := d.c.FindDevice(bob, hex.EncodeToString(d.b1.dev.ID[:])); err != nil || got.ID != d.b1.dev.ID {
		t.Fatalf("find by full id: %+v %v", got, err)
	}
	if got, err := d.c.FindDevice(bob, " "+bd[0].Short()[:3]+" "); err != nil || got.ID != d.b1.dev.ID {
		t.Fatalf("find by prefix: %+v %v", got, err)
	}
	for _, p := range []string{"", "zz", "ffffffffffffffffffffffffffffffffff"} {
		if _, err := d.c.FindDevice(bob, p); !errors.Is(err, ErrNoDevice) {
			t.Fatalf("find %q: %v", p, err)
		}
	}
}

// RevokeDevice refuses this device unless allowed, and anything that isn't
// a verified device of ours.
func TestRevokeDeviceRefusals(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	ctx := t.Context()
	if _, err := d.c.RevokeDevice(ctx, d.me.ID, false); !errors.Is(err, ErrRevokeSelf) {
		t.Fatalf("self: %v", err)
	}
	if _, err := d.c.RevokeDevice(ctx, d.b1.dev.ID, false); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("someone else's device: %v", err)
	}
}
