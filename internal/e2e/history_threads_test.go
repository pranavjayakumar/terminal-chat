package e2e

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// steppingClock advances a second per reading, so a test can send far more
// than the per-user send budget without waiting for refills.
func steppingClock() func() time.Time {
	var mu sync.Mutex
	t := time.Now().Add(-24 * time.Hour)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t = t.Add(time.Second)
		return t
	}
}

// A channel whose newest page of history is all thread replies must still
// page back to its older top-level messages. The TUI asks for history
// older than the oldest top-level message it shows (0 when it shows none),
// so a page with no top-level message in it used to come back again and
// again: the messages before a busy thread were unreachable.
func TestHistoryPagesPastLongThread(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	s, addr := startServerClock(t, t.TempDir(), "127.0.0.1:0", steppingClock())
	alice := login(t, s, addr, "alice", true)
	eventually(t, "alice online", alice.online)
	gen := alice.channel("general")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for i := 1; i <= 3; i++ {
		if err := alice.Send(gen, fmt.Sprintf("top %d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := alice.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var root uint64
	for _, m := range alice.Messages(gen, 10) {
		if m.Body == "top 3" {
			root = m.ID
		}
	}
	if root == 0 {
		t.Fatal("no root")
	}
	for i := 1; i <= 230; i++ {
		if err := alice.Send(gen, fmt.Sprintf("reply %d", i), root); err != nil {
			t.Fatal(err)
		}
	}
	if err := alice.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	// A new device (snapshot: the last 50 messages, all replies).
	bob := login(t, s, addr, "bob", false)
	eventually(t, "bob online", bob.online)
	oldestTop := func() uint64 {
		for _, m := range bob.Messages(gen, 1000) {
			if m.ID != 0 {
				return m.ID
			}
		}
		return 0
	}
	for i := 0; i < 5 && !bob.has(gen, "top 1"); i++ {
		more, err := bob.History(ctx, gen, oldestTop())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	for _, want := range []string{"top 1", "top 2", "top 3"} {
		if !bob.has(gen, want) {
			t.Fatalf("after paging history bob still lacks %q", want)
		}
	}
	if n := len(bob.Thread(root)); n < 230 {
		t.Fatalf("thread has %d messages, want the root and 230 replies", n)
	}
}
