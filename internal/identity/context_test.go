package identity

import (
	"crypto"
	"crypto/dsa" //lint:ignore SA1019 needed to prove DSA keys are rejected
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// skSigner is a software stand-in for a FIDO security key: it produces
// sk-ssh-ed25519 / sk-ecdsa-sha2-nistp256 signatures in the openssh
// PROTOCOL.u2f format, with configurable flags.
type skSigner struct {
	typ     string
	app     string
	ed      ed25519.PrivateKey
	ec      *ecdsa.PrivateKey
	flags   byte
	counter uint32
	pub     ssh.PublicKey
}

func newSKSigner(t *testing.T, typ string, flags byte) *skSigner {
	t.Helper()
	s := &skSigner{typ: typ, app: "ssh:", flags: flags, counter: 7}
	var wire []byte
	switch typ {
	case ssh.KeyAlgoSKED25519:
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		s.ed = priv
		wire = ssh.Marshal(struct {
			Name string
			Key  []byte
			App  string
		}{typ, pub, s.app})
	case ssh.KeyAlgoSKECDSA256:
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		s.ec = priv
		pt, err := priv.PublicKey.ECDH()
		if err != nil {
			t.Fatal(err)
		}
		wire = ssh.Marshal(struct {
			Name, Curve string
			Key         []byte
			App         string
		}{typ, "nistp256", pt.Bytes(), s.app})
	default:
		t.Fatalf("unknown sk type %s", typ)
	}
	pk, err := ssh.ParsePublicKey(wire)
	if err != nil {
		t.Fatalf("parsing hand-built %s key: %v", typ, err)
	}
	s.pub = pk
	return s
}

func (s *skSigner) PublicKey() ssh.PublicKey { return s.pub }

func (s *skSigner) Sign(r io.Reader, data []byte) (*ssh.Signature, error) {
	appH := sha256.Sum256([]byte(s.app))
	dataH := sha256.Sum256(data)
	var fc [5]byte
	fc[0] = s.flags
	binary.BigEndian.PutUint32(fc[1:], s.counter)
	signed := append(append(appH[:], fc[:]...), dataH[:]...)

	sig := &ssh.Signature{Format: s.typ, Rest: fc[:]}
	if s.ed != nil {
		sig.Blob = ed25519.Sign(s.ed, signed)
		return sig, nil
	}
	d := sha256.Sum256(signed)
	R, S, err := ecdsa.Sign(r, s.ec, d[:])
	if err != nil {
		return nil, err
	}
	sig.Blob = ssh.Marshal(struct{ R, S *big.Int }{R, S})
	return sig, nil
}

func signerFromKey(t *testing.T, k crypto.PrivateKey) ssh.Signer {
	t.Helper()
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// agentSigner adds k to an in-memory keyring and returns the signer an agent
// client sees for it, so signing goes through the real agent protocol.
func agentSigner(t *testing.T, k crypto.PrivateKey) ssh.Signer {
	t.Helper()
	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{PrivateKey: k}); err != nil {
		t.Fatal(err)
	}
	c, srv := net.Pipe()
	t.Cleanup(func() { c.Close(); srv.Close() })
	go agent.ServeAgent(kr, srv)
	signers, err := agent.NewClient(c).Signers()
	if err != nil || len(signers) != 1 {
		t.Fatalf("agent signers: %v (%d)", err, len(signers))
	}
	return signers[0]
}

type namedSigner struct {
	name string
	s    ssh.Signer
}

func allSigners(t *testing.T) []namedSigner {
	t.Helper()
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	ec := func(c elliptic.Curve) *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(c, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return []namedSigner{
		{"ed25519", signerFromKey(t, edPriv)},
		{"ecdsa-p256", signerFromKey(t, ec(elliptic.P256()))},
		{"ecdsa-p384", signerFromKey(t, ec(elliptic.P384()))},
		{"ecdsa-p521", signerFromKey(t, ec(elliptic.P521()))},
		{"rsa-2048", signerFromKey(t, rsaKey)},
		{"agent-ed25519", agentSigner(t, edPriv)},
		{"agent-ecdsa-p256", agentSigner(t, ec(elliptic.P256()))},
		{"agent-rsa-2048", agentSigner(t, rsaKey)},
		{"sk-ed25519", newSKSigner(t, ssh.KeyAlgoSKED25519, skFlagUserPresence)},
		{"sk-ecdsa-p256", newSKSigner(t, ssh.KeyAlgoSKECDSA256, skFlagUserPresence|0x04)},
	}
}

func TestSignVerifyContextAllKeyTypes(t *testing.T) {
	b := []byte("payload bytes")
	for _, ns := range allSigners(t) {
		t.Run(ns.name, func(t *testing.T) {
			pub := ns.s.PublicKey().Marshal()
			sig, err := SignContext(ns.s, CtxDevice, b)
			if err != nil {
				t.Fatal(err)
			}
			pk, err := VerifyContext(pub, sig, CtxDevice, b)
			if err != nil {
				t.Fatalf("valid signature rejected: %v", err)
			}
			if string(pk.Marshal()) != string(pub) {
				t.Fatal("returned key differs")
			}
			if _, err := VerifyContext(pub, sig, CtxEndorse, b); err == nil {
				t.Fatal("accepted under another context")
			}
			if _, err := VerifyContext(pub, sig, CtxDevice, []byte("payload bytez")); err == nil {
				t.Fatal("accepted tampered b")
			}
			if _, err := VerifyContext(newSigner(t).PublicKey().Marshal(), sig, CtxDevice, b); err == nil {
				t.Fatal("accepted for another key")
			}

			// The auth handshake shares the same rules and code.
			exp := exporter()
			asig, err := Sign(ns.s, exp)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Verify(pub, asig, exp); err != nil {
				t.Fatalf("auth signature rejected: %v", err)
			}
		})
	}
}

func TestRSAContextAlgorithms(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []ssh.Signer{signerFromKey(t, priv), agentSigner(t, priv)} {
		pub := s.PublicKey().Marshal()
		b := []byte("rsa")
		sig, err := SignContext(s, CtxDevice, b)
		if err != nil {
			t.Fatal(err)
		}
		var parsed ssh.Signature
		if err := ssh.Unmarshal(sig, &parsed); err != nil || parsed.Format != ssh.KeyAlgoRSASHA256 {
			t.Fatalf("SignContext used %q, want rsa-sha2-256 (%v)", parsed.Format, err)
		}

		as := s.(ssh.AlgorithmSigner)
		msg := withContext(CtxDevice, b)
		s512, err := as.SignWithAlgorithm(rand.Reader, msg, ssh.KeyAlgoRSASHA512)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyContext(pub, ssh.Marshal(s512), CtxDevice, b); err != nil {
			t.Fatalf("rsa-sha2-512 rejected: %v", err)
		}
		s1, err := as.SignWithAlgorithm(rand.Reader, msg, ssh.KeyAlgoRSA)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyContext(pub, ssh.Marshal(s1), CtxDevice, b); err == nil {
			t.Fatal("ssh-rsa (SHA-1) accepted")
		}
	}
}

// plainSigner hides AlgorithmSigner, so an RSA key could only sign with SHA-1.
type plainSigner struct{ ssh.Signer }

func TestRSAWithoutAlgorithmSignerRefused(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SignContext(plainSigner{signerFromKey(t, priv)}, CtxDevice, nil); err == nil {
		t.Fatal("signed with an RSA signer that can only do SHA-1")
	}
}

func TestDSARejected(t *testing.T) {
	var priv dsa.PrivateKey
	if err := dsa.GenerateParameters(&priv.Parameters, rand.Reader, dsa.L1024N160); err != nil {
		t.Fatal(err)
	}
	if err := dsa.GenerateKey(&priv, rand.Reader); err != nil {
		t.Fatal(err)
	}
	s := signerFromKey(t, &priv)
	if _, err := SignContext(s, CtxDevice, nil); err == nil {
		t.Fatal("SignContext accepted a DSA key")
	}
	raw, err := s.Sign(rand.Reader, withContext(CtxDevice, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyContext(s.PublicKey().Marshal(), ssh.Marshal(raw), CtxDevice, nil); err == nil {
		t.Fatal("VerifyContext accepted a DSA signature")
	}
}

func TestSKUserPresence(t *testing.T) {
	for _, typ := range []string{ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256} {
		t.Run(typ, func(t *testing.T) {
			s := newSKSigner(t, typ, skFlagUserPresence)
			pub := s.PublicKey().Marshal()
			b := []byte("touch me")
			sig, err := SignContext(s, CtxDevice, b)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyContext(pub, sig, CtxDevice, b); err != nil {
				t.Fatalf("UP signature rejected: %v", err)
			}

			s.flags = 0x04 // user verified, but not present
			sig, err = SignContext(s, CtxDevice, b)
			if err != nil {
				t.Fatal(err)
			}
			_, err = VerifyContext(pub, sig, CtxDevice, b)
			if err == nil || !strings.Contains(err.Error(), "user presence") {
				t.Fatalf("signature without UP: err = %v, want user-presence rejection", err)
			}
			exp := exporter()
			asig, _ := Sign(s, exp)
			if _, err := Verify(pub, asig, exp); err == nil {
				t.Fatal("auth accepted a signature without UP")
			}

			// Flags/counter must be exactly 5 bytes.
			s.flags = skFlagUserPresence
			raw, _ := s.Sign(rand.Reader, withContext(CtxDevice, b))
			raw.Rest = append(raw.Rest, 0)
			if _, err := VerifyContext(pub, ssh.Marshal(raw), CtxDevice, b); err == nil {
				t.Fatal("accepted sk signature with trailing bytes")
			}
		})
	}
}

func TestTrailingSignatureDataRejected(t *testing.T) {
	s := newSigner(t)
	raw, err := s.Sign(rand.Reader, withContext(CtxDevice, nil))
	if err != nil {
		t.Fatal(err)
	}
	raw.Rest = []byte{1}
	if _, err := VerifyContext(s.PublicKey().Marshal(), ssh.Marshal(raw), CtxDevice, nil); err == nil {
		t.Fatal("accepted ed25519 signature with trailing data")
	}
}

func TestContextValidation(t *testing.T) {
	s := newSigner(t)
	for _, c := range []string{"", "\x00", "tc-device-v1", "tc\x00device\x00", "tc-device-v1\x00x"} {
		if _, err := SignContext(s, c, nil); err == nil {
			t.Errorf("SignContext accepted context %q", c)
		}
		if _, err := VerifyContext(s.PublicKey().Marshal(), nil, c, nil); err == nil {
			t.Errorf("VerifyContext accepted context %q", c)
		}
	}
}

func TestAuthSignatureNotAContextSignature(t *testing.T) {
	s := newSigner(t)
	pub := s.PublicKey().Marshal()
	exp := exporter()
	asig, err := Sign(s, exp)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{CtxDevice, CtxEndorse, CtxRevoke} {
		if _, err := VerifyContext(pub, asig, c, exp); err == nil {
			t.Fatalf("auth signature verified under %q", c)
		}
	}
	csig, _ := SignContext(s, CtxDevice, exp)
	if _, err := Verify(pub, csig, exp); err == nil {
		t.Fatal("device-context signature verified as auth")
	}
}
