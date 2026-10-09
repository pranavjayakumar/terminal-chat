package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
)

func (u *user) sid(t *testing.T) e2ecrypto.ServerID {
	t.Helper()
	var sid e2ecrypto.ServerID
	eventually(t, "server id", func() bool {
		var ok bool
		sid, ok = u.ServerID()
		return ok
	})
	return sid
}

// verifiedDevices is how many of user's devices u's trust store verified.
func (u *user) verifiedDevices(sid e2ecrypto.ServerID, of uint64) int {
	ds, _ := u.Trust().Devices(sid, of)
	n := 0
	for _, d := range ds {
		if !d.Revoked {
			n++
		}
	}
	return n
}

func (u *user) epochKey(sid e2ecrypto.ServerID, ch uint64, epoch uint32) *e2ecrypto.Key {
	e, ok, _ := u.Trust().Epoch(sid, ch, epoch)
	if !ok {
		return nil
	}
	return e.Key
}

func (u *user) status(ch uint64) client.E2EStatus { return u.E2EStatus(ch) }

// openAs runs a second device of an existing user: a fresh TC_HOME with the
// same config (same SSH key), so a new device but no new login.
func openAs(t *testing.T, of *user) *user {
	t.Helper()
	home := t.TempDir()
	b, err := os.ReadFile(filepath.Join(of.Home(), "config.json"))
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
	u := &user{Client: c, cancel: cancel}
	t.Cleanup(func() { cancel(); c.Close() })
	return u
}

// A DM gets epoch 1 from its creator automatically; a second device of bob
// that joined later has no box, asks with key.need and gets K by key.share;
// then a rotation boxes every verified device.
func TestEpochKeysAcrossDevices(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	sid := alice.sid(t)
	a, b := alice.Me().ID, bob.Me().ID
	eventually(t, "devices verified both ways", func() bool {
		return alice.verifiedDevices(sid, b) == 1 && bob.verifiedDevices(sid, a) == 1 &&
			alice.verifiedDevices(sid, a) == 1
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dm, err := alice.OpenDM(ctx, []uint64{b})
	if err != nil {
		t.Fatal(err)
	}
	if !dm.E2E {
		t.Fatal("DM not encrypted")
	}
	eventually(t, "epoch 1 on both sides", func() bool {
		return alice.status(dm.ID).State == client.E2EOK && bob.status(dm.ID).State == client.E2EOK
	})
	k1 := alice.epochKey(sid, dm.ID, 1)
	if k1 == nil || bob.epochKey(sid, dm.ID, 1) == nil || *bob.epochKey(sid, dm.ID, 1) != *k1 {
		t.Fatal("alice and bob hold different epoch-1 keys")
	}
	if pend, _ := alice.Trust().PendingKeyChanges(sid); len(pend) != 0 {
		t.Fatalf("pending key changes: %+v", pend)
	}

	// bob's second device: verifies the chain, has no box, asks.
	bob2 := openAs(t, bob)
	eventually(t, "bob2 waits for a key", func() bool {
		st := bob2.status(dm.ID)
		return st.State == client.E2EWaitingKey && st.Epoch == 1 || st.State == client.E2EOK
	})
	eventually(t, "bob2 gets K by key.share", func() bool { return bob2.status(dm.ID).State == client.E2EOK })
	if k := bob2.epochKey(sid, dm.ID, 1); k == nil || *k != *k1 {
		t.Fatal("bob2 got a different K")
	}

	// A rotation now seals to all three devices; everyone agrees on K2.
	eventually(t, "alice verified bob2", func() bool { return alice.verifiedDevices(sid, b) == 2 })
	if err := alice.RotateKey(ctx, dm.ID); err != nil {
		t.Fatal(err)
	}
	for _, u := range []*user{alice, bob, bob2} {
		eventually(t, "epoch 2", func() bool { st := u.status(dm.ID); return st.State == client.E2EOK && st.Epoch == 2 })
	}
	k2 := alice.epochKey(sid, dm.ID, 2)
	for _, u := range []*user{bob, bob2} {
		if k := u.epochKey(sid, dm.ID, 2); k == nil || *k != *k2 {
			t.Fatal("epoch-2 keys differ")
		}
	}

	// An encrypted channel we create gets epoch 1 by itself, too.
	ch, err := bob.CreateEncryptedChannel(ctx, "sec", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "channel keyed", func() bool { return bob.status(ch.ID).State == client.E2EOK })
	if st := bob.status(ch.ID); st.Epoch != 1 {
		t.Fatalf("channel status %+v", st)
	}
}
