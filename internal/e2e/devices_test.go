package e2e

import (
	"context"
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
