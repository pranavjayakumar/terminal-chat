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

// tc update --voice switches a plain build to the voice build, even when
// it's already the latest release (the /huddle error points here, #85).
func TestUpdateSwitchesToVoice(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	srv.Publish(t, "v0.4.0", map[string][]byte{"linux_amd64": []byte("PLAIN"), "tc-voice_linux_amd64": []byte("VOICE")})
	for _, current := range []string{"v0.4.0", "v0.3.0", "dev"} {
		exe := fakeInstall(t)
		var probed []byte
		req := updateReq{toVoice: true, runs: func(_ context.Context, dir string, bin []byte) error {
			if dir != filepath.Dir(exe) {
				t.Errorf("probe dir %s", dir)
			}
			probed = bin
			return nil
		}}
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

// A voice build that can't start here (old glibc) keeps the plain build.
func TestUpdateVoiceThatDoesNotRunKeepsPlain(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	srv.Publish(t, "v0.4.0", map[string][]byte{"linux_amd64": []byte("PLAIN"), "tc-voice_linux_amd64": []byte("VOICE")})
	exe := fakeInstall(t)
	req := updateReq{toVoice: true, runs: func(context.Context, string, []byte) error {
		return errors.New("GLIBC_2.35 not found")
	}}
	err := runUpdate(context.Background(), &bytes.Buffer{}, srv.Source(), exe, "v0.4.0", "linux", "amd64", req)
	if err == nil || !strings.Contains(err.Error(), "glibc 2.35") || !strings.Contains(err.Error(), "kept the plain build") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatalf("replaced: %q", b)
	}
	if ents, _ := os.ReadDir(filepath.Dir(exe)); len(ents) != 1 {
		t.Fatalf("leftovers: %v", ents)
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
	if err == nil || !strings.Contains(err.Error(), "Homebrew") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatal("replaced a brew binary")
	}
}

// runsHere really runs the binary: one that fails is refused with its
// output, one that works passes, and no probe file is left behind.
func TestRunsHere(t *testing.T) {
	dir := t.TempDir()
	err := runsHere(context.Background(), dir, []byte("#!/bin/sh\necho 'needs GLIBC_2.35' >&2\nexit 1\n"))
	if err == nil || !strings.Contains(err.Error(), "GLIBC_2.35") {
		t.Fatalf("failing binary: %v", err)
	}
	if err := runsHere(context.Background(), dir, []byte("#!/bin/sh\n[ \"$1\" = version ] || exit 2\n")); err != nil {
		t.Fatalf("working binary: %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("leftovers: %v", ents)
	}
}
