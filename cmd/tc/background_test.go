package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/background"
	"github.com/skailama/tc/internal/release/releasetest"
)

// tc update restarts tc background once the new binary is in place, and
// only then.
func TestUpdateRestartsBackground(t *testing.T) {
	srv := releasetest.New(t, "v0.5.0")
	srv.Publish(t, "v0.5.0", map[string][]byte{"linux_amd64": []byte("NEW")})
	exe := fakeInstall(t)
	var restartedWith string
	req := updateReq{restart: func(context.Context) string {
		b, _ := os.ReadFile(exe)
		restartedWith = string(b)
		return "Restarted tc background."
	}}
	var out bytes.Buffer
	if err := runUpdate(context.Background(), &out, srv.Source(), exe, "v0.4.0", "linux", "amd64", req); err != nil {
		t.Fatal(err)
	}
	if restartedWith != "NEW" || !strings.HasSuffix(out.String(), "Restarted tc background.\n") {
		t.Fatalf("restart saw %q; output %q", restartedWith, out.String())
	}
	restartedWith = ""
	out.Reset()
	if err := runUpdate(context.Background(), &out, srv.Source(), exe, "v0.5.0", "linux", "amd64", req); err != nil {
		t.Fatal(err)
	}
	if restartedWith != "" {
		t.Fatal("restarted although nothing was updated")
	}
}

func testManager(t *testing.T, goos string) (background.Manager, *[][]string) {
	t.Helper()
	uh := t.TempDir()
	sv, err := background.NewService(background.ServiceSpec{GOOS: goos, Home: filepath.Join(uh, ".config", "tc"), DefaultHome: true,
		Exe: filepath.Join(uh, ".local", "bin", "tc"), UserHome: uh})
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	run := func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, append([]string{name}, args...))
		return "", nil
	}
	return background.Manager{S: sv, Run: run, UID: 501, Sleep: func(time.Duration) {}}, &calls
}

func TestBackgroundOnOffStatus(t *testing.T) {
	if _, err := background.OSSender(); err != nil {
		t.Skip("no desktop notifier here:", err)
	}
	m, calls := testManager(t, "darwin")
	home := m.S.Home
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := backgroundOn(context.Background(), &out, m, home); err == nil || !strings.Contains(err.Error(), "tc join") {
		t.Fatalf("on before joining: %v", err)
	}
	if m.Installed() || len(*calls) != 0 {
		t.Fatal("installed before joining")
	}
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{}`), 0o600)

	backgroundStatus(&out, m, home, time.Now())
	if !strings.Contains(out.String(), "tc background: off") || !strings.Contains(out.String(), "running: no") {
		t.Fatalf("status before on:\n%s", out.String())
	}
	out.Reset()
	// Nothing really starts (fake launchctl): on says so instead of hanging.
	if err := backgroundOn(context.Background(), &out, m, home); err != nil {
		t.Fatal(err)
	}
	if !m.Installed() || !strings.Contains(out.String(), "tc background is on") || !strings.Contains(out.String(), "isn't running yet") {
		t.Fatalf("on:\n%s", out.String())
	}
	out.Reset()
	backgroundStatus(&out, m, home, time.Now())
	if !strings.Contains(out.String(), "tc background: on ("+m.S.File+")") {
		t.Fatalf("status after on:\n%s", out.String())
	}

	// A notifier that ran left its files: off clears them.
	for _, f := range []string{background.StatusFile, background.LogFile, "background.err.log"} {
		os.WriteFile(filepath.Join(home, f), []byte("{}"), 0o600)
	}
	out.Reset()
	if err := backgroundOff(context.Background(), &out, m, home); err != nil {
		t.Fatal(err)
	}
	if m.Installed() || !strings.Contains(out.String(), "tc background is off") {
		t.Fatalf("off:\n%s", out.String())
	}
	for _, f := range []string{background.StatusFile, background.LogFile, "background.err.log"} {
		if _, err := os.Stat(filepath.Join(home, f)); err == nil {
			t.Errorf("%s left behind", f)
		}
	}
	out.Reset()
	if err := backgroundOff(context.Background(), &out, m, home); err != nil || !strings.Contains(out.String(), "already off") {
		t.Fatalf("off twice: %v %s", err, out.String())
	}
}

func TestBackgroundUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"maybe"}, {"on", "extra"}} {
		if err := cmdBackground(args); err != errUsage {
			t.Errorf("%q: %v", args, err)
		}
	}
}
