package background

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSuperviseRestartsAndStops(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	// Exits at once twice, then keeps running until stopped.
	script := `echo x >> "` + runs + `"; n=$(wc -l < "` + runs + `"); [ "$n" -ge 3 ] && exec sleep 60; exit 1`
	var want atomic.Bool
	want.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Supervise(ctx, Child{Name: "t", Path: "/bin/sh", Args: []string{"-c", script}, Want: want.Load,
			Poll: 20 * time.Millisecond, MinBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond, Grace: time.Second})
		close(done)
	}()
	count := func() int {
		b, _ := os.ReadFile(runs)
		return strings.Count(string(b), "x")
	}
	waitUntil(t, func() bool { return count() == 3 })
	// Not wanted any more: stopped and not restarted.
	want.Store(false)
	time.Sleep(150 * time.Millisecond)
	if count() != 3 {
		t.Fatalf("ran %d times", count())
	}
	want.Store(true)
	waitUntil(t, func() bool { return count() == 4 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise didn't return")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(end) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
