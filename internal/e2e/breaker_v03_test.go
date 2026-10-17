package e2e

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// The server allows proto.MaxDevices active devices per user and answers a
// ninth device.register with "too many devices: revoke one first". Device
// revoke (tc devices revoke / /device-revoke) is the way out, and the new
// machine is where the user is standing. But RevokeDevice waits in
// e2eSession for this device's registration (devReady), which can never
// happen while the list is full: the revoke hangs until its context ends
// (tc devices uses signalContext, so forever). Revoking only needs the SSH
// key, which this machine has.
func TestRevokeDevice_BreakFromUnregisteredDeviceWhenFull(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	eventually(t, "online", alice.online)
	sid := alice.sid(t)
	a := alice.Me().ID
	devs := []*user{alice}
	for len(devs) < proto.MaxDevices {
		d := secondDevice(t, alice)
		devs = append(devs, d)
		n := len(devs)
		eventually(t, "device registered", func() bool { return d.online() && alice.verifiedDevices(sid, a) == n })
	}
	// Old machines: stop them (lost, reinstalled, wiped).
	for _, d := range devs[1:] {
		d.cancel()
	}

	fresh := secondDevice(t, alice)
	eventually(t, "new machine online and knows the old devices", func() bool {
		return fresh.online() && fresh.verifiedDevices(sid, a) == proto.MaxDevices
	})
	old, err := devs[1].Trust().DeviceKeys(sid, a)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fresh.RefreshDevices(ctx, a); err != nil {
		t.Fatal(err)
	}
	// The refusal is reported, not waited on for ever.
	eventually(t, "registration failure reported", func() bool { return errors.Is(fresh.DeviceError(), client.ErrTooManyDevices) })
	start := time.Now()
	if _, err := fresh.RevokeDevice(ctx, old.ID, false); err != nil {
		t.Fatalf("revoking an old device from a new machine while the device list is full: %v (after %s)",
			err, time.Since(start).Round(time.Second))
	}
	// The freed slot is this machine's at once.
	if err := fresh.DeviceError(); err != nil {
		t.Fatalf("after the revoke: %v", err)
	}
	mine, err := fresh.Trust().DeviceKeys(sid, a)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "new machine registered", func() bool {
		d, ok, _ := devs[0].Trust().Device(sid, mine.ID)
		return ok && !d.Revoked
	})
}

// `tc devices revoke <this> --self` promises the device "re-registers with
// new keys on its next connection" (and /device-revoke, tc devices list
// "Lost one?" invite users to revoke and re-add freely). Every registration
// uses up one of the user's proto.MaxDeviceRegistrations (32, ever, revoked
// included; every fresh TC_HOME is one). When none is left, revoke --self
// still goes through: the device is revoked for good, can't decrypt or send
// anything encrypted, and nothing can bring it back, with no warning before
// the irreversible step. RevokeDevice(self) must refuse (or at least not
// strand the device) when it can't come back.
func TestRevokeSelf_BreakExhaustsRegistrations(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", false)
	eventually(t, "online", alice.online)
	sid := alice.sid(t)
	a := alice.Me().ID
	cur, err := alice.Trust().DeviceKeys(sid, a)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "first device registered", func() bool {
		d, ok, _ := alice.Trust().Device(sid, cur.ID)
		return ok && !d.Revoked
	})

	// Past machines (reinstalls, CI, TC_HOME=/tmp/...): register and revoke
	// the rest of the user's registrations with the same SSH key.
	cfg := alice.Config()
	burnRegistrations(t, cfg.Addr, cfg.Pin, cfg.KeyPath, sid, a, proto.MaxDeviceRegistrations-1)

	time.Sleep(13 * time.Second) // let the write bucket (60, 5/s) refill
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := alice.RevokeDevice(ctx, cur.ID, true); err != nil {
		if isRateLimited(err) {
			t.Fatalf("still rate limited: %v", err)
		}
		t.Logf("revoke --self refused up front (fine): %v", err)
		return
	}
	next, err := alice.Trust().DeviceKeys(sid, a)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if d, ok, _ := alice.Trust().Device(sid, next.ID); ok && !d.Revoked && alice.online() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	l, _ := rawDevices(t, cfg.Addr, cfg.Pin, cfg.KeyPath)
	t.Fatalf("revoke --self went through with no registration left: this device is revoked for good "+
		"(server: %d certs, %d revoked, online=%v)", len(l.Certs), len(l.Revoked), alice.online())
}

// burnRegistrations registers n devices for user over a bare connection
// and revokes each right away.
func burnRegistrations(t *testing.T, addr, pin, keyPath string, sid e2ecrypto.ServerID, user uint64, n int) {
	t.Helper()
	signer, err := identity.Load(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := transport.DialTCP(ctx, addr, pin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	codec := proto.NewCodec(conn)
	codec.SetMaxFrame(proto.MaxFrame)
	exp, _ := conn.Exporter()
	sig, err := identity.Sign(signer, exp)
	if err != nil {
		t.Fatal(err)
	}
	codec.Write(proto.THello, proto.Hello{Client: "raw", Caps: []string{proto.CapE2E},
		Auth: proto.Auth{Pubkey: signer.PublicKey().Marshal(), Sig: sig}})
	var id uint32
	call := func(op string, args any) {
		t.Helper()
		id++
		codec.Write(proto.TReq, proto.Req{ID: id, Op: op, Args: proto.MustRaw(args)})
		for {
			f, err := codec.Read()
			if err != nil {
				t.Fatal(err)
			}
			if f.Type == proto.TBye {
				t.Fatal("raw: BYE")
			}
			if f.Type != proto.TRes {
				continue
			}
			var r proto.Res
			f.Decode(&r)
			if r.ID != id {
				continue
			}
			if !r.OK && r.Err != nil && r.Err.Code == proto.ErrRateLimited {
				time.Sleep(300 * time.Millisecond)
				id++
				codec.Write(proto.TReq, proto.Req{ID: id, Op: op, Args: proto.MustRaw(args)})
				continue
			}
			if !r.OK {
				t.Fatalf("raw %s: %v", op, r.Err)
			}
			return
		}
	}
	call(proto.OpSync, proto.SyncArgs{})
	for i := 0; i < n; i++ {
		keys, err := e2ecrypto.Generate(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := identity.NewDeviceCert(signer, sid, user, keys, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		call(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: proto.Signed(cert)})
		rev, err := identity.NewDeviceRevoke(signer, sid, user, keys.ID, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		call(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: proto.Signed(rev)})
	}
}
