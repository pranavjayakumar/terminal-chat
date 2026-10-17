package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// rawDevices logs in with keyPath over a bare connection (cap e2e1) and
// returns the server's devices.list and its server id.
func rawDevices(t *testing.T, addr, pin, keyPath string) (proto.DevicesListRes, e2ecrypto.ServerID) {
	t.Helper()
	signer, err := identity.Load(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.DialTCP(ctx, addr, pin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	codec := proto.NewCodec(conn)
	exp, _ := conn.Exporter()
	sig, err := identity.Sign(signer, exp)
	if err != nil {
		t.Fatal(err)
	}
	codec.Write(proto.THello, proto.Hello{Client: "raw", Caps: []string{proto.CapE2E},
		Auth: proto.Auth{Pubkey: signer.PublicKey().Marshal(), Sig: sig}})
	codec.SetMaxFrame(proto.MaxFrame)
	var w proto.Welcome
	var sid e2ecrypto.ServerID
	for id := uint32(0); ; {
		f, err := codec.Read()
		if err != nil {
			t.Fatal(err)
		}
		switch f.Type {
		case proto.TWelcome:
			f.Decode(&w)
			copy(sid[:], w.ServerID)
			id++
			codec.Write(proto.TReq, proto.Req{ID: id, Op: proto.OpSync, Args: proto.MustRaw(proto.SyncArgs{})})
		case proto.TRes:
			var r proto.Res
			f.Decode(&r)
			if !r.OK {
				t.Fatalf("raw req %d: %v", r.ID, r.Err)
			}
			if r.ID == 2 {
				var l proto.DevicesListRes
				if err := proto.Unmarshal(r.Data, &l); err != nil {
					t.Fatal(err)
				}
				return l, sid
			}
			id++
			codec.Write(proto.TReq, proto.Req{ID: id, Op: proto.OpDevicesList, Args: proto.MustRaw(proto.DevicesListArgs{})})
		case proto.TBye:
			t.Fatal("raw login: BYE")
		}
	}
}

// The real client registers its device with the real server exactly once,
// however often it (or another process on the same home) reconnects.
func TestDeviceRegisteredOnceAcrossRestarts(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	keyPath := alice.Config().KeyPath
	if keyPath == "" {
		t.Fatal("key path not recorded in config")
	}
	countFor := func(want int) proto.DevicesListRes {
		t.Helper()
		var l proto.DevicesListRes
		eventually(t, "devices registered", func() bool {
			l, _ = rawDevices(t, addr, s.Pin(), keyPath)
			return len(l.Certs) == want
		})
		return l
	}
	countFor(2)

	for range 2 {
		other, err := client.Open(filepath.Dir(keyPath))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go other.Run(ctx)
		eventually(t, "other online", func() bool { return other.Status().State == client.Online })
		time.Sleep(200 * time.Millisecond) // let a (wrong) second registration land
		cancel()
		other.Close()
	}

	l, sid := rawDevices(t, addr, s.Pin(), keyPath)
	if len(l.Certs) != 2 || len(l.Revoked) != 0 {
		t.Fatalf("after restarts: %d certs, %d revoked", len(l.Certs), len(l.Revoked))
	}
	signer, _ := identity.Load(keyPath)
	mine := 0
	for _, sc := range l.Certs {
		if c, err := identity.Signed(sc).VerifyDeviceCert(sid, signer.PublicKey().Marshal()); err == nil && c.User == alice.Me().ID {
			mine++
		}
	}
	if mine != 1 {
		t.Fatalf("alice has %d verifiable devices, want 1", mine)
	}
}

// secondDevice opens another TC_HOME with u's config: same user and SSH
// key, new device keys.
func secondDevice(t *testing.T, u *user) *user {
	t.Helper()
	home := t.TempDir()
	b, err := os.ReadFile(filepath.Join(u.home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := client.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	d := &user{Client: c, cancel: cancel, home: home}
	t.Cleanup(func() { cancel(); c.Close() })
	return d
}

// --self: a device revokes itself, retires its keys at once, reconnects and
// registers as a new device; its sibling sees the old one revoked, the new
// one active, and is told a new device of its user appeared. (So a revoked
// device that still holds the SSH key comes back: device revoke alone is no
// defense against a stolen SSH key.)
func TestRevokeSelfReRegisters(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	laptop := secondDevice(t, alice)
	eventually(t, "online", func() bool { return alice.online() && laptop.online() })
	sid := alice.sid(t)
	a := alice.Me().ID
	eventually(t, "both devices verified", func() bool {
		return alice.verifiedDevices(sid, a) == 2 && laptop.verifiedDevices(sid, a) == 2
	})
	newOwn := make(chan client.DeviceRef, 8)
	go func() {
		for u := range alice.Updates() {
			for _, r := range u.NewOwn {
				newOwn <- r
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	old, err := laptop.Trust().DeviceKeys(sid, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.RevokeDevice(ctx, old.ID, true); err != nil {
		t.Fatal(err)
	}
	cur, err := laptop.Trust().DeviceKeys(sid, a)
	if err != nil || cur.ID == old.ID {
		t.Fatalf("keys not retired at once: %v", err)
	}
	eventually(t, "laptop back online as a new device", func() bool {
		d, ok, _ := laptop.Trust().Device(sid, cur.ID)
		return laptop.online() && ok && !d.Revoked
	})
	eventually(t, "alice sees old revoked, new active", func() bool {
		var oldRev, newOK bool
		for _, d := range alice.Devices(a) {
			oldRev = oldRev || (d.ID == old.ID && d.Revoked)
			newOK = newOK || (d.ID == cur.ID && !d.Revoked && d.Verified)
		}
		return oldRev && newOK
	})
	// (The laptop's first device may have been announced too, if alice
	// verified herself before it registered.)
	for timeout := time.After(10 * time.Second); ; {
		select {
		case r := <-newOwn:
			if r.Dev == cur.ID {
				return
			}
			if r.Dev != old.ID {
				t.Fatalf("announced %+v", r)
			}
		case <-timeout:
			t.Fatal("alice wasn't told about her new device")
		}
	}
}

// "I lost my laptop": alice has two devices and revokes one from the
// other. Her encrypted DM with bob gets a new epoch without it at once, the
// revoked device can't read anything sent afterwards, her remaining device
// and bob keep working, and bob's client reports the revoke.
func TestRevokeLostDevice(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	laptop := secondDevice(t, alice)
	eventually(t, "online", func() bool { return alice.online() && bob.online() && laptop.online() })
	sid := alice.sid(t)
	a, b := alice.Me().ID, bob.Me().ID
	eventually(t, "devices verified", func() bool {
		return bob.verifiedDevices(sid, a) == 2 && alice.verifiedDevices(sid, b) == 1 &&
			laptop.verifiedDevices(sid, b) == 1 && alice.verifiedDevices(sid, a) == 2
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := alice.RefreshDevices(ctx, a); err != nil {
		t.Fatal(err)
	}
	devs := alice.Devices(a)
	if len(devs) != 2 || !devs[0].This || devs[1].This || !devs[0].Verified || !devs[1].Verified {
		t.Fatalf("alice's devices: %+v", devs)
	}
	lost := devs[1]
	if ld := laptop.Devices(a); len(ld) != 2 || !ld[0].This || ld[0].ID != lost.ID {
		t.Fatalf("laptop's view: %+v", ld)
	}
	if bd := bob.Devices(a); len(bd) != 2 || bd[0].This || bd[1].This {
		t.Fatalf("bob's view of alice: %+v", bd)
	}
	if got, err := alice.FindDevice(a, lost.Short()); err != nil || got.ID != lost.ID {
		t.Fatalf("find %s: %+v %v", lost.Short(), got, err)
	}
	if _, err := alice.RevokeDevice(ctx, devs[0].ID, false); !errors.Is(err, client.ErrRevokeSelf) {
		t.Fatalf("revoking this device without allowSelf: %v", err)
	}

	dm, err := alice.OpenDM(ctx, []uint64{b})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob has the DM", func() bool { _, ok := bob.Channel(dm.ID); return ok })
	if err := bob.Send(dm.ID, "before", 0); err != nil {
		t.Fatal(err)
	}
	if err := bob.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for _, u := range []*user{alice, laptop} {
		eventually(t, "both of alice's devices read bob", func() bool { sec, ok := u.sec(dm.ID, "before"); return ok && sec == client.SecE2E })
	}

	notices := make(chan client.DeviceRef, 4)
	go func() {
		for u := range bob.Updates() {
			for _, r := range u.Revoked {
				notices <- r
			}
		}
	}()
	rotated, err := alice.RevokeDevice(ctx, lost.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if rotated != 1 {
		t.Fatalf("rotated %d channels, want the DM", rotated)
	}
	if _, err := alice.RevokeDevice(ctx, lost.ID, false); !errors.Is(err, client.ErrAlreadyRevoked) {
		t.Fatalf("second revoke: %v", err)
	}
	select {
	case r := <-notices:
		if r.User != a || r.Dev != lost.ID {
			t.Fatalf("bob was told %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bob's client didn't report the revoke")
	}
	eventually(t, "bob sees the revoke", func() bool {
		bd := bob.Devices(a)
		return len(bd) == 2 && bd[1].ID == lost.ID && bd[1].Revoked && !bd[0].Revoked
	})

	if err := bob.Send(dm.ID, "after", 0); err != nil {
		t.Fatal(err)
	}
	if err := bob.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := alice.Send(dm.ID, "still here", 0); err != nil {
		t.Fatal(err)
	}
	if err := alice.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice reads bob after the revoke", func() bool { sec, ok := alice.sec(dm.ID, "after"); return ok && sec == client.SecE2E })
	eventually(t, "bob reads alice after the revoke", func() bool {
		sec, ok := bob.sec(dm.ID, "still here")
		return ok && sec == client.SecE2E
	})
	head, _, _ := alice.Trust().ChainHead(sid, dm.ID)
	if head.Epoch < 2 {
		t.Fatalf("DM still on epoch %d", head.Epoch)
	}
	time.Sleep(500 * time.Millisecond) // let key.need/key.share run, if they (wrongly) would
	if _, ok := laptop.sec(dm.ID, "after"); ok {
		t.Fatal("the revoked device decrypted a message sent after its revoke")
	}
	if _, ok := laptop.sec(dm.ID, "still here"); ok {
		t.Fatal("the revoked device decrypted alice's message sent after its revoke")
	}
	for e := uint32(2); e <= head.Epoch; e++ {
		if laptop.epochKey(sid, dm.ID, e) != nil {
			t.Fatalf("the revoked device holds the key of epoch %d", e)
		}
	}
}
