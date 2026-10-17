package tui

import (
	"context"
	"os"
	"path/filepath"
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
// copy must not wait for it.
func TestDefaultExtCopyDoesNotWaitForDaemon(t *testing.T) {
	out := filepath.Join(t.TempDir(), "got")
	fakeTool(t, "xclip", "/bin/cat > "+out+"\n/bin/sleep 8 &")
	start := time.Now()
	if err := defaultExtCopy(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("blocked %v on the daemon child", d)
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

func TestDefaultExtCopyHonorsDeadline(t *testing.T) {
	fakeTool(t, "pbcopy", "exec /bin/sleep 30")
	start := time.Now()
	if err := defaultExtCopy(context.Background(), "x"); err == nil {
		t.Fatal("hung tool must be an error")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("took %v", d)
	}
}
