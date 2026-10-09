package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"testing"

	"golang.org/x/crypto/ssh"
)

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func exporter() []byte {
	b := make([]byte, 32)
	rand.Read(b)
	return b
}

func TestSignVerify(t *testing.T) {
	s := newSigner(t)
	exp := exporter()

	sig, err := Sign(s, exp)
	if err != nil {
		t.Fatal(err)
	}
	pub := s.PublicKey().Marshal()
	if _, err := Verify(pub, sig, exp); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if _, err := Verify(pub, sig, exporter()); err == nil {
		t.Fatal("signature accepted on a different TLS session")
	}
	if _, err := Verify(newSigner(t).PublicKey().Marshal(), sig, exp); err == nil {
		t.Fatal("signature accepted for a different key")
	}
	if _, err := Verify(pub, []byte("garbage"), exp); err == nil {
		t.Fatal("garbage signature accepted")
	}
}

func TestRSAUsesSHA2AndRejectsSHA1(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	exp := exporter()
	sig, err := Sign(s, exp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(s.PublicKey().Marshal(), sig, exp); err != nil {
		t.Fatalf("rsa-sha2 signature rejected: %v", err)
	}

	sha1Sig, err := s.(ssh.AlgorithmSigner).SignWithAlgorithm(rand.Reader, AuthPayload(exp), ssh.KeyAlgoRSA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(s.PublicKey().Marshal(), ssh.Marshal(sha1Sig), exp); err == nil {
		t.Fatal("ssh-rsa SHA-1 signature accepted")
	}
}

func TestFingerprintStable(t *testing.T) {
	s := newSigner(t)
	if Fingerprint(s.PublicKey()) != ssh.FingerprintSHA256(s.PublicKey()) {
		t.Fatalf("fingerprint differs from ssh-keygen format")
	}
}
