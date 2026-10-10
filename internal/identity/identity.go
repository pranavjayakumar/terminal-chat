// Package identity provides the user's SSH key as a tc identity: loading a
// signer (ssh-agent first, then key files) and building/verifying the auth
// signature defined in PROTOCOL.md §4, plus the SSH-signed E2E identity
// objects (device certs, key endorsements, device revokes; E2E.md §13.2).
package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const authContext = "tc-auth-v1\x00"

// SSH signing contexts for E2E identity objects (E2E.md §13.2), used with
// SignContext/VerifyContext.
const (
	CtxDevice  = "tc-device-v1\x00"
	CtxEndorse = "tc-endorse-v1\x00"
	CtxRevoke  = "tc-revoke-v1\x00"
)

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
	sig, err := signRaw(s, AuthPayload(exporter))
	if err != nil {
		return nil, fmt.Errorf("signing auth payload: %w", err)
	}
	return sig, nil
}

// Verify checks a wire signature from Sign against a wire-encoded public key.
func Verify(pubkey, sig, exporter []byte) (ssh.PublicKey, error) {
	return verifyRaw(pubkey, sig, AuthPayload(exporter))
}

// SignContext signs context || b with the §4 algorithm rules. context must be
// a domain-separation tag ending in its only NUL byte (e.g. "tc-device-v1\x00"),
// which keeps all contexts prefix-free so no signature verifies under two of them.
func SignContext(s ssh.Signer, context string, b []byte) ([]byte, error) {
	if err := checkContext(context); err != nil {
		return nil, err
	}
	sig, err := signRaw(s, withContext(context, b))
	if err != nil {
		return nil, fmt.Errorf("signing %q: %w", strings.TrimSuffix(context, "\x00"), err)
	}
	return sig, nil
}

// VerifyContext checks a SignContext signature by the wire-encoded pubkey over
// context || b.
func VerifyContext(pubkey, sig []byte, context string, b []byte) (ssh.PublicKey, error) {
	if err := checkContext(context); err != nil {
		return nil, err
	}
	return verifyRaw(pubkey, sig, withContext(context, b))
}

func checkContext(context string) error {
	if len(context) < 2 || strings.IndexByte(context, 0) != len(context)-1 {
		return fmt.Errorf("invalid signature context %q", context)
	}
	return nil
}

func withContext(context string, b []byte) []byte {
	m := make([]byte, 0, len(context)+len(b))
	return append(append(m, context...), b...)
}

func signRaw(s ssh.Signer, msg []byte) ([]byte, error) {
	typ := s.PublicKey().Type()
	if !allowedKey[typ] {
		return nil, fmt.Errorf("key type %s not accepted", typ)
	}
	var sig *ssh.Signature
	var err error
	if typ == ssh.KeyAlgoRSA {
		// A plain Signer would produce ssh-rsa (SHA-1), which Verify rejects.
		as, ok := s.(ssh.AlgorithmSigner)
		if !ok {
			return nil, errors.New("RSA signer cannot sign with rsa-sha2-256")
		}
		sig, err = as.SignWithAlgorithm(rand.Reader, msg, ssh.KeyAlgoRSASHA256)
	} else {
		sig, err = s.Sign(rand.Reader, msg)
	}
	if err != nil {
		return nil, err
	}
	return ssh.Marshal(sig), nil
}

// skFlagUserPresence is the FIDO "user present" bit in an sk-* signature's flags.
const skFlagUserPresence = 0x01

func verifyRaw(pubkey, sig, msg []byte) (ssh.PublicKey, error) {
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
	// sk-* signatures carry flags(1) || counter(u32) after the blob
	// (openssh PROTOCOL.u2f); other formats carry nothing. Checked here rather
	// than relying on x/crypto's default so a touch-less signature is refused
	// regardless of library version.
	switch pk.Type() {
	case ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256:
		if len(s.Rest) != 5 {
			return nil, errors.New("malformed security-key signature")
		}
		if s.Rest[0]&skFlagUserPresence == 0 {
			return nil, errors.New("security-key signature without user presence")
		}
	default:
		if len(s.Rest) != 0 {
			return nil, errors.New("trailing data in signature")
		}
	}
	if err := pk.Verify(msg, &s); err != nil {
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

// AgentED25519 returns the first ed25519 key in ssh-agent ($SSH_AUTH_SOCK),
// the one Load("") picks.
func AgentED25519() (ssh.PublicKey, bool) {
	for _, s := range fromAgent() {
		if s.PublicKey().Type() == ssh.KeyAlgoED25519 {
			return s.PublicKey(), true
		}
	}
	return nil, false
}

// GenerateKey writes a new unencrypted ed25519 key to path (0600, OpenSSH
// format, comment "tc") and path.pub, creating the directory 0700 (and
// tightening it to 0700 if it exists). It never overwrites an existing
// file, and path appears only once fully written: the key goes to a temp
// file that is then hard-linked into place, so a crash can't leave an
// empty or partial key that later logins fail to parse.
func GenerateKey(path string) (ssh.Signer, error) { return generateKey(path, nil) }

// generateKey is GenerateKey; beforeLink (tests) runs once the temp file is
// written, where a crash would leave it behind.
func generateKey(path string, beforeLink func(tmp string)) (ssh.Signer, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := ssh.MarshalPrivateKey(priv, "tc")
	if err != nil {
		return nil, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating key directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("securing key directory: %w", err)
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("creating %s: %w", path, os.ErrExist)
	}
	if err := writeNew(path, pem.EncodeToMemory(blk), 0o600, beforeLink); err != nil {
		return nil, err
	}
	// The .pub is public and derived from the key we just created
	// exclusively, so a leftover one is replaced (atomically) to match.
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " tc\n"
	if err := writeReplace(path+".pub", []byte(line), 0o644); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

// writeReplace atomically sets path to b (mode perm).
func writeReplace(path string, b []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(b)
	cherr := tmp.Chmod(perm)
	cerr := tmp.Close()
	if err := errors.Join(werr, cherr, cerr); err != nil {
		os.Remove(name)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// writeNew creates path holding b (mode perm), or fails with os.ErrExist
// if something is already there, never exposing a partial file at path.
func writeNew(path string, b []byte, perm os.FileMode, beforeLink func(tmp string)) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	for _, step := range []func() error{
		func() error { return tmp.Chmod(perm) },
		func() error { _, err := tmp.Write(b); return err },
		tmp.Sync,
	} {
		if err := step(); err != nil {
			tmp.Close()
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if beforeLink != nil {
		beforeLink(name)
	}
	// link (unlike rename) fails if path exists: never overwrite a key.
	if err := os.Link(name, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("creating %s: %w", path, os.ErrExist)
		}
		return fmt.Errorf("creating %s: %w", path, err)
	}
	return nil
}
