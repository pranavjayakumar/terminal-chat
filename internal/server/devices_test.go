package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"slices"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

var e2eCaps = []string{proto.CapE2E}

func newDevKeys(t *testing.T) *e2ecrypto.DeviceKeys {
	t.Helper()
	d, err := e2ecrypto.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// certFor makes a device cert for user signed by key on server sid.
func certFor(t *testing.T, key ssh.Signer, sid e2ecrypto.ServerID, user uint64) (proto.Signed, *e2ecrypto.DeviceKeys) {
	t.Helper()
	d := newDevKeys(t)
	sc, err := identity.NewDeviceCert(key, sid, user, d, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return proto.Signed(sc), d
}

func (c *tclient) registerDevice(ts *testServer) (*e2ecrypto.DeviceKeys, proto.Signed) {
	c.t.Helper()
	cert, d := certFor(c.t, c.signer, ts.sid(), c.user.ID)
	var r proto.SeqRes
	c.ok(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: cert}, &r)
	if r.Seq == 0 {
		c.t.Fatal("register: seq 0")
	}
	return d, cert
}

func revokeBySSH(t *testing.T, key ssh.Signer, sid e2ecrypto.ServerID, user uint64, dev e2ecrypto.DevID) proto.Signed {
	t.Helper()
	r, err := identity.NewDeviceRevoke(key, sid, user, dev, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return proto.Signed(r)
}

func revokeByDev(t *testing.T, by *e2ecrypto.DeviceKeys, sid e2ecrypto.ServerID, user uint64, dev e2ecrypto.DevID) proto.Signed {
	t.Helper()
	r, err := identity.NewDeviceRevokeByDevice(by, sid, user, dev, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return proto.Signed(r)
}

func decodeDevice(t *testing.T, ev proto.Event) identity.DeviceCert {
	t.Helper()
	var d proto.Device
	if err := proto.Unmarshal(ev.Data, &d); err != nil {
		t.Fatal(err)
	}
	c, err := identity.PeekDeviceCert(identity.Signed(d.Cert))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDeviceLimitsMatchE2ECrypto(t *testing.T) {
	for _, p := range [][2]int{
		{proto.MaxSigned, e2ecrypto.MaxDeviceCertSize},
		{proto.MaxSigned, identity.MaxSignedSize},
		{proto.MaxDevices, e2ecrypto.MaxDevicesPerUser},
	} {
		if p[0] != p[1] {
			t.Fatalf("proto device limit %d drifted from e2ecrypto's %d", p[0], p[1])
		}
	}
	// One user's devices must always fit one devices.list RES.
	if proto.MaxDeviceRegistrations*3*proto.MaxSigned+64<<10 > proto.MaxResultBytes {
		t.Fatal("MaxDeviceRegistrations too large for one devices.list page")
	}
}

func TestWelcomeAdvertisesE2E(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	if !slices.Contains(alice.welcome.Caps, proto.CapHooks) || !slices.Contains(alice.welcome.Caps, proto.CapE2E) {
		t.Fatalf("welcome caps = %v", alice.welcome.Caps)
	}
}

func TestDeviceRegister(t *testing.T) {
	forEachTransport(t, func(t *testing.T, ts *testServer) {
		alice := ts.registerCaps(t, "alice", true, e2eCaps)
		bob := ts.registerCaps(t, "bob", false, e2eCaps)
		old := ts.register(t, "old", false) // no e2e1
		alice.syncFresh()
		bob.syncFresh()
		old.syncFresh()

		cert, d := certFor(t, alice.signer, ts.sid(), alice.user.ID)
		var r proto.SeqRes
		alice.ok(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: cert}, &r)
		// The caller's event comes before its RES (held by ok) and matches the seq.
		ev := alice.event(proto.KDeviceNew)
		if ev.Seq != r.Seq || ev.Actor != alice.user.ID || ev.Chan != 0 {
			t.Fatalf("device.new = %+v, res seq %d", ev, r.Seq)
		}
		if c := decodeDevice(t, bob.event(proto.KDeviceNew)); c.Dev != d.ID || c.User != alice.user.ID {
			t.Fatalf("bob got device %+v", c)
		}
		old.quiet(150 * time.Millisecond)

		// Exact repeat is a no-op; same id with another cert is a conflict.
		alice.ok(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: cert}, &r)
		if r.Seq != 0 {
			t.Fatalf("repeat seq = %d", r.Seq)
		}
		other, err := identity.NewDeviceCert(alice.signer, ts.sid(), alice.user.ID, d, time.Now().Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		alice.fail(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: proto.Signed(other)}, proto.ErrConflict)
		bob.quiet(100 * time.Millisecond)

		var list proto.DevicesListRes
		bob.ok(proto.OpDevicesList, proto.DevicesListArgs{}, &list)
		if len(list.Certs) != 1 || !bytes.Equal(list.Certs[0].B, cert.B) || len(list.Revoked) != 0 || list.Next != 0 {
			t.Fatalf("list = %+v", list)
		}
		bob.ok(proto.OpDevicesList, proto.DevicesListArgs{Users: []uint64{bob.user.ID}}, &list)
		if len(list.Certs) != 0 {
			t.Fatalf("bob's list = %+v", list)
		}

		// Without e2e1 the ops don't exist.
		old.fail(proto.OpDevicesList, proto.DevicesListArgs{}, proto.ErrUnknownOp)
		oc, _ := certFor(t, old.signer, ts.sid(), old.user.ID)
		old.fail(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: oc}, proto.ErrUnknownOp)
	})
}

func TestDeviceRegisterRejects(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	alice.syncFresh()
	bob.syncFresh()
	sid := ts.sid()
	reg := func(cert proto.Signed, code string) {
		t.Helper()
		alice.fail(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: cert}, code)
	}

	wrongServer, _ := certFor(t, alice.signer, e2ecrypto.ServerID{0xee}, alice.user.ID)
	reg(wrongServer, proto.ErrBadRequest)

	bobs, _ := certFor(t, bob.signer, sid, bob.user.ID)
	reg(bobs, proto.ErrForbidden) // bob's own cert, replayed by alice
	forBob, _ := certFor(t, alice.signer, sid, bob.user.ID)
	reg(forBob, proto.ErrForbidden) // alice's key, bob's user id

	stranger, _ := certFor(t, newKey(t), sid, alice.user.ID)
	reg(stranger, proto.ErrForbidden) // unregistered SSH key

	good, _ := certFor(t, alice.signer, sid, alice.user.ID)
	badSig := proto.Signed{B: good.B, Sig: bytes.Clone(good.Sig)}
	badSig.Sig[len(badSig.Sig)-1] ^= 1
	reg(badSig, proto.ErrBadRequest)
	badB := proto.Signed{B: bytes.Clone(good.B), Sig: good.Sig}
	badB.B[len(badB.B)-1] ^= 1 // last byte of `created`: still decodes
	reg(badB, proto.ErrBadRequest)
	reg(proto.Signed{B: []byte{0xc1}, Sig: good.Sig}, proto.ErrBadRequest)
	reg(proto.Signed{B: make([]byte, proto.MaxSigned), Sig: good.Sig}, proto.ErrTooLarge)
	alice.fail(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: good,
		Endorse: &proto.Signed{B: make([]byte, proto.MaxSigned+1)}}, proto.ErrTooLarge)

	// Endorsements: must be by one of alice's keys, for alice and this cert's key.
	newKeySigner := newKey(t)
	wrongEndorse, err := identity.NewKeyEndorse(alice.signer, sid, alice.user.ID, newKeySigner.PublicKey(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e := proto.Signed(wrongEndorse)
	alice.fail(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: good, Endorse: &e}, proto.ErrBadRequest)
	strangerEndorse, _ := identity.NewKeyEndorse(newKey(t), sid, alice.user.ID, alice.signer.PublicKey(), time.Now())
	e = proto.Signed(strangerEndorse)
	alice.fail(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: good, Endorse: &e}, proto.ErrBadRequest)
	selfEndorse, _ := identity.NewKeyEndorse(alice.signer, sid, alice.user.ID, alice.signer.PublicKey(), time.Now())
	e = proto.Signed(selfEndorse)
	alice.fail(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: good, Endorse: &e}, proto.ErrBadRequest)

	bob.quiet(100 * time.Millisecond) // nothing above produced an event

	// The same device keys under another device id: conflict.
	dk, _ := alice.registerDevice(ts)
	bob.event(proto.KDeviceNew)
	twin := *dk
	twin.ID[0] ^= 0xff
	tc, err := identity.NewDeviceCert(alice.signer, sid, alice.user.ID, &twin, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reg(proto.Signed(tc), proto.ErrConflict)

	// A second registered key (add-key invite) can sign certs, and an
	// endorsement by the first key travels with the device.
	code, _ := ts.CreateInvite(context.Background(), false, alice.user.ID)
	inv, _ := proto.ParseInvite(code)
	laptop, bye := ts.connectCaps(t, newKeySigner, inv.Token, "", e2eCaps)
	if bye != "" {
		t.Fatal(bye)
	}
	laptop.syncFresh()
	lc, _ := certFor(t, newKeySigner, sid, alice.user.ID)
	endorse, _ := identity.NewKeyEndorse(alice.signer, sid, alice.user.ID, newKeySigner.PublicKey(), time.Now())
	e = proto.Signed(endorse)
	laptop.ok(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: lc, Endorse: &e}, nil)
	var d proto.Device
	proto.Unmarshal(bob.event(proto.KDeviceNew).Data, &d)
	if d.Endorse == nil || !bytes.Equal(d.Endorse.B, e.B) {
		t.Fatalf("endorse not forwarded: %+v", d)
	}
	var list proto.DevicesListRes
	bob.ok(proto.OpDevicesList, proto.DevicesListArgs{Users: []uint64{alice.user.ID}}, &list)
	if len(list.Certs) != 2 || len(list.Endorse) != 1 {
		t.Fatalf("list: %d certs, %d endorse", len(list.Certs), len(list.Endorse))
	}

	// At most 8 active devices: 2 so far, 6 more fit, the 9th doesn't.
	var devs []*e2ecrypto.DeviceKeys
	for range proto.MaxDevices - 2 {
		k, _ := alice.registerDevice(ts)
		devs = append(devs, k)
	}
	ninth, _ := certFor(t, alice.signer, sid, alice.user.ID)
	reg(ninth, proto.ErrBadRequest)
	// Revoking one frees a slot.
	alice.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, alice.signer, sid, alice.user.ID, devs[0].ID)}, nil)
	alice.ok(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: ninth}, nil)
}

