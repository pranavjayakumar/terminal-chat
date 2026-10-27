package tui

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeTool(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// xclip, xsel and wl-copy leave a daemon holding the inherited pipes; the
// copy must not wait for it. Proof without a wall-clock bound: the copy
// returns cleanly while the daemon is still alive. The generous deadline is
// so a loaded machine (slow fork/exec) cannot fail it; a copy that did wait
// would hit it and error.
func TestDefaultExtCopyDoesNotWaitForDaemon(t *testing.T) {
	dir := t.TempDir()
	out, pidf := filepath.Join(dir, "got"), filepath.Join(dir, "pid")
	fakeTool(t, "xclip", "/bin/cat > "+out+"\n/bin/sleep 300 &\necho $! > "+pidf)
	if err := extCopyWithin(context.Background(), "hello", time.Minute); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(pidf)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("daemon gone (%v): the copy waited for it", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "hello" {
		t.Fatalf("tool got %q", b)
	}
}

func TestDefaultExtCopyNoToolOrFailing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := defaultExtCopy(context.Background(), "x"); err == nil {
		t.Fatal("no tool must be an error")
	}
	fakeTool(t, "pbcopy", "exit 1")
	if err := defaultExtCopy(context.Background(), "x"); err == nil {
		t.Fatal("failing tool must be an error")
	}
}

// A hung tool is killed at the deadline, long before it would exit itself.
func TestDefaultExtCopyHonorsDeadline(t *testing.T) {
	fakeTool(t, "pbcopy", "exec /bin/sleep 60")
	start := time.Now()
	if err := extCopyWithin(context.Background(), "x", 200*time.Millisecond); err == nil {
		t.Fatal("hung tool must be an error")
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("took %v: the deadline did not kill the tool", d)
	}
}
