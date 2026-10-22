package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/release/releasetest"
)

// voiceProbe is a runs func that reports bin as tc <tag> (voice).
func voiceProbe(tag string, seen *[]byte) func(context.Context, string, []byte) (string, error) {
	return func(_ context.Context, _ string, bin []byte) (string, error) {
		if seen != nil {
			*seen = bin
		}
		return "tc " + tag + " (voice)", nil
	}
}

// tc update --voice switches a plain build to the voice build, even when
// it's already the latest release (the /huddle error points here, #85).
func TestUpdateSwitchesToVoice(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	srv.Publish(t, "v0.4.0", map[string][]byte{"linux_amd64": []byte("PLAIN"), "tc-voice_linux_amd64": []byte("VOICE")})
	for _, current := range []string{"v0.4.0", "v0.3.0", "dev"} {
		exe := fakeInstall(t)
		var probed []byte
		req := updateReq{toVoice: true, runs: voiceProbe("v0.4.0", &probed)}
		var out bytes.Buffer
		if err := runUpdate(context.Background(), &out, srv.Source(), exe, current, "linux", "amd64", req); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(exe); string(b) != "VOICE" || string(probed) != "VOICE" {
			t.Fatalf("%s: installed %q, probed %q", current, b, probed)
		}
		if !strings.Contains(out.String(), "(voice build)") {
			t.Fatalf("output: %s", out.String())
		}
	}

	// --check --voice reports and downloads nothing.
	exe := fakeInstall(t)
	var out bytes.Buffer
	if err := runUpdate(context.Background(), &out, srv.Source(), exe, "v0.4.0", "linux", "amd64", updateReq{check: true, toVoice: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "tc update --voice") {
		t.Fatalf("check output: %s", out.String())
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatal("--check touched the binary")
	}
}

// Switching flavor never installs an older release than the running one.
func TestUpdateVoiceRefusesDowngrade(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	srv.Publish(t, "v0.4.0", map[string][]byte{"linux_amd64": []byte("PLAIN"), "tc-voice_linux_amd64": []byte("VOICE")})
	exe := fakeInstall(t)
	err := runUpdate(context.Background(), &bytes.Buffer{}, srv.Source(), exe, "v0.5.0", "linux", "amd64", updateReq{toVoice: true, runs: voiceProbe("v0.4.0", nil)})
	if err == nil || !strings.Contains(err.Error(), "older than this tc") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatalf("replaced: %q", b)
	}
}

// A voice build that can't start here (old glibc), or that doesn't say it's
// this release's voice build, keeps the plain build.
func TestUpdateVoiceThatDoesNotRunKeepsPlain(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	srv.Publish(t, "v0.4.0", map[string][]byte{"linux_amd64": []byte("PLAIN"), "tc-voice_linux_amd64": []byte("VOICE")})
	for _, c := range []struct {
		name string
		runs func(context.Context, string, []byte) (string, error)
		want string
	}{
		{"glibc", func(context.Context, string, []byte) (string, error) {
			return "", errors.New("GLIBC_2.35 not found")
		}, "glibc 2.35"},
		{"plain build", func(context.Context, string, []byte) (string, error) {
			return "tc v0.4.0", nil
		}, "not \"tc v0.4.0 (voice)\""},
		{"other release", voiceProbe("v0.3.0", nil), "not \"tc v0.4.0 (voice)\""},
	} {
		exe := fakeInstall(t)
		err := runUpdate(context.Background(), &bytes.Buffer{}, srv.Source(), exe, "v0.4.0", "linux", "amd64", updateReq{toVoice: true, runs: c.runs})
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "kept the plain build") {
			t.Fatalf("%s: err = %v", c.name, err)
		}
		if b, _ := os.ReadFile(exe); string(b) != "OLD" {
			t.Fatalf("%s: replaced: %q", c.name, b)
		}
		if ents, _ := os.ReadDir(filepath.Dir(exe)); len(ents) != 1 {
			t.Fatalf("%s: leftovers: %v", c.name, ents)
		}
	}
}

// Homebrew has no voice build: say so, touch nothing.
func TestUpdateVoiceHomebrew(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	exe := filepath.Join(t.TempDir(), "Cellar", "tc", "0.4.0", "bin", "tc")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(exe, []byte("OLD"), 0o755)
	err := runUpdate(context.Background(), &bytes.Buffer{}, srv.Source(), exe, "v0.4.0", "darwin", "arm64", updateReq{toVoice: true})
	if err == nil || !strings.Contains(err.Error(), "Homebrew") || !strings.Contains(err.Error(), "--voice") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatal("replaced a brew binary")
	}
}

// runsHere really runs the binary, from a private directory: one that fails
// is refused with its output, one that works returns its output, and
// nothing is left behind.
func TestRunsHere(t *testing.T) {
	dir := t.TempDir()
	_, err := runsHere(context.Background(), dir, []byte("#!/bin/sh\necho 'needs GLIBC_2.35' >&2\nexit 1\n"))
	if err == nil || !strings.Contains(err.Error(), "GLIBC_2.35") {
		t.Fatalf("failing binary: %v", err)
	}
	script := "#!/bin/sh\n[ \"$1\" = version ] || exit 2\n" +
		"[ \"$(ls -ld \"$(dirname \"$0\")\" | cut -c1-10)\" = drwx------ ] || { echo 'dir not private'; exit 3; }\n" +
		"echo 'tc v9.9.9 (voice)'\n"
	out, err := runsHere(context.Background(), dir, []byte(script))
	if err != nil || out != "tc v9.9.9 (voice)" {
		t.Fatalf("working binary: %q %v", out, err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("leftovers: %v", ents)
	}
}
