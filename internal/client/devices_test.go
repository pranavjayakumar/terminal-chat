package client

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

func testSID() e2ecrypto.ServerID {
	var sid e2ecrypto.ServerID
	copy(sid[:], serverID)
	return sid
}

// bobDevice is a device cert for bob, signed by a fresh key.
func bobDevice(t *testing.T) (proto.Signed, *e2ecrypto.DeviceKeys, ssh.Signer) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	d, err := e2ecrypto.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := identity.NewDeviceCert(key, testSID(), bob, d, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return proto.Signed(sc), d, key
}

func count(t *testing.T, c *Client, table string) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// noReq fails if fc receives a REQ for op within d.
func (fc *fakeConn) noReq(op string, d time.Duration) {
	fc.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-fc.in:
			if !ok {
				return
			}
			var r proto.Req
			if f.Type == proto.TReq && f.Decode(&r) == nil && r.Op == op {
				fc.t.Fatalf("unexpected REQ %s", op)
			}
		case <-deadline:
			return
		}
	}
}

// expectRegister checks the device.register REQ is a valid cert by alice's
// login key for this server and answers it, event first.
func (fc *fakeConn) expectRegister(fs *fakeServer, seq uint64) proto.Signed {
	fc.t.Helper()
	var a proto.DeviceRegisterArgs
	r := fc.expectReq(proto.OpDeviceRegister, &a)
	key := fs.hello.Load().(proto.Hello).Auth.Pubkey
	c, err := identity.Signed(a.Cert).VerifyDeviceCert(testSID(), key)
	if err != nil || c.User != alice {
		fc.t.Fatalf("registered cert: %+v %v", c, err)
	}
	fc.event(seq, proto.KDeviceNew, 0, alice, proto.Device{Cert: a.Cert})
	fc.ok(r.ID, proto.SeqRes{Seq: seq})
	return a.Cert
}