func TestDeviceRegistrationLifetimeCap(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	alice.syncFresh()
	sid := ts.sid()
	for range proto.MaxDeviceRegistrations {
		d, _ := alice.registerDevice(ts)
		alice.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, alice.signer, sid, alice.user.ID, d.ID)}, nil)
	}
	c, _ := certFor(t, alice.signer, sid, alice.user.ID)
	alice.fail(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: c}, proto.ErrBadRequest)
}

func TestDeviceRevoke(t *testing.T) {
	forEachTransport(t, func(t *testing.T, ts *testServer) {
		alice := ts.registerCaps(t, "alice", true, e2eCaps)
		bob := ts.registerCaps(t, "bob", false, e2eCaps)
		old := ts.register(t, "old", false)
		alice.syncFresh()
		bob.syncFresh()
		old.syncFresh()
		sid := ts.sid()
		a1, _ := alice.registerDevice(ts)
		a2, _ := alice.registerDevice(ts)
		a3, _ := alice.registerDevice(ts)
		b1, _ := bob.registerDevice(ts)
		for range 4 {
			bob.event(proto.KDeviceNew)
		}

		// Not alice's to revoke, or not signed by her.
		alice.fail(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, alice.signer, sid, bob.user.ID, b1.ID)}, proto.ErrForbidden)
		alice.fail(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, bob.signer, sid, bob.user.ID, b1.ID)}, proto.ErrForbidden)
		alice.fail(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeByDev(t, b1, sid, alice.user.ID, a1.ID)}, proto.ErrForbidden)
		alice.fail(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, newKey(t), sid, alice.user.ID, a1.ID)}, proto.ErrForbidden)
		alice.fail(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, alice.signer, e2ecrypto.ServerID{1}, alice.user.ID, a1.ID)}, proto.ErrBadRequest)
		alice.fail(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, alice.signer, sid, alice.user.ID, e2ecrypto.DevID{9})}, proto.ErrNotFound)
		// A device can't revoke itself (it must be another device).
		alice.fail(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeByDev(t, a1, sid, alice.user.ID, a1.ID)}, proto.ErrForbidden)
		big := revokeBySSH(t, alice.signer, sid, alice.user.ID, a1.ID)
		big.Sig = make([]byte, proto.MaxSigned)
		alice.fail(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: big}, proto.ErrTooLarge)
		bob.quiet(100 * time.Millisecond)

		// By SSH key.
		rv := revokeBySSH(t, alice.signer, sid, alice.user.ID, a1.ID)
		var r proto.SeqRes
		alice.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: rv}, &r)
		if ev := alice.event(proto.KDeviceRevoke); ev.Seq != r.Seq {
			t.Fatalf("own revoke event seq %d, res %d", ev.Seq, r.Seq)
		}
		var got proto.DeviceRevokeArgs
		proto.Unmarshal(bob.event(proto.KDeviceRevoke).Data, &got)
		if !bytes.Equal(got.Revoke.B, rv.B) || !bytes.Equal(got.Revoke.Sig, rv.Sig) {
			t.Fatal("bob got a different revoke")
		}
		// Again: a no-op.
		alice.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: rv}, &r)
		if r.Seq != 0 {
			t.Fatalf("repeat revoke seq %d", r.Seq)
		}

		// By another device.
		alice.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeByDev(t, a3, sid, alice.user.ID, a2.ID)}, nil)
		bob.event(proto.KDeviceRevoke)
		// A revoked device can't revoke.
		alice.fail(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeByDev(t, a2, sid, alice.user.ID, a3.ID)}, proto.ErrForbidden)
		alice.fail(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeByDev(t, a1, sid, alice.user.ID, a3.ID)}, proto.ErrForbidden)
		old.quiet(100 * time.Millisecond)

		var list proto.DevicesListRes
		bob.ok(proto.OpDevicesList, proto.DevicesListArgs{}, &list)
		if len(list.Certs) != 4 || len(list.Revoked) != 2 {
			t.Fatalf("list: %d certs, %d revoked", len(list.Certs), len(list.Revoked))
		}
	})
}

