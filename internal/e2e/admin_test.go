package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
)

// B7: a private channel made the way the TUI's /create #x --private does
// (CreateEncryptedChannel) is end-to-end encrypted: the server stores no
// message text, only ciphertext, and members read it.
func TestCreatePrivateStoresCiphertext(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch, err := alice.CreateEncryptedChannel(ctx, "secret-plans", "")
	if err != nil {
		t.Fatal(err)
	}
	if !ch.E2E {
		t.Fatalf("channel %+v not e2e", ch)
	}
	if err := alice.AddMember(ctx, ch.ID, bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice keyed the channel for bob", func() bool {
		st := alice.status(ch.ID)
		return st.State == client.E2EOK && len(alice.KeyChangedMembers(ch.ID)) == 0
	})
	const text = "the launch code is 0000"
	if err := alice.Send(ch.ID, text, 0); err != nil {
		t.Fatal(err)
	}
	if err := alice.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob reads it decrypted", func() bool {
		sec, ok := bob.sec(ch.ID, text)
		return ok && sec == client.SecE2E
	})

	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "tc.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var e2e, plain, enc int
	if err := db.QueryRow(`SELECT e2e FROM channels WHERE id = ?`, ch.ID).Scan(&e2e); err != nil || e2e != 1 {
		t.Fatalf("server channel e2e = %d, %v", e2e, err)
	}
	db.QueryRow(`SELECT COUNT(*) FILTER (WHERE body != ''), COUNT(*) FILTER (WHERE enc IS NOT NULL AND length(enc) > 0)
		FROM messages WHERE chan = ?`, ch.ID).Scan(&plain, &enc)
	if plain != 0 || enc == 0 {
		t.Fatalf("server messages in the private channel: %d plaintext, %d encrypted", plain, enc)
	}
}

// Adding someone to an encrypted channel right as their first device
// registers (#127): its device.new may land after the adder verified
// devices (the adder verifies again rather than call the new member
// unverifiable), or after the adder built the bundle (the server calls
// that a race, stale_epoch, and the retry boxes the device).
func TestAddMemberWhileTheirDeviceRegisters(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	eventually(t, "online", alice.online)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := alice.CreateEncryptedChannel(ctx, "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		u := login(t, s, addr, fmt.Sprintf("u%d", i), false)
		eventually(t, "online", u.online)
		time.Sleep(time.Duration(i%4) * time.Millisecond) // vary where the add lands
		if err := alice.AddMember(ctx, ch.ID, u.Me().ID); err != nil {
			t.Fatalf("adding u%d: %v", i, err)
		}
	}
	eventually(t, "keyed", func() bool { return alice.status(ch.ID).State == client.E2EOK })
}

// B8: what /disable @bob does (DisableUser) cuts off every session of the
// account at once (a thief's laptop and the owner's desktop alike), a fresh
// login with the same key is refused, and the DM with bob gets a new key
// that doesn't include him.
func TestDisableStopsEveryDevice(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	bob2 := openAs(t, bob)
	eventually(t, "bob's second device online", bob2.online)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dm, err := alice.OpenDM(ctx, []uint64{bob.Me().ID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "dm keyed", func() bool { return alice.status(dm.ID).State == client.E2EOK })
	epoch := alice.status(dm.ID).Epoch

	if err := alice.DisableUser(ctx, bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	for _, u := range []*user{bob, bob2} {
		eventually(t, "bob's device stopped", func() bool { return u.Status().Stopped })
	}
	again := openAs(t, bob)
	eventually(t, "a new login is refused", func() bool { return again.Status().Stopped })
	eventually(t, "dm rotated without bob", func() bool {
		st := alice.status(dm.ID)
		return st.State == client.E2EOK && st.Epoch > epoch
	})
}
