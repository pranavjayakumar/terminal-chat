package e2e

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
)

// Breaker (devfix): a group channel whose current key alice's laptop got by
// key.share; the laptop is revoked with no tc client rotating; then every
// member sends at once. All of them race to rotate the stale epoch: every
// message must arrive (no permanent failure like rotateReason), and the
// laptop must read none.
func TestRevokeSharedStormSenders_BreakDevfix(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	names := []string{"alice", "bob", "carol", "dave", "erin"}
	var us []*user
	for i, n := range names {
		us = append(us, login(t, s, addr, n, i == 0))
	}
	eventually(t, "online", func() bool {
		for _, u := range us {
			if !u.online() {
				return false
			}
		}
		return true
	})
	sid := us[0].sid(t)
	eventually(t, "devices verified", func() bool {
		for _, a := range us {
			for _, b := range us {
				if a.verifiedDevices(sid, b.Me().ID) != 1 {
					return false
				}
			}
		}
		return true
	})
	alice := us[0]
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ch, err := alice.CreateEncryptedChannel(ctx, "sec", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "epoch 1", alice.e2eAt(ch.ID, 1))
	for _, m := range us[1:] {
		if err := alice.AddMember(ctx, ch.ID, m.Me().ID); err != nil {
			t.Fatal(err)
		}
	}
	last := uint32(len(us))
	for _, u := range us {
		eventually(t, "everyone at the last epoch", u.e2eAt(ch.ID, last))
	}
	alice.send(t, ctx, ch.ID, "before laptop")

	laptop := secondDevice(t, alice)
	a := alice.Me().ID
	eventually(t, "laptop verified everywhere", func() bool {
		if !laptop.online() {
			return false
		}
		for _, u := range us {
			if u.verifiedDevices(sid, a) != 2 {
				return false
			}
		}
		return true
	})
	eventually(t, "laptop reads via key.share", laptop.reads(ch.ID, "before laptop"))
	lost, err := laptop.Trust().DeviceKeys(sid, a)
	if err != nil {
		t.Fatal(err)
	}
	rawRevoke(t, addr, s.Pin(), alice.Config().KeyPath, sid, a, lost.ID)
	for _, u := range us {
		eventually(t, "revoke seen", func() bool {
			d, ok, _ := u.Trust().Device(sid, lost.ID)
			return ok && d.Revoked
		})
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(us))
	for i, u := range us {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := u.Send(ch.ID, fmt.Sprintf("storm %d", i), 0); err != nil {
				errs <- err
				return
			}
			if err := u.Flush(ctx); err != nil {
				errs <- fmt.Errorf("%s: %w", names[i], err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for i, u := range us {
		if f := u.brFailed(ch.ID); f != "" {
			t.Errorf("%s: a message failed: %q", names[i], f)
		}
	}
	for i := range us {
		body := fmt.Sprintf("storm %d", i)
		for _, u := range us {
			eventually(t, "everyone reads "+body, u.reads(ch.ID, body))
		}
		if _, ok := laptop.sec(ch.ID, body); ok {
			t.Errorf("revoked laptop decrypted %q", body)
		}
	}
}

// Breaker (devfix): bob writes while offline (sealed offline under the
// current epoch, which the laptop holds by key.share); meanwhile the laptop
// is revoked and nobody rotates. When bob comes back, the laptop must not
// read his message.
func TestRevokeOfflineSealedOutbox_BreakDevfix(t *testing.T) {
	s, addr, alice, laptop, bob, dm, ctx := shareSetup(t)
	sid := alice.sid(t)
	a := alice.Me().ID
	lost, err := laptop.Trust().DeviceKeys(sid, a)
	if err != nil {
		t.Fatal(err)
	}
	bob.cancel()
	bob.Close()
	off, err := client.Open(bob.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := off.Send(dm, "bob offline", 0); err != nil {
		t.Fatal(err)
	}
	rawRevoke(t, addr, s.Pin(), alice.Config().KeyPath, sid, a, lost.ID)
	rctx, rcancel := context.WithCancel(context.Background())
	go off.Run(rctx)
	t.Cleanup(func() { rcancel(); off.Close() })
	if err := off.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice reads bob", alice.reads(dm, "bob offline"))
	time.Sleep(500 * time.Millisecond)
	if _, ok := laptop.sec(dm, "bob offline"); ok {
		t.Fatal("revoked laptop decrypted a message sealed offline before the revoke was known")
	}
}

// Breaker (devfix): two long-lived processes on one TC_HOME (TUI + tc tail,
// or two TUIs) when one runs revoke --self. Both reconnect at once (one by
// restart, the other via selfRevoked) and both register the same new keys
// with different certs: the loser gets "device id already registered",
// which tryDevice records as devErr, so its encrypted sends fail at once
// and for good (failErr) though the device is registered.
func TestRevokeSelfTwoLiveProcesses_BreakDevfix(t *testing.T) {
	_, _, alice, _, bob, dm, ctx := revokeSetup(t)
	a := alice.Me().ID
	sid := alice.sid(t)
	for i := range 10 {
		other := reopen(t, alice.home)
		eventually(t, "other online", func() bool { return other.online() && other.DeviceError() == nil })
		cur, err := alice.Trust().DeviceKeys(sid, a)
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "registered", func() bool {
			d, ok, _ := alice.Trust().Device(sid, cur.ID)
			return alice.online() && ok && !d.Revoked && alice.DeviceError() == nil
		})
		time.Sleep(2100 * time.Millisecond) // rotation cap
		if _, err := alice.RevokeDevice(ctx, cur.ID, true); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		next, _ := alice.Trust().DeviceKeys(sid, a)
		eventually(t, "new keys registered", func() bool {
			d, ok, _ := alice.Trust().Device(sid, next.ID)
			return ok && !d.Revoked
		})
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && !(alice.online() && other.online()) {
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(1500 * time.Millisecond) // both sessions' first registration attempt
		for name, u := range map[string]*user{"revoker": alice, "sibling": other} {
			body := fmt.Sprintf("%s after self revoke %d", name, i)
			if err := u.Send(dm, body, 0); err != nil {
				t.Fatal(err)
			}
			fctx, fcancel := context.WithTimeout(ctx, 20*time.Second)
			err := u.Flush(fctx)
			fcancel()
			if f := u.brFailed(dm); f != "" {
				t.Fatalf("cycle %d: %s process's encrypted send failed for good: %q (its keys %x are registered on the server)",
					i, name, f, next.ID[:4])
			}
			if err != nil {
				t.Fatalf("cycle %d: %s flush: %v (online %v, device error %v, e2e %+v)",
					i, name, err, u.online(), u.DeviceError(), u.status(dm))
			}
			eventually(t, "bob reads "+body, bob.reads(dm, body))
		}
		other.cancel()
		other.Close()
	}
}