// Snapshots carry devices only for e2e1 sessions, replays carry device
// events only for them, and an old client syncs exactly as before.
func TestDeviceSyncGatedOnCap(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	snap, sr := alice.syncFresh()
	gen := generalID(t, snap)
	d1, _ := alice.registerDevice(ts)
	alice.registerDevice(ts)
	alice.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, alice.signer, ts.sid(), alice.user.ID, d1.ID)}, nil)
	alice.send(gen, "hello")

	// Fresh sync.
	c, _ := ts.connectCaps(t, alice.signer, "", "", e2eCaps)
	s2, _ := c.syncFresh()
	if len(s2.Devs) != 2 || len(s2.Revoked) != 1 {
		t.Fatalf("e2e snapshot: %d devs, %d revoked", len(s2.Devs), len(s2.Revoked))
	}
	s3, _ := alice.syncFreshAgain(t, ts) // no caps
	if len(s3.Devs) != 0 || len(s3.Revoked) != 0 || len(s3.Msgs) != 1 || len(s3.Chans) != len(s2.Chans) {
		t.Fatalf("old snapshot: %+v", s3)
	}

	// Replay.
	kinds := func(frames []proto.Frame) (dev, msg int) {
		for _, f := range frames {
			var ev proto.Event
			f.Decode(&ev)
			if isDeviceKind(ev.Kind) {
				dev++
			}
			if ev.Kind == proto.KMsgNew {
				msg++
			}
		}
		return
	}
	if dev, msg := kinds(alice.replayFrom(t, ts, sr.Head)); dev != 0 || msg != 1 {
		t.Fatalf("old replay: %d device events, %d msgs", dev, msg)
	}
	c2, _ := ts.connectCaps(t, alice.signer, "", "", e2eCaps)
	c2.nextID++
	c2.codec.Write(proto.TReq, proto.Req{ID: c2.nextID, Op: proto.OpSync,
		Args: proto.MustRaw(proto.SyncArgs{Since: sr.Head, ServerID: ts.st.serverID})})
	var frames []proto.Frame
	for done := false; !done; {
		f := c2.recv("replay")
		switch f.Type {
		case proto.TEvent:
			frames = append(frames, f)
		case proto.TRes:
			done = true
		}
	}
	if dev, msg := kinds(frames); dev != 3 || msg != 1 {
		t.Fatalf("e2e replay: %d device events, %d msgs", dev, msg)
	}
}

