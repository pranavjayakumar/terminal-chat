package e2e

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/identity"
)

// addKeyDevice signs a new computer of of's in with a key of its own,
// through an add-key invite (#59), and returns it and its key's path.
func addKeyDevice(t *testing.T, s *srv, of *user) (*user, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code, err := s.CreateInvite(ctx, false, of.Me().ID)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	keyPath := writeKey(t, t.TempDir())
	if u, err := client.Login(ctx, home, code, "ignored", keyPath); err != nil || u.ID != of.Me().ID {
		t.Fatalf("add-key login: %+v %v", u, err)
	}
	c, err := client.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(context.Background())
	go c.Run(rctx)
	t.Cleanup(func() { rcancel(); c.Close() })
	return &user{Client: c, cancel: rcancel, home: home}, keyPath
}

func keyFP(t *testing.T, path string) string {
	t.Helper()
	s, err := identity.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return identity.Fingerprint(s.PublicKey())
}

// #146: alice's laptop joins with its own SSH key (add-key invite). Until a
// computer holding a key alice's devices trust vouches for it, its messages
// stay untrusted everywhere and nothing is sealed to it; once alice
// endorses it (and the laptop endorses her desktop's key back), everyone
// trusts it without a prompt, and the laptop reads and sends as alice.
func TestOwnKeyEndorsement(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	sid := alice.sid(t)
	a, b := alice.Me().ID, bob.Me().ID
	eventually(t, "devices verified", func() bool {
		return alice.verifiedDevices(sid, b) == 1 && bob.verifiedDevices(sid, a) == 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dm, err := alice.OpenDM(ctx, []uint64{b})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "DM keyed", func() bool {
		return alice.status(dm.ID).State == client.E2EOK && bob.status(dm.ID).State == client.E2EOK
	})

	laptop, laptopKey := addKeyDevice(t, s, alice)
	fpLaptop := keyFP(t, laptopKey)
	fpDesk := keyFP(t, filepath.Join(alice.home, "id_ed25519"))
	pinned := func(u *user, of uint64, fp string) bool {
		ks, _ := u.Trust().PinnedKeys(sid, of)
		for _, k := range ks {
			if k.Fingerprint == fp {
				return true
			}
		}
		return false
	}
	pending := func(u *user, of uint64, fp string) bool {
		ks, _ := u.Trust().PendingKeyChanges(sid)
		for _, k := range ks {
			if k.User == of && k.Fingerprint == fp {
				return true
			}
		}
		return false
	}
	eventually(t, "the laptop's key is a pending change for alice and bob", func() bool {
		return pending(alice, a, fpLaptop) && pending(bob, a, fpLaptop)
	})
	if alice.verifiedDevices(sid, a) != 1 || len(alice.PendingKeys(a)) != 0 {
		t.Fatal("alice's desktop trusts the laptop without an endorsement")
	}
	if err := alice.AcceptKeyChanges(a, []string{fpLaptop}, true); !errors.Is(err, client.ErrOwnKeyAccept) {
		t.Fatal("accepted her own key on a prompt")
	}

	// What EndorseKey refuses.
	if _, err := alice.EndorseKey(ctx, "SHA256:"+strings.Repeat("A", 43)); !errors.Is(err, client.ErrKeyNotSeen) {
		t.Fatalf("unknown key: %v", err)
	}
	if _, err := alice.EndorseKey(ctx, fpDesk); err == nil {
		t.Fatal("endorsed its own key")
	}
	if _, err := alice.EndorseKey(ctx, "laptop"); err == nil {
		t.Fatal("endorsed a non-fingerprint")
	}
	if _, err := alice.EndorseKey(ctx, keyFP(t, filepath.Join(bob.home, "id_ed25519"))); !errors.Is(err, client.ErrKeyNotSeen) {
		t.Fatalf("bob's key: %v", err)
	}

	r, err := alice.EndorseKey(ctx, fpLaptop)
	if err != nil || r.By != fpDesk || r.Key != fpLaptop {
		t.Fatalf("endorse: %+v %v", r, err)
	}
	if !pinned(alice, a, fpLaptop) || alice.verifiedDevices(sid, a) != 2 {
		t.Fatal("the desktop doesn't trust the laptop right after endorsing it")
	}
	eventually(t, "bob trusts the laptop's key through the endorsement", func() bool {
		return pinned(bob, a, fpLaptop) && !slices.Contains(bob.KeyChangedMembers(dm.ID), a)
	})
	if _, err := alice.EndorseKey(ctx, fpLaptop); !errors.Is(err, client.ErrKeyTrusted) {
		t.Fatalf("second endorse: %v", err)
	}

	// The laptop doesn't trust the desktop yet: it's the same problem the
	// other way round, solved the same way.
	eventually(t, "the desktop's key is pending on the laptop", func() bool { return pending(laptop, a, fpDesk) })
	if _, err := laptop.EndorseKey(ctx, fpDesk); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the laptop holds the DM key", func() bool { return laptop.status(dm.ID).State == client.E2EOK })
	laptop.send(t, ctx, dm.ID, "from the laptop")
	eventually(t, "alice and bob read the laptop", func() bool {
		return alice.reads(dm.ID, "from the laptop")() && bob.reads(dm.ID, "from the laptop")()
	})
	bob.send(t, ctx, dm.ID, "to both")
	eventually(t, "the laptop reads bob", laptop.reads(dm.ID, "to both"))

	// A device that joins later (fresh cache, snapshot) trusts the laptop too.
	bob2 := openAs(t, bob)
	eventually(t, "a new device of bob's trusts the laptop", func() bool { return pinned(bob2, a, fpLaptop) })
}
