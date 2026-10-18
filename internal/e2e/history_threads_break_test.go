package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// History gives up after maxReplyPages pages and reports more=true, but
// the TUI's cursor (the oldest top-level message it shows) has not moved,
// so every later call fetches the same 50 pages again: whatever is older
// than a thread longer than 50 pages stays unreachable. 50 pages is 5000
// short replies, or ~550 near-64 KiB ones (trimBytes cuts a page to 768
// KiB, ~11 such messages).
func TestHistoryPagesPastLongThread_BreakPageCap(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	s, addr := startServerClock(t, t.TempDir(), "127.0.0.1:0", steppingClock())
	alice := login(t, s, addr, "alice", true)
	eventually(t, "alice online", alice.online)
	gen := alice.channel("general")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for _, b := range []string{"top 1", "root"} {
		if err := alice.Send(gen, b, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := alice.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var root uint64
	for _, m := range alice.Messages(gen, 10) {
		if m.Body == "root" {
			root = m.ID
		}
	}
	big := strings.Repeat("x", 60<<10)
	for i := 1; i <= 900; i++ {
		if err := alice.Send(gen, fmt.Sprintf("%d %s", i, big), root); err != nil {
			t.Fatal(err)
		}
		if i%50 == 0 {
			if err := alice.Flush(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := alice.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	bob := login(t, s, addr, "bob", false)
	eventually(t, "bob online", bob.online)
	oldestTop := func() uint64 { // what the TUI pages by
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
	if !bob.has(gen, "top 1") {
		t.Fatalf("after 5 History calls bob still lacks the message before a 900-reply thread (cursor stuck at %d)", oldestTop())
	}
}
func TestHistoryPagesPastLongThread_BreakPageCapShort(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	s, addr := startServerClock(t, t.TempDir(), "127.0.0.1:0", steppingClock())
	alice := login(t, s, addr, "alice", true)
	eventually(t, "alice online", alice.online)
	gen := alice.channel("general")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	for _, b := range []string{"top 1", "root"} {
		if err := alice.Send(gen, b, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := alice.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var root uint64
	for _, m := range alice.Messages(gen, 10) {
		if m.Body == "root" {
			root = m.ID
		}
	}
	big := "ok"
	for i := 1; i <= 5100; i++ {
		if err := alice.Send(gen, fmt.Sprintf("%d %s", i, big), root); err != nil {
			t.Fatal(err)
		}
		if i%500 == 0 {
			if err := alice.Flush(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := alice.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	bob := login(t, s, addr, "bob", false)
	eventually(t, "bob online", bob.online)
	oldestTop := func() uint64 { // what the TUI pages by
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
	if !bob.has(gen, "top 1") {
		t.Fatalf("after 5 History calls bob still lacks the message before a 5100-reply thread (cursor stuck at %d)", oldestTop())
	}
}
