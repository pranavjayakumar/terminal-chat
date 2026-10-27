package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/skailama/tc/internal/background"
)

// fakeNotifier holds home's run lock (so Running says yes) and reports
// state, like a connected tc background.
func fakeNotifier(t *testing.T, home, state string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(home, background.RunLockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	setNotifierState(t, home, state)
}

func setNotifierState(t *testing.T, home, state string) {
	t.Helper()
	b, _ := json.Marshal(background.Status{PID: os.Getpid(), State: state})
	tmp := filepath.Join(home, ".bg.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(home, background.StatusFile)); err != nil {
		t.Fatal(err)
	}
}

func startedWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// The chat window's client must not run on the cache while tc background
// is still connected on it: it starts once the notifier has paused.
func TestHoldUIWaitsForNotifierToLetGo(t *testing.T) {
	home := t.TempDir()
	fakeNotifier(t, home, "online")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	release, err := holdUI(ctx, home, func(context.Context) { close(started) })
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !background.UIOpen(home) {
		t.Fatal("UI lock not held")
	}
	if startedWithin(started, 500*time.Millisecond) {
		t.Fatal("the window's client started while tc background was still online on the same cache")
	}
	setNotifierState(t, home, "paused")
	if !startedWithin(started, 2*time.Second) {
		t.Fatal("the window's client never started after the notifier paused")
	}
}

// No notifier, or one with no login (no connection): start at once.
func TestHoldUIStartsAtOnceWhenNotifierQuiet(t *testing.T) {
	for _, state := range []string{"", "waiting"} {
		home := t.TempDir()
		if state != "" {
			fakeNotifier(t, home, state)
		}
		started := make(chan struct{})
		release, err := holdUI(context.Background(), home, func(context.Context) { close(started) })
		if err != nil {
			t.Fatal(err)
		}
		if !startedWithin(started, 500*time.Millisecond) {
			t.Errorf("notifier %q: the window's client didn't start at once", state)
		}
		release()
	}
}

// Quitting before the notifier lets go doesn't leave the start pending.
func TestHoldUICancelledWhileWaiting(t *testing.T) {
	home := t.TempDir()
	fakeNotifier(t, home, "online")
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	release, err := holdUI(ctx, home, func(ctx context.Context) { got <- ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cancel()
	select {
	case err := <-got:
		if err == nil {
			t.Fatal("run got a live context after the window quit")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter goroutine still pending after cancel")
	}
}
