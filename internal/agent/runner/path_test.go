package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeBin(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func testEnv(home, path, shell string) (*map[string]string, pathEnv) {
	vars := map[string]string{"PATH": path}
	return &vars, pathEnv{
		getenv:    func(k string) string { return vars[k] },
		setenv:    func(k, v string) error { vars[k] = v; return nil },
		home:      home,
		shellPath: func(context.Context) string { return shell },
	}
}

// A service manager's bare PATH still finds claude in ~/.local/bin.
func TestEnsurePathFindsHomeInstall(t *testing.T) {
	home := t.TempDir()
	fakeBin(t, filepath.Join(home, ".local", "bin"), "claude")
	vars, e := testEnv(home, "/usr/bin:/bin", "")
	if err := ensurePath(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	got := (*vars)["PATH"]
	if !strings.HasPrefix(got, "/usr/bin:/bin:") || !strings.Contains(got, filepath.Join(home, ".local", "bin")) {
		t.Fatalf("PATH = %q", got)
	}
}

// claude somewhere only the user's shell knows about is found through it.
func TestEnsurePathAsksLoginShell(t *testing.T) {
	home, odd := t.TempDir(), t.TempDir()
	fakeBin(t, odd, "claude")
	vars, e := testEnv(home, "/usr/bin", "/usr/bin:"+odd+":relative/dir")
	if err := ensurePath(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if got := (*vars)["PATH"]; !strings.Contains(got, odd) || strings.Contains(got, "relative") {
		t.Fatalf("PATH = %q", got)
	}
}

func TestEnsurePathNoClaude(t *testing.T) {
	_, e := testEnv(t.TempDir(), "/nonexistent", "/also/nonexistent")
	if err := ensurePath(context.Background(), e); !errors.Is(err, ErrNoClaude) {
		t.Fatalf("err = %v, want ErrNoClaude", err)
	}
}

func TestParseShellPath(t *testing.T) {
	out := "Welcome!\nzsh: no job control\n\n" + pathMark + "/a:/b\n"
	if got := parseShellPath(out); got != "/a:/b" {
		t.Fatalf("got %q", got)
	}
	if got := parseShellPath("noise only"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// The real login shell answers with a PATH and returns in time.
func TestLoginShellPathReal(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the user's shell")
	}
	t.Setenv("SHELL", "/bin/sh")
	if p := loginShellPath(context.Background()); !strings.Contains(p, "/") {
		t.Fatalf("login shell PATH = %q", p)
	}
}
