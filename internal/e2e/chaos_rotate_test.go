package e2e

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/trust"
)

// Chaos during rotations (docs/E2E.md step 12): four clients in an
// encrypted private channel send concurrently while the creator adds a
// member, removes another, and members rotate keys, all through a proxy that
// kills connections and adds latency. Afterwards every member holds the
// same decrypted messages (none lost, none twice, nothing waiting, no
// alarm) and the same verified chain head, and the removed member can't
// open anything the server stored after its removal.
func TestChaosRotations(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test is slow")
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	dir := t.TempDir()
	// The server's clock jumps ahead before each explicit rotation, past
	// its rate cap for rotations that change nothing (2 s per channel,
	// 10 s per user): at most a couple of minutes in all, within the
	// clients' 5 min signed-time drift.
	var skew atomic.Int64
	s, addr := startServerClock(t, dir, "127.0.0.1:0", func() time.Time { return time.Now().Add(time.Duration(skew.Load())) })
	proxy := newChaosProxy(t, addr)
	proxy.calm() // set up first, then chaos
	// Under constant kills a slow runner's reconnects never got through
	// sync, so clients stayed offline for minutes and nothing happened
	// (alice "offline" for 180 s). Each connection gets a second first.
	proxy.minAge = time.Second

	names := []string{"alice", "bob", "carol", "dave"}
	var us []*user
	for i, n := range names {
		us = append(us, login(t, s, proxy.addr(), n, i == 0))
	}
	alice, bob, carol, dave := us[0], us[1], us[2], us[3]
	eventually(t, "online", func() bool {
		for _, u := range us {
			if !u.online() {
				return false
			}
		}
		return true
	})
	sid := alice.sid(t)
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
	setup, cancelSetup := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelSetup()
	ch, err := alice.CreateEncryptedChannel(setup, "sec", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "epoch 1", alice.e2eAt(ch.ID, 1))
	for _, u := range []*user{bob, dave} {
		if err := alice.AddMember(setup, ch.ID, u.Me().ID); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "everyone at epoch 3", func() bool {
		return alice.e2eAt(ch.ID, 3)() && bob.e2eAt(ch.ID, 3)() && dave.e2eAt(ch.ID, 3)()
	})

	// Chaos on: kill a connection every 150–500 ms, latency on every write.
	proxy.mu.Lock()
	proxy.chaos = true
	proxy.mu.Unlock()
	chaosAt := time.Now()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		for ctx.Err() == nil {
			time.Sleep(time.Duration(150+rand.IntN(350)) * time.Millisecond)
			proxy.killRandom()
		}
	}()

	// retry runs op until it succeeds or done says it took effect anyway
	// (a lost RES); transient failures (offline, stale, rate limited) retry.
	retry := func(what string, op func(context.Context) error, done func() bool) {
		// Outlive the chaos phase: under constant kills a client's reconnect
		// backoff reaches 30 s, and slow CI runners stretch chaos past 60 s.
		deadline := time.Now().Add(180 * time.Second)
		for {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := op(cctx)
			cancel()
			if err == nil || done() {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("%s: %v", what, err)
				return
			}
			time.Sleep(time.Duration(50+rand.IntN(200)) * time.Millisecond)
		}
	}
	member := func(u *user) func() bool {
		return func() bool { v, ok := alice.Channel(ch.ID); return ok && slices.Contains(v.Members, u.Me().ID) }
	}

	const perUser = 15
	var removedAt uint32 // the epoch alice's removal of dave created
	var wg sync.WaitGroup
	send := func(u *user, start time.Duration) {
		defer wg.Done()
		time.Sleep(start)
		for i := range perUser {
			if err := u.Send(ch.ID, fmt.Sprintf("%s-%02d", u.Me().Name, i), 0); err != nil {
				t.Errorf("%s send: %v", u.Me().Name, err)
			}
			time.Sleep(time.Duration(300+rand.IntN(600)) * time.Millisecond)
		}
	}
	wg.Add(3)
	go send(alice, 0)
	go send(bob, 0)
	// dave sends only while he's a member: his sends are done (flushed)
	// before alice removes him, so none of them can be refused.
	go func() {
		defer wg.Done()
		for i := range 5 {
			if err := dave.Send(ch.ID, fmt.Sprintf("dave-%02d", i), 0); err != nil {
				t.Errorf("dave send: %v", err)
			}
			time.Sleep(time.Duration(50+rand.IntN(150)) * time.Millisecond)
		}
		fctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := dave.Flush(fctx); err != nil {
			t.Errorf("dave flush: %v", err)
		}

		time.Sleep(300 * time.Millisecond)
		retry("add carol", func(c context.Context) error { return alice.AddMember(c, ch.ID, carol.Me().ID) }, member(carol))
		// carol sends once her client has the channel, as a user would: until
		// then her send gate can't check the members' keys and fails closed
		// ("channel not loaded"), and under chaos that can take a while.
		for w := time.Now().Add(180 * time.Second); time.Now().Before(w); time.Sleep(20 * time.Millisecond) {
			if v, ok := carol.Channel(ch.ID); ok && v.Member {
				break
			}
		}
		wg.Add(1)
		go send(carol, 0)
		time.Sleep(3 * time.Second)
		retry("remove dave", func(c context.Context) error { return alice.RemoveMember(c, ch.ID, dave.Me().ID) },
			func() bool { return !member(dave)() })
		head, _, _ := alice.Trust().ChainHead(sid, ch.ID)
		removedAt = head.Epoch
	}()
	// Rotations by every current member in turn, racing everything above,
	// until six went through, or 30 s passed with at least three (chaos
	// lasts until then too). Bounded: under constant kills a client's
	// reconnect backoff grows to 30 s, so a long chaos phase mostly measures
	// backoff; slow CI runners still get up to 120 s to reach the minimum.
	// At most maxAttempts tries: each moves the server's clock 11 s ahead,
	// and the total must stay within the clients' 5 min drift.
	const wantRotations, minRotations, maxAttempts = 6, 3, 20
	rotations := 0
	rotDone := make(chan struct{})
	go func() {
		defer close(rotDone)
		soft, hard := time.Now().Add(30*time.Second), time.Now().Add(120*time.Second)
		for i, attempts := 0, 0; attempts < maxAttempts && rotations < wantRotations && time.Now().Before(hard) &&
			(rotations < minRotations || time.Now().Before(soft)); i++ {
			time.Sleep(time.Duration(400+rand.IntN(800)) * time.Millisecond)
			// The next member in turn that has a session: once its backoff
			// reaches the client's 30 s cap a client is offline most of the
			// time (sessions never last the 10 s that reset it), so waiting
			// for one particular member found slow runners making only a
			// couple of attempts in 120 s (PR #45/#48/#51).
			var u *user
			for u == nil && time.Now().Before(hard) {
				for j := range us {
					c := us[(i+j)%len(us)]
					if v, ok := c.Channel(ch.ID); ok && v.Member && c.online() {
						u, i = c, i+j
						break
					}
				}
				if u == nil {
					time.Sleep(20 * time.Millisecond)
				}
			}
			if u == nil {
				break
			}
			attempts++
			skew.Add(int64(11 * time.Second))
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := u.RotateKey(cctx, ch.ID)
			cancel()
			if err == nil {
				rotations++
			} else {
				t.Logf("rotate by %s: %v", u.Me().Name, err) // killed mid-rotation, stale, rate limited: chaos
			}
		}
	}()
	wg.Wait()
	<-rotDone
	time.Sleep(2 * time.Second)
	stop()
	proxy.calm()
	t.Logf("chaos lasted %v", time.Since(chaosAt).Round(100*time.Millisecond))
	t.Logf("proxy killed %d connections; %d explicit rotations; dave removed at epoch %d",
		proxy.kills, rotations, removedAt)
	if t.Failed() {
		t.FailNow()
	}
	if proxy.kills < 5 {
		t.Fatalf("chaos too gentle: only %d kills", proxy.kills)
	}
	if rotations < minRotations {
		t.Fatalf("only %d explicit rotations went through", rotations)
	}

	members := []*user{alice, bob, carol}
	view := func(u *user) (got []string, waiting, pending int) {
		for _, m := range u.Messages(ch.ID, 1000) {
			switch {
			case m.ID == 0:
				pending++
			case m.Sec == client.SecWaiting:
				waiting++
			case m.Sec == client.SecE2E:
				got = append(got, fmt.Sprintf("%d:%s", m.ID, m.Body))
			default:
				got = append(got, fmt.Sprintf("%d:?%v", m.ID, m.Sec))
			}
		}
		return got, waiting, pending
	}
	want := 3*perUser + 5
	calmAt := time.Now()
	deadline := time.Now().Add(90 * time.Second)
	for {
		ok := true
		hctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		for _, u := range members {
			for more, before := true, uint64(0); more; {
				var err error
				if more, err = u.History(hctx, ch.ID, before); err != nil {
					break
				}
				ms := u.Messages(ch.ID, 1000)
				if len(ms) == 0 || ms[0].ID == before {
					break
				}
				before = ms[0].ID
			}
			got, waiting, pending := view(u)
			if len(got) != want || waiting > 0 || pending > 0 || u.status(ch.ID).State != client.E2EOK {
				ok = false
			}
		}
		cancel()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			for _, u := range members {
				got, waiting, pending := view(u)
				t.Logf("%s: %d shown, %d waiting, %d pending, status %+v", u.Me().Name, len(got), waiting, pending, u.status(ch.ID))
				for _, m := range u.Messages(ch.ID, 1000) {
					if m.ID == 0 {
						t.Logf("  %s unsent %q: pending=%v failed=%q", u.Me().Name, m.Body, m.Pending, m.Failed)
					}
				}
			}
			t.Fatal("members did not converge")
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Logf("converged %v after the chaos", time.Since(calmAt).Round(100*time.Millisecond))
	ref, _, _ := view(alice)
	seen := map[string]int{}
	for _, b := range ref {
		var id uint64
		var text string
		fmt.Sscanf(b, "%d:%s", &id, &text)
		seen[text]++
	}
	for _, n := range []string{"alice", "bob", "carol"} {
		for i := range perUser {
			if c := seen[fmt.Sprintf("%s-%02d", n, i)]; c != 1 {
				t.Errorf("%s-%02d shown %d times", n, i, c)
			}
		}
	}
	for i := range 5 {
		if c := seen[fmt.Sprintf("dave-%02d", i)]; c != 1 {
			t.Errorf("dave-%02d shown %d times", i, c)
		}
	}
	for _, u := range members[1:] {
		if got, _, _ := view(u); !slices.Equal(got, ref) {
			t.Errorf("%s's messages differ from alice's", u.Me().Name)
		}
	}
	h0, _, _ := alice.Trust().ChainHead(sid, ch.ID)
	t.Logf("%d messages, head epoch %d", len(ref), h0.Epoch)
	for _, u := range members {
		if _, ok := u.Alarm(ch.ID); ok {
			t.Errorf("%s has an alarm", u.Me().Name)
		}
		eventually(t, u.Me().Name+" at the same head", func() bool {
			h, _, _ := u.Trust().ChainHead(sid, ch.ID)
			return h == h0
		})
	}

	// dave can't open anything stored after his removal.
	// The removal epoch is the first one after he joined whose roster lacks him. Reading
	// alice's head right after the remove can still see the epoch before it:
	// a retried RemoveMember may have landed after its timeout, and the new
	// bundle reaches alice's trust store after the member change.
	if removedAt == 0 {
		t.Fatal("removal never observed")
	}
	removedAt = 0
	var ep trust.Epoch
	seenDave := false
	for e := uint32(1); e <= h0.Epoch; e++ {
		got, ok, _ := alice.Trust().Epoch(sid, ch.ID, e)
		if !ok {
			continue
		}
		if slices.Contains(got.Roster, dave.Me().ID) {
			seenDave = true
		} else if seenDave {
			removedAt, ep = e, got
			break
		}
	}
	if removedAt == 0 || ep.Seq == 0 {
		t.Fatalf("no epoch without dave up to head %d", h0.Epoch)
	}
	after := 0
	for id, enc := range rawEncs(t, dir, ch.ID) {
		if id > ep.Seq {
			after++
			if dave.canDecrypt(t, sid, ch.ID, enc) {
				t.Errorf("dave can decrypt message %d, stored after his removal", id)
			}
		}
	}
	if after == 0 {
		t.Fatal("nothing was sent after dave's removal")
	}
	for e := removedAt; e <= h0.Epoch; e++ {
		if dave.epochKey(sid, ch.ID, e) != nil {
			t.Errorf("dave holds the key of epoch %d", e)
		}
	}
}
