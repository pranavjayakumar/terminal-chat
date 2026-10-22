package release_test

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/release/releasetest"
)

// onTTY runs cmd on a pseudo-terminal via script(1), typing input (and
// keeping the terminal open until cmd is done, so EOF can't end it early).
func onTTY(t *testing.T, cmd *exec.Cmd, input string) *exec.Cmd {
	t.Helper()
	bin, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script(1) not installed")
	}
	var c *exec.Cmd
	if runtime.GOOS == "darwin" {
		c = exec.Command(bin, append([]string{"-q", "/dev/null"}, cmd.Args...)...)
	} else {
		q := make([]string, len(cmd.Args))
		for i, a := range cmd.Args {
			q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		c = exec.Command(bin, "-q", "-e", "-c", strings.Join(q, " "), "/dev/null")
	}
	c.Dir = cmd.Dir
	// util-linux script runs the command with $SHELL.
	c.Env = append(append([]string(nil), cmd.Env...), "SHELL=/bin/sh")
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c.Stdin = pr
	go func() {
		time.Sleep(200 * time.Millisecond)
		pw.WriteString(input)
	}()
	t.Cleanup(func() { pw.Close(); pr.Close() })
	return c
}

// stubJoinTC appends each run's argv to $TC_STUB_LOG and never reads stdin
// (on a terminal that would wait for ^D). TC_STUB_FAIL=join|background
// makes that command fail.
func stubJoinTC() []byte {
	return []byte(`#!/bin/sh
[ "$1" = version ] && { echo 'tc stub'; exit 0; }
printf '%s\n' "$*" >> "$TC_STUB_LOG"
[ "${TC_STUB_FAIL:-}" = "$1" ] && exit 1
exit 0
`)
}

func publishJoinStub(t *testing.T) *releasetest.Server {
	srv := releasetest.New(t, "v0.5.0")
	bins := map[string][]byte{}
	for _, oa := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
		bins[oa] = stubJoinTC()
	}
	srv.Publish(t, "v0.5.0", bins)
	return srv
}

func hostOS() string {
	if runtime.GOOS == "darwin" {
		return "Darwin"
	}
	return "Linux"
}

const bgPrompt = "Get notifications when tc is closed? [Y/n]"

// After a join on a terminal install.sh offers tc background; yes (or
// Enter) turns it on, anything else doesn't.
func TestInstallOffersBackgroundOnTTY(t *testing.T) {
	srv := publishJoinStub(t)
	for _, c := range []struct {
		input string
		on    bool
	}{{"y\n", true}, {"\n", true}, {"YES\n", true}, {"n\n", false}, {"no thanks\n", false}} {
		r := runInstall(t, srv, installOpts{unameS: hostOS(), unameM: "x86_64", args: []string{installInvite}, tty: true, input: c.input})
		out := r.stdout + r.stderr
		if r.err != nil {
			t.Fatalf("%q: %v\n%s", c.input, r.err, out)
		}
		if !strings.Contains(out, bgPrompt) {
			t.Fatalf("%q: no prompt:\n%s", c.input, out)
		}
		want := []string{"join"}
		if c.on {
			want = append(want, "background on")
		} else if !strings.Contains(out, "tc background on") {
			t.Fatalf("%q: no hint for later:\n%s", c.input, out)
		}
		if got := r.stubLog(t); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%q: tc ran %q, want %q", c.input, got, want)
		}
	}
}

// Without a terminal (CI, scripts) it never asks; TC_BACKGROUND decides.
func TestInstallBackgroundWithoutTTY(t *testing.T) {
	srv := publishJoinStub(t)
	for _, c := range []struct {
		env  []string
		want string
	}{{nil, "join"}, {[]string{"TC_BACKGROUND=0"}, "join"}, {[]string{"TC_BACKGROUND=1"}, "join|background on"}} {
		r := runInstall(t, srv, installOpts{unameS: hostOS(), unameM: "x86_64", args: []string{installInvite}, env: c.env})
		if r.err != nil {
			t.Fatalf("%v: %v\n%s", c.env, r.err, r.stderr)
		}
		if strings.Contains(r.stderr, bgPrompt) {
			t.Fatalf("%v: asked without a terminal:\n%s", c.env, r.stderr)
		}
		if got := strings.Join(r.stubLog(t), "|"); got != c.want {
			t.Fatalf("%v: tc ran %q, want %q", c.env, got, c.want)
		}
	}
	// No invite, no join: nothing to offer.
	r := runInstall(t, srv, installOpts{unameS: hostOS(), unameM: "x86_64", env: []string{"TC_BACKGROUND=1"}})
	if r.err != nil || r.stubLog(t) != nil {
		t.Fatalf("no invite: %v, tc ran %q", r.err, r.stubLog(t))
	}
}

func TestInstallBackgroundOnTTYOverrides(t *testing.T) {
	srv := publishJoinStub(t)
	r := runInstall(t, srv, installOpts{unameS: hostOS(), unameM: "x86_64", args: []string{installInvite}, tty: true,
		input: "y\n", env: []string{"TC_BACKGROUND=0"}})
	if r.err != nil || strings.Contains(r.stdout, bgPrompt) || strings.Join(r.stubLog(t), "|") != "join" {
		t.Fatalf("TC_BACKGROUND=0 on a terminal: %v, ran %q\n%s", r.err, r.stubLog(t), r.stdout)
	}
}

// A failed join never offers it; a failed tc background on doesn't fail
// the install (tc is installed and joined).
func TestInstallBackgroundFailures(t *testing.T) {
	srv := publishJoinStub(t)
	r := runInstall(t, srv, installOpts{unameS: hostOS(), unameM: "x86_64", args: []string{installInvite}, tty: true,
		input: "y\n", env: []string{"TC_STUB_FAIL=join"}})
	if r.err == nil || strings.Contains(r.stdout, bgPrompt) || strings.Join(r.stubLog(t), "|") != "join" {
		t.Fatalf("failed join: err %v, ran %q\n%s", r.err, r.stubLog(t), r.stdout)
	}
	r = runInstall(t, srv, installOpts{unameS: hostOS(), unameM: "x86_64", args: []string{installInvite}, tty: true,
		input: "y\n", env: []string{"TC_STUB_FAIL=background"}})
	if r.err != nil || !strings.Contains(r.stdout, "couldn't turn on notifications") {
		t.Fatalf("failed background on: %v\n%s", r.err, r.stdout)
	}
}
