package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/skailama/tc/internal/background"
	"github.com/skailama/tc/internal/notify"
)

type dropSender struct{}

func (dropSender) Send(notify.Note) bool { return true }

// runBackground starts a notifier for home in-process and waits for state.
func runBackground(t *testing.T, home, state string) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- background.Run(ctx, background.Config{Home: home, Version: "test", Out: dropSender{},
			Poll: 100 * time.Millisecond, UIPoll: 50 * time.Millisecond})
	}()
	t.Cleanup(func() { stop(); <-done })
	if !waitFor(5*time.Second, func() bool {
		st, err := background.ReadStatus(home)
		return err == nil && st.State == state
	}) {
		t.Fatalf("background never reached %q", state)
	}
}

// A notifier with no login holds no connection: tc join must not sit out
// the whole pause timeout waiting for it to say "paused".
func TestPauseBackgroundWaitingIsImmediate(t *testing.T) {
	home := t.TempDir()
	runBackground(t, home, "waiting")
	start := time.Now()
	release := pauseBackground(home)
	defer release()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("pauseBackground took %v with the notifier waiting for a login", d)
	}
	if !background.UIOpen(home) {
		t.Fatal("UI lock not held during the login")
	}
}

// Once the login lands, the waiting notifier pauses (doesn't connect)
// while the join still holds the UI lock.
func TestPauseBackgroundWaitingThenLoginPauses(t *testing.T) {
	home := t.TempDir()
	runBackground(t, home, "waiting")
	release := pauseBackground(home)
	defer release()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool {
		st, err := background.ReadStatus(home)
		return err == nil && st.State == "paused"
	}) {
		st, _ := background.ReadStatus(home)
		t.Fatalf("notifier state %q after a login with the UI held, want paused", st.State)
	}
}
