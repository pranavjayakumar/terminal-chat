package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/release"
	"github.com/skailama/tc/internal/release/releasetest"
)

func fakeInstall(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "tc")
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestUpdateReplacesAtomically(t *testing.T) {
	srv := releasetest.New(t, "v0.3.0")
	srv.Publish(t, "v0.3.0", map[string][]byte{"linux_arm64": []byte("NEW-ARM"), "linux_amd64": []byte("NEW-AMD")})
	exe := fakeInstall(t)
	var out bytes.Buffer
	err := runUpdate(context.Background(), &out, srv.Source(), exe, "v0.2.0", "linux", "arm64", updateReq{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(exe)
	st, _ := os.Stat(exe)
	if string(b) != "NEW-ARM" || st.Mode().Perm() != 0o755 {
		t.Fatalf("binary %q mode %v", b, st.Mode())
	}
	if !strings.Contains(out.String(), "v0.2.0 → v0.3.0") {
		t.Fatalf("output: %s", out.String())
	}
	ents, _ := os.ReadDir(filepath.Dir(exe))
	if len(ents) != 1 {
		t.Fatalf("leftovers: %v", ents)
	}

	out.Reset()
	if err := runUpdate(context.Background(), &out, srv.Source(), exe, "v0.3.0", "linux", "arm64", updateReq{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("up to date: %s", out.String())
	}
}

// tc update keeps the flavor that's running: a voice build fetches
// tc-voice_*, the default build tc_*, from the same signed checksums.txt.
func TestUpdateKeepsVoiceFlavor(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	srv.Publish(t, "v0.4.0", map[string][]byte{"linux_amd64": []byte("PLAIN"), "tc-voice_linux_amd64": []byte("VOICE")})
	for _, c := range []struct {
		voice      bool
		want, note string
	}{{false, "PLAIN", ""}, {true, "VOICE", "(voice build)"}} {
		exe := fakeInstall(t)
		src := srv.Source()
		src.Voice = c.voice
		var out bytes.Buffer
		if err := runUpdate(context.Background(), &out, src, exe, "v0.3.0", "linux", "amd64", updateReq{}); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(exe); string(b) != c.want {
			t.Fatalf("voice=%v installed %q", c.voice, b)
		}
		if !strings.Contains(out.String(), c.note) {
			t.Fatalf("output: %s", out.String())
		}
	}
	// A release without voice assets fails the voice build cleanly.
	srv.Publish(t, "v0.4.0", map[string][]byte{"linux_amd64": []byte("PLAIN")})
	exe := fakeInstall(t)
	src := srv.Source()
	src.Voice = true
	err := runUpdate(context.Background(), &bytes.Buffer{}, src, exe, "v0.3.0", "linux", "amd64", updateReq{})
	if err == nil || !strings.Contains(err.Error(), "tc-voice_linux_amd64.tar.gz") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatalf("replaced: %q", b)
	}
}

func TestUpdateChecksumMismatchRefuses(t *testing.T) {
	srv := releasetest.New(t, "v0.3.0")
	srv.Publish(t, "v0.3.0", map[string][]byte{"darwin_arm64": []byte("GOOD")})
	srv.Put("v0.3.0", "tc_darwin_arm64.tar.gz", releasetest.Tarball([]byte("TAMPERED")))
	exe := fakeInstall(t)
	var out bytes.Buffer
	err := runUpdate(context.Background(), &out, srv.Source(), exe, "v0.2.0", "darwin", "arm64", updateReq{})
	if !errors.Is(err, release.ErrChecksum) {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatalf("binary replaced despite mismatch: %q", b)
	}
	if ents, _ := os.ReadDir(filepath.Dir(exe)); len(ents) != 1 {
		t.Fatalf("leftovers: %v", ents)
	}
}

func TestUpdateCheckOnly(t *testing.T) {
	srv := releasetest.New(t, "v0.3.0")
	exe := fakeInstall(t)
	src := srv.Source()
	for _, c := range []struct{ current, want string }{
		{"v0.2.0", "Update available: v0.2.0 → v0.3.0. Run: tc update\n"},
		{"v0.3.0", "tc v0.3.0 is up to date.\n"},
		{"dev", "This is a dev build; the latest release is v0.3.0.\n"},
	} {
		var out bytes.Buffer
		if err := runUpdate(context.Background(), &out, src, exe, c.current, "linux", "amd64", updateReq{check: true}); err != nil {
			t.Fatal(err)
		}
		if out.String() != c.want {
			t.Errorf("%s: got %q want %q", c.current, out.String(), c.want)
		}
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatal("--check touched the binary")
	}
	for _, h := range srv.Hits() {
		if strings.Contains(h, "/download/") {
			t.Fatalf("--check downloaded %s", h)
		}
	}
}

func TestUpdateRefusesUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	srv := releasetest.New(t, "v0.3.0")
	srv.Publish(t, "v0.3.0", map[string][]byte{"linux_amd64": []byte("NEW")})
	exe := fakeInstall(t)
	dir := filepath.Dir(exe)
	os.Chmod(dir, 0o555)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	err := runUpdate(context.Background(), &bytes.Buffer{}, srv.Source(), exe, "v0.2.0", "linux", "amd64", updateReq{})
	if err == nil || !strings.Contains(err.Error(), "isn't writable") {
		t.Fatalf("err = %v", err)
	}
	for _, h := range srv.Hits() {
		if strings.Contains(h, "/download/") {
			t.Fatalf("downloaded before checking permissions: %s", h)
		}
	}
}

// A build without an embedded release key never replaces itself.
func TestUpdatePlaceholderKeyRefuses(t *testing.T) {
	srv := releasetest.New(t, "v0.3.0")
	srv.Publish(t, "v0.3.0", map[string][]byte{"linux_amd64": []byte("NEW")})
	exe := fakeInstall(t)
	src := &release.Source{Base: srv.Base, Signers: release.Placeholder}
	err := runUpdate(context.Background(), &bytes.Buffer{}, src, exe, "v0.2.0", "linux", "amd64", updateReq{})
	if !errors.Is(err, release.ErrNoSigningKey) || !strings.Contains(err.Error(), "no release signing key") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatalf("binary replaced: %q", b)
	}
}

// Signed by a key this build doesn't trust: refused, binary untouched.
func TestUpdateUntrustedSignatureRefuses(t *testing.T) {
	srv := releasetest.New(t, "v0.3.0")
	srv.Publish(t, "v0.3.0", map[string][]byte{"linux_amd64": []byte("NEW")})
	exe := fakeInstall(t)
	src := srv.Source()
	src.Signers = releasetest.AllowedSigners(releasetest.NewKey(t).PublicKey())
	err := runUpdate(context.Background(), &bytes.Buffer{}, src, exe, "v0.2.0", "linux", "amd64", updateReq{})
	if !errors.Is(err, release.ErrSignature) {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatalf("binary replaced: %q", b)
	}
}
