package release_test

import (
	"strings"
	"testing"

	"github.com/skailama/tc/internal/release/releasetest"
)

const iconsPrompt = "Install tc icons (a small font for sharper icons)? [Y/n]"

// stubFontsTC logs each run's argv; `fonts status --quiet` exits with
// $TC_STUB_FONT (default 1: not installed).
func stubFontsTC() []byte {
	return []byte(`#!/bin/sh
[ "$1" = version ] && { echo 'tc stub'; exit 0; }
printf '%s\n' "$*" >> "$TC_STUB_LOG"
[ "$1 $2" = "fonts status" ] && exit "${TC_STUB_FONT:-1}"
exit 0
`)
}

func publishFontsStub(t *testing.T) *releasetest.Server {
	srv := releasetest.New(t, "v0.5.0")
	bins := map[string][]byte{}
	for _, oa := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
		bins[oa] = stubFontsTC()
	}
	srv.Publish(t, "v0.5.0", bins)
	return srv
}

// On a desktop terminal the installer offers the icon font once: yes (or
// Enter) installs it, anything else leaves a hint; never over SSH, never
// on a headless Linux box, never when the font is already there.
func TestInstallOffersIcons(t *testing.T) {
	srv := publishFontsStub(t)
	for _, c := range []struct {
		name, unameS, input string
		env                 []string
		ask                 bool
		want                string
	}{
		{"yes", "Darwin", "y\n", nil, true, "fonts status --quiet|fonts install"},
		{"enter", "Darwin", "\n", nil, true, "fonts status --quiet|fonts install"},
		{"no", "Darwin", "n\n", nil, true, "fonts status --quiet"},
		{"ssh", "Darwin", "y\n", []string{"SSH_CONNECTION=1.2.3.4 5 6.7.8.9 22"}, false, ""},
		{"installed", "Darwin", "y\n", []string{"TC_STUB_FONT=0"}, false, "fonts status --quiet"},
		{"linux desktop", "Linux", "y\n", []string{"DISPLAY=:0"}, true, "fonts status --quiet|fonts install"},
		{"headless linux", "Linux", "y\n", nil, false, ""},
		{"TC_ICONS=0", "Darwin", "y\n", []string{"TC_ICONS=0"}, false, ""},
	} {
		env := append([]string{"TC_ICONS="}, c.env...)
		r := runInstall(t, srv, installOpts{unameS: c.unameS, unameM: "x86_64", tty: true, input: c.input, env: env})
		out := r.stdout + r.stderr
		if r.err != nil {
			t.Fatalf("%s: %v\n%s", c.name, r.err, out)
		}
		if strings.Contains(out, iconsPrompt) != c.ask {
			t.Fatalf("%s: asked=%v, want %v:\n%s", c.name, !c.ask, c.ask, out)
		}
		if c.name == "no" && !strings.Contains(out, "tc fonts install") {
			t.Fatalf("no: no hint for later:\n%s", out)
		}
		if got := strings.Join(r.stubLog(t), "|"); got != c.want {
			t.Fatalf("%s: tc ran %q, want %q", c.name, got, c.want)
		}
	}
}

// Without a terminal it never asks; TC_ICONS=1 installs anyway.
func TestInstallIconsWithoutTTY(t *testing.T) {
	srv := publishFontsStub(t)
	for _, c := range []struct {
		env  []string
		want string
	}{{[]string{"TC_ICONS="}, ""}, {[]string{"TC_ICONS=1"}, "fonts install"}} {
		r := runInstall(t, srv, installOpts{unameS: hostOS(), unameM: "x86_64", env: c.env})
		if r.err != nil || strings.Contains(r.stderr, iconsPrompt) {
			t.Fatalf("%v: %v\n%s", c.env, r.err, r.stderr)
		}
		if got := strings.Join(r.stubLog(t), "|"); got != c.want {
			t.Fatalf("%v: tc ran %q, want %q", c.env, got, c.want)
		}
	}
}

// A tc from before tc fonts (TC_VERSION pinned to an old release) fails
// `fonts status` with a usage error: no offer, no error output. And the
// next step stays the last line.
func TestInstallIconsOldTCAndOrder(t *testing.T) {
	srv := publishFontsStub(t)
	r := runInstall(t, srv, installOpts{unameS: "Darwin", unameM: "x86_64", tty: true, input: "y\n", env: []string{"TC_ICONS=", "TC_STUB_FONT=2"}})
	out := r.stdout + r.stderr
	if r.err != nil || strings.Contains(out, iconsPrompt) || strings.Contains(out, "couldn't install") {
		t.Fatalf("old tc: %v\n%s", r.err, out)
	}
	r = runInstall(t, srv, installOpts{unameS: "Darwin", unameM: "x86_64", tty: true, input: "n\n", env: []string{"TC_ICONS="}})
	lines := strings.Split(strings.TrimSpace(r.stdout+r.stderr), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "next:") {
		t.Fatalf("last line %q", last)
	}
}