func TestDevicesListPages(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	alice.syncFresh()
	// Fake bulky rows straight into the store: 3 users × ~700 KiB.
	var ids []uint64
	for i := range 3 {
		u := ts.registerCaps(t, "u"+string(rune('a'+i)), false, e2eCaps)
		ids = append(ids, u.user.ID)
		for j := range 24 {
			blob := proto.MustRaw(proto.Signed{B: make([]byte, 30000), Sig: []byte{byte(j)}})
			id := []byte{byte(i), byte(j)}
			if _, err := ts.st.db.Exec(`INSERT INTO devices (dev, user, x25519, ed25519, cert, created) VALUES (?, ?, ?, ?, ?, ?)`,
				id, u.user.ID, append([]byte{'x'}, id...), append([]byte{'e'}, id...), blob, j); err != nil {
				t.Fatal(err)
			}
		}
	}
	total, after := 0, uint64(0)
	pages := 0
	for {
		var list proto.DevicesListRes
		alice.ok(proto.OpDevicesList, proto.DevicesListArgs{After: after}, &list)
		pages++
		total += len(list.Certs)
		if list.Next == 0 {
			break
		}
		after = list.Next
	}
	if total != 72 || pages < 2 {
		t.Fatalf("got %d certs in %d pages", total, pages)
	}
	var one proto.DevicesListRes
	alice.ok(proto.OpDevicesList, proto.DevicesListArgs{Users: []uint64{ids[1]}}, &one)
	if len(one.Certs) != 24 || one.Next != 0 {
		t.Fatalf("one user: %d certs next %d", len(one.Certs), one.Next)
	}
	// A filtered list pages too, and skips users that aren't asked for.
	var two proto.DevicesListRes
	alice.ok(proto.OpDevicesList, proto.DevicesListArgs{Users: []uint64{ids[0], ids[2]}}, &two)
	if len(two.Certs) != 24 || two.Next != ids[0] {
		t.Fatalf("filtered page 1: %d certs next %d", len(two.Certs), two.Next)
	}
	var rest proto.DevicesListRes
	alice.ok(proto.OpDevicesList, proto.DevicesListArgs{Users: []uint64{ids[0], ids[2]}, After: two.Next}, &rest)
	if len(rest.Certs) != 24 || rest.Next != 0 {
		t.Fatalf("filtered page 2: %d certs next %d", len(rest.Certs), rest.Next)
	}
	alice.fail(proto.OpDevicesList, proto.DevicesListArgs{Users: make([]uint64, maxListUsers+1)}, proto.ErrBadRequest)
}

func TestDevicesListRejectsOutOfRangeIDs(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	alice.syncFresh()
	alice.fail(proto.OpDevicesList, proto.DevicesListArgs{After: 1 << 63}, proto.ErrBadRequest)
	alice.fail(proto.OpDevicesList, proto.DevicesListArgs{Users: []uint64{1 << 63}}, proto.ErrBadRequest)
}

// A member with a low-order X25519 key could never be sealed to, and every
// rotation must box all members' devices: the server refuses such certs.
func TestDeviceRegisterRejectsLowOrderX25519(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	alice.syncFresh()
	for _, k := range [][32]byte{{}, {1}} {
		d := newDevKeys(t)
		d.X25519Pub = k
		sc, err := identity.NewDeviceCert(alice.signer, ts.sid(), alice.user.ID, d, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		alice.fail(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: proto.Signed(sc)}, proto.ErrBadRequest)
	}
}
