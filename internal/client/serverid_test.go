package client

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/trust"
)

// The first server_id seen at the config's address and TLS pin is pinned; a
// WELCOME with another id stops Run for good without touching trust, and
// only `tc login` re-trusts.
func TestServerIdentityChangeStops(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	home := login(t, fs)
	c := open(t, home)
	stopped := run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))
	cfg := c.Config()
	ts := c.Trust()
	if sid, ok, err := ts.ServerIdentity(cfg.Addr, cfg.Pin); err != nil || !ok || !bytes.Equal(sid[:], serverID) {
		t.Fatalf("pinned identity %x %v %v", sid, ok, err)
	}
	bobKey := newSSH(t)
	fp := ssh.FingerprintSHA256(bobKey.PublicKey())
	if err := ts.PinKey(testSID(), bob, fp, bobKey.PublicKey().Marshal(), trust.TOFU); err != nil {
		t.Fatal(err)
	}

	other := bytes.Repeat([]byte{9}, 16)
	fs.sid.Store(other)
	fc.conn.Close()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept going with another server identity")
	}
	fs.accept() // the refused connection
	if st := c.Status(); !st.Stopped || st.Err != "server identity changed — log in again to trust it" {
		t.Fatalf("status %+v", st)
	}
	if sid, _, _ := ts.ServerIdentity(cfg.Addr, cfg.Pin); !bytes.Equal(sid[:], serverID) {
		t.Fatal("pinned identity replaced")
	}
	if pins, _ := ts.PinnedKeys(testSID(), bob); len(pins) != 1 {
		t.Fatalf("trust touched: %+v", pins)
	}
	if got, _ := c.ServerID(); !bytes.Equal(got[:], serverID) {
		t.Fatal("cache reset to the new server")
	}

	// An explicit login trusts the new identity.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Login(ctx, home, proto.FormatInvite(fs.addr(), "tok", fs.pin), "alice", cfg.KeyPath); err != nil {
		t.Fatal(err)
	}
	if sid, _, _ := ts.ServerIdentity(cfg.Addr, cfg.Pin); !bytes.Equal(sid[:], other) {
		t.Fatalf("login didn't re-trust: %x", sid)
	}
	c2 := open(t, home)
	run(t, c2)
	fc = fs.accept()
	fc.bootstrap()
	waitFor(t, "online after login", online(c2))
}

// A server once seen with e2e1 that stops advertising it raises a sticky
// alarm, which every encrypted channel's status reflects.
func TestServerE2EDowngradeAlarm(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)}, Devs: []proto.Device{{Cert: b1.cert}}}, proto.DevicesListRes{})
	f.myKeys(c)
	waitFor(t, "server marked e2e", func() bool { ok, _ := c.Trust().ServerUsesE2E(testSID()); return ok })
	if _, ok := c.TrustAlarm(); ok {
		t.Fatal("alarm before any downgrade")
	}

	fs.caps.Store([]string(nil))
	f.fc.conn.Close()
	fc := fs.accept()
	r := fc.expectReq(proto.OpSync, nil)
	fc.ok(r.ID, proto.SyncRes{Head: 21})
	waitFor(t, "trust alarm", func() bool { _, ok := c.TrustAlarm(); return ok })
	if st := c.E2EStatus(dm); st.State != E2EAlarm || st.Detail != "the server stopped offering end-to-end encryption" {
		t.Fatalf("dm status %+v", st)
	}
	if st := c.E2EStatus(general); st.State != E2EOff {
		t.Fatalf("plaintext channel status %+v", st)
	}
	if detail, ok, _ := c.Trust().ServerAlarm(e2ecrypto.ServerID(serverID)); !ok || detail == "" {
		t.Fatal("alarm not persisted")
	}
}

// Nothing goes out as plaintext into an encrypted channel: a message queued
// before the server dropped e2e1 fails instead of being sent, and new sends
// are refused while the downgrade alarm stands.
func TestPlaintextIntoE2ERefused(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)}, Devs: []proto.Device{{Cert: b1.cert}}}, proto.DevicesListRes{})
	f.myKeys(c)
	waitFor(t, "server marked e2e", func() bool { ok, _ := c.Trust().ServerUsesE2E(testSID()); return ok })

	fs.caps.Store([]string(nil))
	f.fc.conn.Close()
	waitFor(t, "offline", func() bool { return c.Status().State != Online })
	if err := c.Send(dm, "secret plan", 0); err != nil {
		t.Fatal(err)
	}
	fc := fs.accept()
	r := fc.expectReq(proto.OpSync, nil)
	fc.ok(r.ID, proto.SyncRes{Head: 21})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Flush(ctx); err == nil || !strings.Contains(err.Error(), "stopped offering end-to-end encryption") {
		t.Fatalf("flush: %v", err)
	}
	if err := c.Send(dm, "more", 0); !errors.Is(err, ErrE2EAlarm) {
		t.Fatalf("send after the alarm: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	for len(fc.in) > 0 {
		var req proto.Req
		if f := <-fc.in; f.Type == proto.TReq && f.Decode(&req) == nil && req.Op == proto.OpSend {
			t.Fatalf("sent %s", req.Args)
		}
	}
}
