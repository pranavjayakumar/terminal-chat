package e2e

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
)

// B2: a thief holding alice's SSH key revokes her desktop while it's
// offline and registers a device of its own. When the desktop reconnects it
// re-registers under a new id first; the thief's device must still be
// announced as a new device of hers in that pass, and the desktop's old id
// must read as this device's (Former), not as some other device of hers.
func TestOfflineRevokedDeviceAnnouncesNewDevice(t *testing.T) {
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
	old, err := alice.Trust().DeviceKeys(sid, a)
	if err != nil {
		t.Fatal(err)
	}
	alice.cancel()
	alice.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := laptop.RevokeDevice(ctx, old.ID, false); err != nil {
		t.Fatal(err)
	}
	thief := secondDevice(t, laptop)
	eventually(t, "thief's device registered", func() bool { return thief.online() && laptop.verifiedDevices(sid, a) == 2 })
	tk, err := thief.Trust().DeviceKeys(sid, a)
	if err != nil {
		t.Fatal(err)
	}

	desk, err := client.Open(alice.home)
	if err != nil {
		t.Fatal(err)
	}
	newOwn := make(chan client.DeviceRef, 8)
	revoked := make(chan client.DeviceRef, 8)
	go func() {
		for u := range desk.Updates() {
			for _, r := range u.NewOwn {
				newOwn <- r
			}
			for _, r := range u.Revoked {
				revoked <- r
			}
		}
	}()
	dctx, dcancel := context.WithCancel(context.Background())
	go desk.Run(dctx)
	t.Cleanup(func() { dcancel(); desk.Close() })

	var sawNew, sawRevoke bool
	for timeout := time.After(15 * time.Second); !sawNew || !sawRevoke; {
		select {
		case r := <-newOwn:
			if r.Dev == old.ID {
				t.Fatalf("own former id announced as new: %+v", r)
			}
			sawNew = sawNew || r.Dev == tk.ID
		case r := <-revoked:
			sawRevoke = sawRevoke || r.Dev == old.ID
		case <-timeout:
			t.Fatalf("desktop: thief's device announced %v, own revoke reported %v", sawNew, sawRevoke)
		}
	}
	devs := desk.Devices(a)
	i := slices.IndexFunc(devs, func(d client.DeviceInfo) bool { return d.ID == old.ID })
	if i < 0 || !devs[i].Former || devs[i].This || !devs[i].Revoked {
		t.Fatalf("desktop's old id: %+v", devs)
	}
	if devs[0].This == false || devs[0].ID == old.ID {
		t.Fatalf("this device: %+v", devs)
	}
}
