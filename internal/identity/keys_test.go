package identity

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// ServeTestAgent runs an in-memory ssh-agent holding keys on a unix socket
// and points SSH_AUTH_SOCK at it for the test.
func serveTestAgent(t *testing.T, keys ...any) {
	t.Helper()
	// Short path: unix socket paths are limited to ~104 bytes on macOS.
	dir, err := os.MkdirTemp("/tmp", "tcag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	kr := agent.NewKeyring()
	for _, k := range keys {
		if err := kr.Add(agent.AddedKey{PrivateKey: k}); err != nil {
			t.Fatal(err)
		}
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { agent.ServeAgent(kr, c); c.Close() }()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sock)
}

func TestAgentED25519(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	if _, ok := AgentED25519(); ok {
		t.Fatal("no agent: found a key")
	}
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serveTestAgent(t, ec)
	if _, ok := AgentED25519(); ok {
		t.Fatal("ecdsa-only agent: found an ed25519 key")
	}
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	serveTestAgent(t, ec, ed)
	pk, ok := AgentED25519()
	if !ok || pk.Type() != ssh.KeyAlgoED25519 {
		t.Fatalf("got %v %v", pk, ok)
	}
	want, _ := ssh.NewPublicKey(ed.Public())
	if Fingerprint(pk) != Fingerprint(want) {
		t.Fatal("wrong key")
	}
	// Load("") signs with the same key.
	s, err := Load("")
	if err != nil || Fingerprint(s.PublicKey()) != Fingerprint(want) {
		t.Fatalf("Load: %v", err)
	}
}

func TestGenerateKey(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	dir := filepath.Join(t.TempDir(), "tc")
	path := filepath.Join(dir, "id_ed25519")
	s, err := GenerateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v %v", st.Mode(), err)
	}
	if d, _ := os.Stat(dir); d.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", d.Mode())
	}
	pub, _ := os.ReadFile(path + ".pub")
	if !strings.HasPrefix(string(pub), "ssh-ed25519 ") || !strings.HasSuffix(string(pub), " tc\n") {
		t.Fatalf("pub: %q", pub)
	}
	loaded, err := Load(path)
	if err != nil || Fingerprint(loaded.PublicKey()) != Fingerprint(s.PublicKey()) {
		t.Fatalf("Load: %v", err)
	}
	if _, err := GenerateKey(path); err == nil {
		t.Fatal("overwrote an existing key")
	}
	if l2, _ := Load(path); Fingerprint(l2.PublicKey()) != Fingerprint(s.PublicKey()) {
		t.Fatal("key changed")
	}
}

// A crash mid-write must not leave an empty or partial key at path (every
// later join would fail to parse it): until it's complete, the key only
// exists under a temp name.
func TestGenerateKeyAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "id_ed25519")
	var sawPath bool
	var tmpMode os.FileMode
	if _, err := generateKey(path, func(tmp string) {
		_, err := os.Lstat(path)
		sawPath = err == nil
		st, _ := os.Stat(tmp)
		tmpMode = st.Mode().Perm()
	}); err != nil {
		t.Fatal(err)
	}
	if sawPath {
		t.Fatal("key path existed before the key was fully written")
	}
	if tmpMode != 0o600 {
		t.Fatalf("temp key mode %v, want 0600", tmpMode)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Fatalf("leftover temp files: %v", ents)
	}

	// Something already at path (a planted symlink included) is never replaced.
	other := filepath.Join(t.TempDir(), "id_ed25519")
	victim := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(victim, []byte("x"), 0o644)
	os.Symlink(victim, other)
	if _, err := GenerateKey(other); !errors.Is(err, os.ErrExist) {
		t.Fatalf("over a symlink: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "x" {
		t.Fatal("wrote through a symlink")
	}

	// A stale .pub from another key is replaced to match.
	third := filepath.Join(t.TempDir(), "id_ed25519")
	os.WriteFile(third+".pub", []byte("ssh-ed25519 AAAAstale old\n"), 0o644)
	s, err := GenerateKey(third)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := os.ReadFile(third + ".pub")
	if k, _, _, _, err := ssh.ParseAuthorizedKey(pub); err != nil || Fingerprint(k) != Fingerprint(s.PublicKey()) {
		t.Fatalf(".pub doesn't match the new key: %q %v", pub, err)
	}
}

// An existing, too-open TC_HOME is tightened to 0700.
func TestGenerateKeyTightensDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tc")
	os.MkdirAll(dir, 0o755)
	os.Chmod(dir, 0o755)
	if _, err := GenerateKey(filepath.Join(dir, "id_ed25519")); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, want 0700", st.Mode().Perm())
	}
}
