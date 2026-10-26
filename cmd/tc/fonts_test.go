package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/iconfont"
	"github.com/skailama/tc/internal/release/releasetest"
)

func TestFontsCommand(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no per-user font folder here")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("PATH", "")
	t.Setenv("SSH_TTY", "")
	t.Setenv("SSH_CONNECTION", "")
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := cmdFonts(args, &out)
		return out.String(), err
	}
	if out, _ := run("status"); !strings.Contains(out, "not installed") {
		t.Fatalf("status: %s", out)
	}
	var code exitCode
	if _, err := run("status", "--quiet"); !errors.As(err, &code) || code != 1 {
		t.Fatalf("quiet status of a missing font: %v", err)
	}
	out, err := run("install")
	if err != nil || !strings.Contains(out, "Installed tc icons") || !strings.Contains(out, "restart") {
		t.Fatalf("install: %v %s", err, out)
	}
	if _, err := run("status", "--quiet"); err != nil {
		t.Fatalf("quiet status of an installed font: %v", err)
	}
	if out, _ := run("status"); !strings.Contains(out, "up to date") {
		t.Fatalf("status: %s", out)
	}
	out, _ = run("test")
	if !strings.Contains(out, "") || !strings.Contains(out, "talking") || strings.Count(out, "\n") < 25 {
		t.Fatalf("test:\n%s", out)
	}
	if out, _ := run("refresh"); out != "" {
		t.Fatalf("refresh of a current font said %q", out)
	}
	_, p, _ := iconfont.Status()
	os.WriteFile(p, []byte("old"), 0o644)
	if out, _ := run("refresh"); !strings.Contains(out, "Refreshed") {
		t.Fatalf("refresh of a stale font: %q", out)
	}
	if out, _ := run("remove"); !strings.Contains(out, "Removed") {
		t.Fatalf("remove: %s", out)
	}
	if out, _ := run("remove"); !strings.Contains(out, "aren't installed") {
		t.Fatalf("remove twice: %s", out)
	}
	if _, err := run("nope"); !errors.Is(err, errUsage) {
		t.Fatal("unknown subcommand")
	}
	t.Setenv("SSH_TTY", "/dev/pts/1")
	if out, _ := run("install"); !strings.Contains(out, "over SSH") {
		t.Fatalf("no SSH note: %s", out)
	}
}

// tc update refreshes an installed icon font with the new binary's, in one
// line, whether or not there was a new release.
func TestUpdateRefreshesFonts(t *testing.T) {
	srv := releasetest.New(t, "v0.3.0")
	srv.Publish(t, "v0.3.0", map[string][]byte{"linux_arm64": []byte("NEW-ARM")})
	exe := fakeInstall(t)
	var calls []string
	req := updateReq{fonts: func(_ context.Context, e string) string {
		b, _ := os.ReadFile(e)
		calls = append(calls, string(b))
		return "Refreshed tc icons to this version."
	}}
	var out bytes.Buffer
	if err := runUpdate(context.Background(), &out, srv.Source(), exe, "v0.2.0", "linux", "arm64", req); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "NEW-ARM" || !strings.HasSuffix(out.String(), "Refreshed tc icons to this version.\n") {
		t.Fatalf("after an update: calls %q, output %s", calls, out.String())
	}
	out.Reset()
	if err := runUpdate(context.Background(), &out, srv.Source(), exe, "v0.3.0", "linux", "arm64", req); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !strings.Contains(out.String(), "up to date") || !strings.Contains(out.String(), "Refreshed") {
		t.Fatalf("up to date: %s", out.String())
	}
}
