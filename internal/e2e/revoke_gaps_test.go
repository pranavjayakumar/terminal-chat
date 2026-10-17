package e2e

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// shareSetup: alice and bob have an encrypted DM with epoch 1 before
// alice's laptop exists; the laptop then registers and gets epoch 1's key
// by key.share (it isn't in the bundle's boxes) and reads the history.
func shareSetup(t *testing.T) (s *srv, addr string, alice, laptop, bob *user, dm uint64, ctx context.Context) {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr = startServer(t, t.TempDir(), "127.0.0.1:0")
	alice = login(t, s, addr, "alice", true)
	bob = login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	sid := alice.sid(t)
	a, b := alice.Me().ID, bob.Me().ID
	eventually(t, "devices verified", func() bool {
		return bob.verifiedDevices(sid, a) == 1 && alice.verifiedDevices(sid, b) == 1
	})
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	ch, err := alice.OpenDM(c, []uint64{b})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob has the DM", func() bool { _, ok := bob.Channel(ch.ID); return ok })
	bob.send(t, c, ch.ID, "before laptop")
	eventually(t, "alice reads bob", alice.reads(ch.ID, "before laptop"))

	laptop = secondDevice(t, alice)
	eventually(t, "laptop verified everywhere", func() bool {
		return laptop.online() && bob.verifiedDevices(sid, a) == 2 && alice.verifiedDevices(sid, a) == 2 &&
			laptop.verifiedDevices(sid, b) == 1
	})
	eventually(t, "laptop reads the history via key.share", laptop.reads(ch.ID, "before laptop"))
	head, _, _ := alice.Trust().ChainHead(sid, ch.ID)
	if head.Epoch != 1 {
		t.Fatalf("DM at epoch %d before the revoke, want 1", head.Epoch)
	}
	lk, err := laptop.Trust().DeviceKeys(sid, a)
	if err != nil {
		t.Fatal(err)
	}
	if ep, _, _ := alice.Trust().Epoch(sid, ch.ID, 1); slices.Contains(ep.Boxed, lk.ID) {
		t.Fatal("epoch 1 boxed the laptop; the test needs it to get K by share")
	}
	return s, addr, alice, laptop, bob, ch.ID, c
}

// noLaterReads checks that the revoked laptop reads none of bodies and holds
// no key past epoch 1.
func noLaterReads(t *testing.T, sid e2ecrypto.ServerID, alice, laptop *user, dm uint64, bodies ...string) {
	t.Helper()
	head, _, _ := alice.Trust().ChainHead(sid, dm)
	if head.Epoch < 2 {
		t.Fatalf("DM still on epoch %d after the revoke", head.Epoch)
	}
	time.Sleep(500 * time.Millisecond) // let key.need/key.share run, if they (wrongly) would
	for _, b := range bodies {
		if _, ok := laptop.sec(dm, b); ok {
			t.Fatalf("the revoked device decrypted %q, sent after its revoke", b)
		}
	}
	for e := uint32(2); e <= head.Epoch; e++ {
		if laptop.epochKey(sid, dm, e) != nil {
			t.Fatalf("the revoked device holds the key of epoch %d", e)
		}
	}
}

// B5: a device that got a channel's current key by key.share (history
// backfill) must lose it on revoke like one the bundle boxed: RevokeDevice
// rotates that channel at once.
func TestRevokeRotatesKeySharedChannel(t *testing.T) {
	_, _, alice, laptop, bob, dm, ctx := shareSetup(t)
	sid := alice.sid(t)
	lost, err := laptop.Trust().DeviceKeys(sid, alice.Me().ID)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := alice.RevokeDevice(ctx, lost.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if rotated != 1 {
		t.Fatalf("rotated %d channels, want the DM (whose key the laptop got by share)", rotated)
	}
	bob.send(t, ctx, dm, "bob after revoke")
	alice.send(t, ctx, dm, "alice after revoke")
	eventually(t, "alice reads bob", alice.reads(dm, "bob after revoke"))
	eventually(t, "bob reads alice", bob.reads(dm, "alice after revoke"))
	noLaterReads(t, sid, alice, laptop, dm, "bob after revoke", "alice after revoke")
}

// B5 backstop: the revoke reaches everyone but nobody rotated right after
// it (made by another tool, or the revoker went offline). The next sender
// must rotate before sealing, whichever member it is, because the revoked
// device may hold the current key by share.
func TestRevokeSendersRotateKeySharedChannel(t *testing.T) {
	s, addr, alice, laptop, bob, dm, ctx := shareSetup(t)
	sid := alice.sid(t)
	a := alice.Me().ID
	lost, err := laptop.Trust().DeviceKeys(sid, a)
	if err != nil {
		t.Fatal(err)
	}
	rawRevoke(t, addr, s.Pin(), alice.Config().KeyPath, sid, a, lost.ID)
	eventually(t, "bob sees the revoke", func() bool {
		d, ok, _ := bob.Trust().Device(sid, lost.ID)
		return ok && d.Revoked
	})
	bob.send(t, ctx, dm, "bob after raw revoke")
	eventually(t, "alice reads bob", alice.reads(dm, "bob after raw revoke"))
	alice.send(t, ctx, dm, "alice after raw revoke")
	eventually(t, "bob reads alice", bob.reads(dm, "alice after raw revoke"))
	if st := bob.status(dm); st.State != client.E2EOK {
		t.Fatalf("bob's DM status %v", st)
	}
	noLaterReads(t, sid, alice, laptop, dm, "bob after raw revoke", "alice after raw revoke")
}

// rawRevoke revokes dev with the SSH key at keyPath over a bare connection,
// so no tc client rotates anything because of it.
func rawRevoke(t *testing.T, addr, pin, keyPath string, sid e2ecrypto.ServerID, user uint64, dev e2ecrypto.DevID) {
	t.Helper()
	signer, err := identity.Load(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	rev, err := identity.NewDeviceRevoke(signer, sid, user, dev, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reqs := []proto.Req{
		{ID: 1, Op: proto.OpSync, Args: proto.MustRaw(proto.SyncArgs{})},
		{ID: 2, Op: proto.OpDeviceRevoke, Args: proto.MustRaw(proto.DeviceRevokeArgs{Revoke: proto.Signed(rev)})},
	}
	codec.Write(proto.TReq, reqs[0])
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
		if !r.OK {
			t.Fatalf("raw req %d: %v", r.ID, r.Err)
		}
		if r.ID == 2 {
			return
		}
		codec.Write(proto.TReq, reqs[1])
	}
}

// B12: the reconnect after revoke --self is intentional, so it mustn't grow
// the reconnect backoff: several in a row each come back within a second or
// so (it used to double each time: offline > 15 s after ~7 cycles).
func TestRevokeSelfRepeatedNoBackoff(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	eventually(t, "online", alice.online)
	sid := alice.sid(t)
	a := alice.Me().ID
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for i := range 7 {
		cur, err := alice.Trust().DeviceKeys(sid, a)
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "registered", func() bool {
			d, ok, _ := alice.Trust().Device(sid, cur.ID)
			return alice.online() && ok && !d.Revoked && alice.DeviceError() == nil
		})
		if _, err := alice.RevokeDevice(ctx, cur.ID, true); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		start := time.Now()
		next, err := alice.Trust().DeviceKeys(sid, a)
		if err != nil {
			t.Fatal(err)
		}
		for {
			d, ok, _ := alice.Trust().Device(sid, next.ID)
			if alice.online() && ok && !d.Revoked {
				break
			}
			if time.Since(start) > 3*time.Second {
				t.Fatalf("cycle %d: not back as a new device after %s", i, time.Since(start))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}
