package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func writeKeyPair(t *testing.T, path string, priv crypto.PrivateKey, passphrase string) ssh.PublicKey {
	t.Helper()
	var blk *pem.Block
	var err error
	if passphrase != "" {
		blk, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	} else {
		blk, err = ssh.MarshalPrivateKey(priv, "")
	}
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(s.PublicKey()), 0o644); err != nil {
		t.Fatal(err)
	}
	return s.PublicKey()
}

// Candidates: every agent key (any type), then the id_* files the agent
// doesn't hold; a locked file is listed with an error that says ssh-add.
func TestCandidates(t *testing.T) {
	dir := t.TempDir()
	_, agentEd, _ := ed25519.GenerateKey(rand.Reader)
	agentEC, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	serveTestAgent(t, agentEd, agentEC)

	inAgent := writeKeyPair(t, filepath.Join(dir, "id_ed25519"), agentEd, "") // the agent's copy
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecPub := writeKeyPair(t, filepath.Join(dir, "id_ecdsa"), ec, "")
	_, locked, _ := ed25519.GenerateKey(rand.Reader)
	lockedPub := writeKeyPair(t, filepath.Join(dir, "id_rsa"), locked, "secret") // name doesn't matter

	cs := Candidates(dir)
	if len(cs) != 4 {
		t.Fatalf("%d candidates: %+v", len(cs), cs)
	}
	if cs[0].Path != "" || Fingerprint(cs[0].Pub) != Fingerprint(inAgent) || cs[1].Path != "" || cs[1].Signer == nil {
		t.Fatalf("agent keys first: %+v", cs[:2])
	}
	if cs[2].Path != filepath.Join(dir, "id_ecdsa") || Fingerprint(cs[2].Pub) != Fingerprint(ecPub) || cs[2].Signer == nil {
		t.Fatalf("file key: %+v", cs[2])
	}
	if cs[3].Signer != nil || cs[3].Err == nil || !strings.Contains(cs[3].Err.Error(), "ssh-add") ||
		Fingerprint(cs[3].Pub) != Fingerprint(lockedPub) {
		t.Fatalf("locked key: %+v", cs[3])
	}
	if cs[0].Spec() != Fingerprint(inAgent) || cs[2].Spec() != cs[2].Path {
		t.Fatal("specs")
	}
	if !strings.HasPrefix(cs[1].Describe(), "ssh-agent key SHA256:") {
		t.Fatal(cs[1].Describe())
	}
}

// Load takes a fingerprint (an agent key the config remembered) and a
// public key file whose private half is in the agent.
func TestLoadFingerprintAndPublicFile(t *testing.T) {
	_, a, _ := ed25519.GenerateKey(rand.Reader)
	_, b, _ := ed25519.GenerateKey(rand.Reader)
	serveTestAgent(t, a, b)
	bs, _ := ssh.NewSignerFromKey(b)
	fp := Fingerprint(bs.PublicKey())
	s, err := Load(fp)
	if err != nil || Fingerprint(s.PublicKey()) != fp {
		t.Fatalf("Load(fp): %v", err)
	}
	pubPath := filepath.Join(t.TempDir(), "gh.pub")
	os.WriteFile(pubPath, ssh.MarshalAuthorizedKey(bs.PublicKey()), 0o644)
	if s, err := Load(pubPath); err != nil || Fingerprint(s.PublicKey()) != fp {
		t.Fatalf("Load(.pub): %v", err)
	}
	_, c, _ := ed25519.GenerateKey(rand.Reader)
	cs, _ := ssh.NewSignerFromKey(c)
	os.WriteFile(pubPath, ssh.MarshalAuthorizedKey(cs.PublicKey()), 0o644)
	if _, err := Load(pubPath); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("Load(.pub not in agent): %v", err)
	}
	if _, err := Load(Fingerprint(cs.PublicKey())); err == nil {
		t.Fatal("Load(unknown fp) succeeded")
	}
}
