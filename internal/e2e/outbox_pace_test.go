package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// #198: a backlog queued during an outage drains at about the server's send
// refill rate (burst 30, then 3/s, §9) once the link is back: after the
// burst the client paces itself (retry_after_ms, then sendPace) instead of
// waiting a 2 s retry after every refusal. Every message arrives exactly
// once, in order.
func TestOutboxBacklogDrainsAtServerRate(t *testing.T) {
	if testing.Short() {
		t.Skip("drains a real backlog at the real rate (~7 s)")
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "both online", func() bool { return alice.online() && bob.online() })
	gen := alice.channel("general")

	s.stop()
	eventually(t, "alice offline", func() bool { return !alice.online() })
	const n = 50
	for i := range n {
		if err := alice.Send(gen, fmt.Sprintf("backlog %03d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	startServer(t, dir, addr) // fresh buckets
	deadline := time.Now().Add(40 * time.Second)
	var got []string
	var times []int64
	for {
		got, times = got[:0], times[:0]
		for _, m := range bob.Messages(gen, 200) {
			if m.ID != 0 && strings.HasPrefix(m.Body, "backlog ") {
				got = append(got, m.Body)
				times = append(times, m.Time)
			}
		}
		if len(got) >= n || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	took := time.Since(start)
	if len(got) != n {
		t.Fatalf("bob has %d of %d messages after %v", len(got), n, took)
	}
	for i, b := range got {
		if want := fmt.Sprintf("backlog %03d", i); b != want {
			t.Fatalf("message %d = %q, want %q (order or duplicates broken)", i, b, want)
		}
	}
	// 20 messages past the burst at ≤ 3/s take ≥ ~6.7 s; the old 2 s retry
	// left 2 s holes after every few sends. Paced, no gap past the burst
	// is much over one refill interval plus a hint.
	var maxGap int64
	for i := 31; i < n; i++ {
		maxGap = max(maxGap, times[i]-times[i-1])
	}
	t.Logf("drained %d messages in %v, largest gap after the burst %d ms", n, took, maxGap)
	if maxGap > 1200 {
		t.Errorf("largest gap after the burst = %d ms, want paced sends (≤ 1.2 s)", maxGap)
	}
	if span := times[n-1] - times[0]; span < 5500 {
		t.Errorf("drain span %d ms: faster than the server's limit allows?", span)
	}
	if took > 20*time.Second {
		t.Errorf("drain took %v, want close to (n-30)/3 s", took)
	}
	eventually(t, "alice outbox drained", func() bool {
		for _, m := range alice.Messages(gen, 200) {
			if m.Pending || m.Failed != "" {
				return false
			}
		}
		return true
	})
}
