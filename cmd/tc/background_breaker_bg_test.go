package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/skailama/tc/internal/background"
)

// `tc background off` deletes background.lock even when it just told the
// user a notifier is still running. The running notifier keeps its flock on
// the unlinked inode, so `tc background status` says "running: no", and the
// next `tc background on` (or a second hand-started run) creates a fresh
// background.lock and locks it too: two notifiers, every popup twice.
func TestBackgroundOff_BreakDeletesLockOfLiveNotifier(t *testing.T) {
	m, _ := testManager(t, "darwin")
	home := m.S.Home
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// A notifier started by hand (`tc background run`) holds the run lock.
	f, err := os.OpenFile(filepath.Join(home, background.RunLockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := backgroundOff(context.Background(), &out, m, home); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "still running") {
		t.Fatalf("off didn't notice the running notifier:\n%s", out.String())
	}
	if !background.Running(home) {
		t.Error("status says not running while the notifier still runs")
	}
	// A second notifier starting now must not get the lock.
	g, err := os.OpenFile(filepath.Join(home, background.RunLockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := syscall.Flock(int(g.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Error("a second notifier took the run lock while the first still holds it: double notifications")
	}
}
