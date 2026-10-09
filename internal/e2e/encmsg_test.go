package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
)

// sec is the verdict u has on message body in ch (and whether it has it).
func (u *user) sec(ch uint64, body string) (client.Sec, bool) {
	for _, m := range u.Messages(ch, 200) {
		if m.Body == body && m.ID != 0 {
			return m.Sec, true
		}
	}
	return 0, false
}

func (u *user) count(ch uint64, body string) int {
	n := 0
	for _, m := range u.Messages(ch, 200) {
		if m.Body == body {
			n++
		}
	}
	return n
}

// Two real clients over the real server: bob sends first into the DM alice
// just opened (whoever gets there first makes epoch 1), alice answers in a
// thread, and both see everything decrypted and verified.
func TestEncryptedDMRoundTrip(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	sid := alice.sid(t)
	a, b := alice.Me().ID, bob.Me().ID
	eventually(t, "devices verified both ways", func() bool {
		return alice.verifiedDevices(sid, b) == 1 && bob.verifiedDevices(sid, a) == 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dm, err := alice.OpenDM(ctx, []uint64{b})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob has the DM", func() bool { _, ok := bob.Channel(dm.ID); return ok })
	if err := bob.Send(dm.ID, "hi alice", 0); err != nil {
		t.Fatal(err)
	}
	if err := bob.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice reads bob", func() bool { sec, ok := alice.sec(dm.ID, "hi alice"); return ok && sec == client.SecE2E })
	if sec, _ := bob.sec(dm.ID, "hi alice"); sec != client.SecE2E {
		t.Fatalf("bob's own copy: %s", sec)
	}

	var root uint64
	for _, m := range alice.Messages(dm.ID, 10) {
		root = m.ID
	}
	if err := alice.Send(dm.ID, "in a thread", root); err != nil {
		t.Fatal(err)
	}
	if err := alice.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob reads the reply", func() bool {
		for _, m := range bob.Thread(root) {
			if m.Body == "in a thread" && m.Sec == client.SecE2E && m.Thread == root {
				return true
			}
		}
		return false
	})
	// Server search can't see any of it; the local cache can.
	res, err := alice.Search(ctx, "thread", 0)
	if err != nil || len(res) != 1 || res[0].Body != "in a thread" {
		t.Fatalf("search: %+v %v", res, err)
	}
}

// alice queues a message while offline (encrypted at once for epoch 1);
// bob rotates meanwhile; when alice reconnects the send gets stale_epoch,
// is re-encrypted for epoch 2 with the same nonces and lands exactly once.
func TestEncryptedOfflineSendThenRotation(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	sid := alice.sid(t)
	a, b := alice.Me().ID, bob.Me().ID
	eventually(t, "devices verified both ways", func() bool {
		return alice.verifiedDevices(sid, b) == 1 && bob.verifiedDevices(sid, a) == 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dm, err := alice.OpenDM(ctx, []uint64{b})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "epoch 1 on both sides", func() bool {
		return alice.status(dm.ID).State == client.E2EOK && bob.status(dm.ID).State == client.E2EOK
	})

	// alice goes offline and writes.
	alice.cancel()
	alice.Close()
	off, err := client.Open(alice.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := off.Send(dm.ID, "written offline", 0); err != nil {
		t.Fatal(err)
	}

	// Nothing changed since epoch 1, so the rotate rate cap applies.
	time.Sleep(2100 * time.Millisecond)
	if err := bob.RotateKey(ctx, dm.ID); err != nil {
		t.Fatal(err)
	}
	if st := bob.status(dm.ID); st.Epoch != 2 {
		t.Fatalf("bob at %+v", st)
	}

	rctx, rcancel := context.WithCancel(context.Background())
	go off.Run(rctx)
	t.Cleanup(func() { rcancel(); off.Close() })
	if err := off.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob reads it", func() bool { sec, ok := bob.sec(dm.ID, "written offline"); return ok && sec == client.SecE2E })
	if n := bob.count(dm.ID, "written offline"); n != 1 {
		t.Fatalf("bob has %d copies", n)
	}
	eventually(t, "alice sees it once", func() bool { return (&user{Client: off}).count(dm.ID, "written offline") == 1 })
	if sec, _ := (&user{Client: off}).sec(dm.ID, "written offline"); sec != client.SecE2E {
		t.Fatalf("alice's copy: %s", sec)
	}
	if st := off.E2EStatus(dm.ID); st.Epoch != 2 {
		t.Fatalf("alice at %+v", st)
	}
}
