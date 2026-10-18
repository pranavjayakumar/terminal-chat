package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// longThread posts "top 1", "root" and n replies of body to root.
func longThread(t *testing.T, ctx context.Context, alice *user, gen uint64, n int, body string) (root uint64) {
	t.Helper()
	for _, b := range []string{"top 1", "root"} {
		if err := alice.Send(gen, b, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := alice.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for _, m := range alice.Messages(gen, 10) {
		if m.Body == "root" {
			root = m.ID
		}
	}
	for i := 1; i <= n; i++ {
		if err := alice.Send(gen, fmt.Sprintf("%d %s", i, body), root); err != nil {
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
	return root
}

func (u *user) oldestTop(ch uint64) uint64 { // what the TUI pages by
	for _, m := range u.Messages(ch, 1000) {
		if m.ID != 0 {
			return m.ID
		}
	}
	return 0
}

func (u *user) oldestReply(root uint64) uint64 {
	var o uint64
	for _, m := range u.Thread(root) {
		if m.ID != 0 && m.ID != root && (o == 0 || m.ID < o) {
			o = m.ID
		}
	}
	return o
}

// A resume point recorded for before=0 survives a leave. After a rejoin the
// cache holds nothing of the channel, but opening the long thread and
// scrolling it back caches the stop point again (ThreadHistory), so the
// next History(ch, 0) resumes there: everything newer than the stop point
// (here a top-level message posted while bob was away) is never fetched,
// and bob's cursor then moves below it for good. The "still cached" check
// proves nothing about the range between before and the stop point when
// before is 0, since that range keeps growing.
func TestHistoryResume_BreakRejoinSkipsNewer(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	s, addr := startServerClock(t, t.TempDir(), "127.0.0.1:0", steppingClock())
	alice := login(t, s, addr, "alice", true)
	eventually(t, "alice online", alice.online)
	gen := alice.channel("general")
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	root := longThread(t, ctx, alice, gen, 900, strings.Repeat("x", 60<<10))

	bob := login(t, s, addr, "bob", false)
	eventually(t, "bob online", bob.online)
	if b := bob.oldestTop(gen); b != 0 {
		t.Fatalf("setup: bob already shows a top-level message %d", b)
	}
	more, err := bob.History(ctx, gen, 0) // hits the page cap: resume {0, S}
	if err != nil || !more {
		t.Fatalf("first History: more=%v err=%v", more, err)
	}
	if bob.has(gen, "top 1") {
		t.Fatal("setup: the first call already reached top 1 (cap not hit)")
	}
	stop := bob.oldestReply(root)

	if err := bob.Leave(ctx, gen); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob's cache of general wiped", func() bool { return len(bob.Thread(root)) == 0 })

	if err := alice.Send(gen, "fresh", 0); err != nil {
		t.Fatal(err)
	}
	for i := range 60 {
		if err := alice.Send(gen, fmt.Sprintf("late %d", i), root); err != nil {
			t.Fatal(err)
		}
	}
	if err := alice.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	if err := bob.Join(ctx, gen); err != nil {
		t.Fatal(err)
	}
	// Bob opens the thread and scrolls it back past the old stop point.
	for cur := uint64(0); ; {
		more, err := bob.ThreadHistory(ctx, gen, root, cur)
		if err != nil {
			t.Fatal(err)
		}
		cur = bob.oldestReply(root)
		if !more || cur <= stop {
			break
		}
	}
	// Then scrolls the channel like the TUI does.
	for range 10 {
		more, err := bob.History(ctx, gen, bob.oldestTop(gen))
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	if !bob.has(gen, "top 1") {
		t.Fatal("bob never reached top 1")
	}
	if !bob.has(gen, "fresh") {
		t.Fatalf("bob paged to the start of general but never got %q: History resumed from a stale stop point (%d) recorded before the leave", "fresh", stop)
	}
}

// slowProxy forwards TCP to upstream, capping server-to-client bandwidth.
func slowProxy(t *testing.T, upstream string, bytesPerSec int) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", upstream)
			if err != nil {
				c.Close()
				continue
			}
			t.Cleanup(func() { c.Close(); up.Close() })
			go func() { io.Copy(up, c); up.Close(); c.Close() }()
			go func() {
				buf := make([]byte, 32<<10)
				for {
					n, err := up.Read(buf)
					if n > 0 {
						time.Sleep(time.Duration(n) * time.Second / time.Duration(bytesPerSec))
						if _, werr := c.Write(buf[:n]); werr != nil {
							break
						}
					}
					if err != nil {
						break
					}
				}
				up.Close()
				c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// Every History call that ends in an error (the TUI's request timeout on a
// slow link, a dropped connection) throws away the pages it walked: the
// resume point is recorded only when the call reaches the page cap. On a
// link that can't move 50 pages (up to ~38 MB) within the TUI's 30 s
// timeout, every call times out and the next restarts at the caller's
// unchanged cursor, so the messages before a long thread stay unreachable
// (#81 again). Here: 8 MB/s and a 1 s timeout per call, the same budget as
// 30 s at ~2 Mbit/s.
func TestHistoryResume_BreakTimeoutLosesProgress(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServerClock(t, t.TempDir(), "127.0.0.1:0", steppingClock())
	alice := login(t, s, addr, "alice", true)
	eventually(t, "alice online", alice.online)
	gen := alice.channel("general")
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	longThread(t, ctx, alice, gen, 900, strings.Repeat("x", 60<<10))

	bob := login(t, s, slowProxy(t, addr, 8<<20), "bob", false)
	eventually(t, "bob online", bob.online)
	var errs int
	for range 20 {
		if bob.has(gen, "top 1") {
			break
		}
		eventually(t, "bob online", bob.online)
		cctx, ccancel := context.WithTimeout(ctx, time.Second)
		more, err := bob.History(cctx, gen, bob.oldestTop(gen))
		ccancel()
		if err != nil {
			errs++
			continue
		}
		if !more {
			break
		}
	}
	if !bob.has(gen, "top 1") {
		t.Fatalf("after 20 History calls (%d timed out) bob still lacks the message before an 82-page thread: each timed-out call's progress was dropped", errs)
	}
}