func TestDeviceRegisteredOnceAndEventsApplied(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	fs.caps.Store([]string{proto.CapE2E})
	home := login(t, fs)
	c := open(t, home)
	run(t, c)
	fc := fs.accept()
	if !slices.Contains(fs.hello.Load().(proto.Hello).Caps, proto.CapE2E) {
		t.Fatal("HELLO doesn't advertise e2e1")
	}

	// A reset snapshot in an e2e1 session carries every device, so the
	// client then only checks its own.
	bobCert, bobKeys, bobSSH := bobDevice(t)
	r := fc.expectReq(proto.OpSync, nil)
	fc.write(proto.TSnapshot, proto.Snapshot{Users: users,
		Chans: []proto.Channel{{ID: general, Name: "general", Kind: proto.ChanPublic, Members: []uint64{alice, bob}}},
		Devs:  []proto.Device{{Cert: bobCert}}})
	fc.ok(r.ID, proto.SyncRes{Head: 20, Reset: true})
	var la proto.DevicesListArgs
	r = fc.expectReq(proto.OpDevicesList, &la)
	if !slices.Equal(la.Users, []uint64{alice}) {
		t.Fatalf("devices.list users = %v, want just alice", la.Users)
	}
	fc.ok(r.ID, proto.DevicesListRes{})
	mine := fc.expectRegister(fs, 21)

	rev, err := identity.NewDeviceRevoke(bobSSH, testSID(), bob, bobKeys.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fc.event(22, proto.KDeviceRevoke, 0, bob, proto.DeviceRevokeArgs{Revoke: proto.Signed(rev)})
	fc.event(23, proto.KDeviceNew, 0, bob, proto.Device{Cert: proto.Signed{B: []byte{0xc1}}}) // garbage: dropped
	waitFor(t, "seq 23", func() bool { return lastSeq(t, c) == 23 })
	if n := count(t, c, "devices"); n != 2 {
		t.Fatalf("%d devices cached, want 2", n)
	}
	var revDev []byte
	if err := c.db.QueryRow(`SELECT dev FROM device_revokes WHERE user = ?`, bob).Scan(&revDev); err != nil ||
		!bytes.Equal(revDev, bobKeys.ID[:]) {
		t.Fatalf("revoke not cached: %x %v", revDev, err)
	}

	st, err := os.Stat(filepath.Join(home, deviceKeyFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("device.key: %v %v", st, err)
	}

	// Reconnect: the server already has our device, so no second register.
	fc.conn.Close()
	fc2 := fs.accept()
	var sa proto.SyncArgs
	r = fc2.expectReq(proto.OpSync, &sa)
	if sa.Since != 23 {
		t.Fatalf("resume since %d", sa.Since)
	}
	fc2.ok(r.ID, proto.SyncRes{Head: 23})
	r = fc2.expectReq(proto.OpDevicesList, &la)
	fc2.ok(r.ID, proto.DevicesListRes{Certs: []proto.Signed{mine}})
	fc2.noReq(proto.OpDeviceRegister, 300*time.Millisecond)

	// A restarted process reuses the same device keys.
	keys1, err := loadDeviceKeys(home, testSID(), alice, nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := identity.PeekDeviceCert(identity.Signed(mine))
	if keys1.ID != cert.Dev {
		t.Fatal("device key changed")
	}
}

// A cache from before e2e1 (or from a server without it) gets the full,
// paged device list on its first e2e1 connection.
func TestDeviceFullListAfterUpgrade(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	home := login(t, fs)
	c := open(t, home)
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))
	fc.noReq(proto.OpDevicesList, 200*time.Millisecond) // no e2e1: nothing
	if _, err := os.Stat(filepath.Join(home, deviceKeyFile)); !os.IsNotExist(err) {
		t.Fatalf("device.key without e2e1: %v", err)
	}

	fs.caps.Store([]string{proto.CapE2E})
	fc.conn.Close()
	fc2 := fs.accept()
	r := fc2.expectReq(proto.OpSync, nil)
	fc2.ok(r.ID, proto.SyncRes{Head: 20})
	bobCert, _, _ := bobDevice(t)
	var la proto.DevicesListArgs
	r = fc2.expectReq(proto.OpDevicesList, &la)
	if len(la.Users) != 0 || la.After != 0 {
		t.Fatalf("first list = %+v, want everyone", la)
	}
	fc2.ok(r.ID, proto.DevicesListRes{Certs: []proto.Signed{bobCert}, Next: bob})
	r = fc2.expectReq(proto.OpDevicesList, &la)
	if la.After != bob {
		t.Fatalf("second page after = %d", la.After)
	}
	other, _, _ := bobDevice(t)
	fc2.ok(r.ID, proto.DevicesListRes{Certs: []proto.Signed{other}})
	fc2.expectRegister(fs, 21)
	waitFor(t, "seq 21", func() bool { return lastSeq(t, c) == 21 })
	if n := count(t, c, "devices"); n != 3 {
		t.Fatalf("%d devices cached, want 3", n)
	}
	if full, _ := metaUint(c.db, "devices_full"); full != 1 {
		t.Fatal("devices_full not set after a complete list")
	}
}

func TestDeviceKeyFileReplacedForOtherLogin(t *testing.T) {
	home := t.TempDir()
	a, err := loadDeviceKeys(home, testSID(), alice, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := loadDeviceKeys(home, testSID(), alice, nil)
	if err != nil || again.ID != a.ID {
		t.Fatalf("reload: %v", err)
	}
	other, err := loadDeviceKeys(home, e2ecrypto.ServerID{1}, alice, nil)
	if err != nil || other.ID == a.ID {
		t.Fatalf("other server got the same device: %v", err)
	}
	b, err := loadDeviceKeys(home, e2ecrypto.ServerID{1}, bob, nil)
	if err != nil || b.ID == other.ID {
		t.Fatalf("other user got the same device: %v", err)
	}
}

// A device whose revoke we can verify ourselves is retired: the client
// re-keys and registers as a new device. A revoke we can't check is ignored.
func TestRevokedDeviceReKeys(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	fs.caps.Store([]string{proto.CapE2E})
	home := login(t, fs)
	c := open(t, home)
	run(t, c)
	fc := fs.accept()
	r := fc.expectReq(proto.OpSync, nil)
	fc.ok(r.ID, proto.SyncRes{Head: 20, Reset: true})
	r = fc.expectReq(proto.OpDevicesList, nil)
	fc.ok(r.ID, proto.DevicesListRes{})
	mine := fc.expectRegister(fs, 21)
	old, _ := identity.PeekDeviceCert(identity.Signed(mine))

	// Forged: "revoked" by a key that isn't ours.
	_, _, strangerSSH := bobDevice(t)
	fake, _ := identity.NewDeviceRevoke(strangerSSH, testSID(), alice, old.Dev, time.Now())
	fc.conn.Close()
	fc = fs.accept()
	r = fc.expectReq(proto.OpSync, nil)
	fc.ok(r.ID, proto.SyncRes{Head: 21})
	r = fc.expectReq(proto.OpDevicesList, nil)
	fc.ok(r.ID, proto.DevicesListRes{Certs: []proto.Signed{mine}, Revoked: []proto.Signed{proto.Signed(fake)}})
	fc.noReq(proto.OpDeviceRegister, 300*time.Millisecond)

	// Real: signed by our own SSH key.
	signer, err := identity.Load(c.Config().KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := identity.NewDeviceRevoke(signer, testSID(), alice, old.Dev, time.Now())
	fc.conn.Close()
	fc = fs.accept()
	r = fc.expectReq(proto.OpSync, nil)
	fc.ok(r.ID, proto.SyncRes{Head: 21})
	r = fc.expectReq(proto.OpDevicesList, nil)
	fc.ok(r.ID, proto.DevicesListRes{Certs: []proto.Signed{mine}, Revoked: []proto.Signed{proto.Signed(real)}})
	fresh := fc.expectRegister(fs, 22)
	nc, _ := identity.PeekDeviceCert(identity.Signed(fresh))
	if nc.Dev == old.Dev || nc.Ed25519.Equal(old.Ed25519) {
		t.Fatal("revoked device registered again with its old keys")
	}
	if k, _ := loadDeviceKeys(home, testSID(), alice, nil); k.ID != nc.Dev {
		t.Fatal("new keys not persisted")
	}
}

// After a session without e2e1 (device events skipped), the next e2e1
// session lists everyone again even if a full list was cached before.
func TestDevicesFullClearedByNonE2ESession(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	fs.caps.Store([]string{proto.CapE2E})
	home := login(t, fs)
	c := open(t, home)
	run(t, c)
	fc := fs.accept()
	r := fc.expectReq(proto.OpSync, nil)
	fc.ok(r.ID, proto.SyncRes{Head: 20, Reset: true})
	var la proto.DevicesListArgs
	r = fc.expectReq(proto.OpDevicesList, &la)
	if len(la.Users) != 1 {
		t.Fatalf("after e2e1 reset: list %+v", la)
	}
	fc.ok(r.ID, proto.DevicesListRes{})
	fc.expectRegister(fs, 21)
	waitFor(t, "seq 21", func() bool { return lastSeq(t, c) == 21 })

	fs.caps.Store([]string(nil))
	fc.conn.Close()
	fc = fs.accept()
	r = fc.expectReq(proto.OpSync, nil)
	fc.ok(r.ID, proto.SyncRes{Head: 30})
	waitFor(t, "seq 30", func() bool { return lastSeq(t, c) == 30 })

	fs.caps.Store([]string{proto.CapE2E})
	fc.conn.Close()
	fc = fs.accept()
	r = fc.expectReq(proto.OpSync, nil)
	fc.ok(r.ID, proto.SyncRes{Head: 30})
	var again proto.DevicesListArgs
	r = fc.expectReq(proto.OpDevicesList, &again)
	if len(again.Users) != 0 {
		t.Fatalf("after a non-e2e1 session: list %+v, want everyone", again)
	}
}

func TestDeviceKeysConcurrentFirstUse(t *testing.T) {
	home := t.TempDir()
	ids := make(chan e2ecrypto.DevID, 8)
	for range 8 {
		go func() {
			k, err := loadDeviceKeys(home, testSID(), alice, nil)
			if err != nil {
				t.Error(err)
				ids <- e2ecrypto.DevID{}
				return
			}
			ids <- k.ID
		}()
	}
	first := <-ids
	for range 7 {
		if id := <-ids; id != first {
			t.Fatal("concurrent first use produced different device keys")
		}
	}
}
