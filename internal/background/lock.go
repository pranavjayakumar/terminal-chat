// Package background is `tc background`: a process with no UI that stays
// connected and shows desktop notifications while the chat window is closed
// (docs/TUI.md, "Notifications when tc is closed"). It runs the same policy
// as the chat UI (internal/alert) and is silent while a chat window for the
// same TC_HOME is open. It never marks anything read.
//
// It is started by the OS service manager: a LaunchAgent on macOS, a systemd
// user unit on Linux (service.go).
package background

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Lock files in TC_HOME.
const (
	// UILockFile is held (shared) by every open chat window; the
	// background notifier stays silent while anyone holds it. An OS lock
	// goes away with its process, crash or not.
	UILockFile = "ui.lock"
	// RunLockFile is held (exclusive) by the running background notifier:
	// one per TC_HOME, and how `tc background status` tells it's running.
	RunLockFile = "background.lock"
)

func openLock(home, name string) (*os.File, error) {
	// O_NOFOLLOW: never lock (or create) through a planted symlink.
	return os.OpenFile(filepath.Join(home, name), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
}

// HoldUI marks a chat window open for home until release is called or the
// process exits. Several windows may hold it at once.
func HoldUI(home string) (release func(), err error) {
	f, err := openLock(home, UILockFile)
	if err != nil {
		return func() {}, fmt.Errorf("opening %s: %w", UILockFile, err)
	}
	// The notifier's probe holds the lock exclusively for an instant:
	// retry rather than block forever on a wedged one.
	for i := 0; ; i++ {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) || i >= 200 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		f.Close()
		return func() {}, fmt.Errorf("locking %s: %w", UILockFile, err)
	}
	return func() { f.Close() }, nil // closing releases the lock
}

// UIProbe tells whether a chat window is open for a home.
type UIProbe struct {
	f *os.File
}

// NewUIProbe opens home's UI lock for probing.
func NewUIProbe(home string) (*UIProbe, error) {
	f, err := openLock(home, UILockFile)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", UILockFile, err)
	}
	return &UIProbe{f: f}, nil
}

// Open reports whether some process holds the UI lock. When unsure (an
// error other than "held") it says open: a missed popup beats a double one.
func (p *UIProbe) Open() bool {
	fd := int(p.f.Fd())
	err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		return false
	}
	return true
}

// Close releases the probe's file.
func (p *UIProbe) Close() error { return p.f.Close() }

// UIOpen is a one-off probe.
func UIOpen(home string) bool {
	p, err := NewUIProbe(home)
	if err != nil {
		return false
	}
	defer p.Close()
	return p.Open()
}

// acquireRun takes the run lock, waiting (until stop is closed) while another
// notifier holds it, so a duplicate start idles instead of crash-looping.
func acquireRun(home string, stop <-chan struct{}, every time.Duration) (*os.File, error) {
	f, err := openLock(home, RunLockFile)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", RunLockFile, err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("locking %s: %w", RunLockFile, err)
		}
		select {
		case <-stop:
			f.Close()
			return nil, errStopped
		case <-time.After(every):
		}
	}
}

var errStopped = errors.New("stopped")

// Running reports whether a background notifier is running for home.
func Running(home string) bool {
	f, err := os.OpenFile(filepath.Join(home, RunLockFile), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
