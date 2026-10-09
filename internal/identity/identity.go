// Package identity provides the user's SSH key as a tc identity: loading a
// signer (ssh-agent first, then key files) and building/verifying the auth
// signature defined in PROTOCOL.md §4.
package identity

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const authContext = "tc-auth-v1\x00"

// Accepted key and signature algorithms (PROTOCOL.md §4). DSA and
// ssh-rsa/SHA-1 signatures are rejected.
var (
	allowedKey = map[string]bool{
		ssh.KeyAlgoED25519: true, ssh.KeyAlgoSKED25519: true, ssh.KeyAlgoRSA: true,
		ssh.KeyAlgoECDSA256: true, ssh.KeyAlgoECDSA384: true, ssh.KeyAlgoECDSA521: true, ssh.KeyAlgoSKECDSA256: true,
	}
	allowedSig = map[string]bool{
		ssh.KeyAlgoED25519: true, ssh.KeyAlgoSKED25519: true, ssh.KeyAlgoRSASHA256: true, ssh.KeyAlgoRSASHA512: true,
		ssh.KeyAlgoECDSA256: true, ssh.KeyAlgoECDSA384: true, ssh.KeyAlgoECDSA521: true, ssh.KeyAlgoSKECDSA256: true,
	}
)

// ErrNoKey means no usable SSH key was found in the agent or on disk.
var ErrNoKey = errors.New("no SSH key found (tried ssh-agent and ~/.ssh/id_ed25519, id_ecdsa, id_rsa)")

// AuthPayload is the exact byte string a client signs during the handshake.
// The TLS exporter makes it unique to one TLS session.
func AuthPayload(exporter []byte) []byte {
	return append([]byte(authContext), exporter...)
}

// Sign produces the wire signature (ssh.Signature, ssh wire-encoded) over the
// auth payload. RSA keys sign with SHA-256; SHA-1 is never used.
func Sign(s ssh.Signer, exporter []byte) ([]byte, error) {
	var sig *ssh.Signature
	var err error
	if as, ok := s.(ssh.AlgorithmSigner); ok && s.PublicKey().Type() == ssh.KeyAlgoRSA {
		sig, err = as.SignWithAlgorithm(rand.Reader, AuthPayload(exporter), ssh.KeyAlgoRSASHA256)
	} else {
		sig, err = s.Sign(rand.Reader, AuthPayload(exporter))
	}
	if err != nil {
		return nil, fmt.Errorf("signing auth payload: %w", err)
	}
	return ssh.Marshal(sig), nil
}

// Verify checks a wire signature from Sign against a wire-encoded public key.
func Verify(pubkey, sig, exporter []byte) (ssh.PublicKey, error) {
	pk, err := ssh.ParsePublicKey(pubkey)
	if err != nil {
		return nil, fmt.Errorf("parsing public key: %w", err)
	}
	var s ssh.Signature
	if err := ssh.Unmarshal(sig, &s); err != nil {
		return nil, fmt.Errorf("parsing signature: %w", err)
	}
	if !allowedSig[s.Format] || !allowedKey[pk.Type()] {
		return nil, fmt.Errorf("key type %s / signature %s not accepted", pk.Type(), s.Format)
	}
	if err := pk.Verify(AuthPayload(exporter), &s); err != nil {
		return nil, fmt.Errorf("verifying signature: %w", err)
	}
	return pk, nil
}

// Fingerprint is the stable identifier used to look up a user by key.
func Fingerprint(pk ssh.PublicKey) string {
	sum := sha256.Sum256(pk.Marshal())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// Load returns a signer for the user's key. If keyPath is set it is used
// (via the agent if the agent holds it, otherwise read from disk). Otherwise
// the first ed25519 key in the agent wins, then any agent key, then default files.
func Load(keyPath string) (ssh.Signer, error) {
	agentSigners := fromAgent()

	if keyPath != "" {
		pub, _ := readPublic(keyPath + ".pub")
		if pub != nil {
			for _, s := range agentSigners {
				if bytes.Equal(s.PublicKey().Marshal(), pub.Marshal()) {
					return s, nil
				}
			}
		}
		return readPrivate(keyPath)
	}

	for _, s := range agentSigners {
		if s.PublicKey().Type() == ssh.KeyAlgoED25519 {
			return s, nil
		}
	}
	if len(agentSigners) > 0 {
		return agentSigners[0], nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, ErrNoKey
	}
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		s, err := readPrivate(filepath.Join(home, ".ssh", name))
		if err == nil {
			return s, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, ErrNoKey
}

func fromAgent() []ssh.Signer {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil
	}
	// The connection must outlive this call: agent signers use it on Sign.
	signers, err := agent.NewClient(conn).Signers()
	if err != nil || len(signers) == 0 {
		conn.Close()
		return nil
	}
	return signers
}

func readPublic(path string) (ssh.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey(b)
	return pk, err
}

func readPrivate(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := ssh.ParsePrivateKey(b)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		return nil, fmt.Errorf("%s is passphrase-protected: add it to ssh-agent (ssh-add %s)", path, path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return s, nil
}
