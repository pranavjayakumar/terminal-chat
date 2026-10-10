package release

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"path"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Release signatures are OpenSSH SSHSIG signatures (PROTOCOL.sshsig) of
// checksums.txt, made with `ssh-keygen -Y sign -n tc-release` and checked
// here and in install.sh against AllowedSigners.
const (
	SigNamespace = "tc-release" // -n
	SigPrincipal = "tc-release" // -I: the principal AllowedSigners lists
	SigFile      = "checksums.txt.sig"
)

// ErrSignature means checksums.txt isn't signed by an allowed release key.
var ErrSignature = errors.New("release signature check failed")

// ErrNoSigningKey means this build has no release key embedded.
var ErrNoSigningKey = errors.New("this tc build has no release signing key embedded, so it can't verify updates; reinstall tc from an official release")

const (
	sigMagic   = "SSHSIG"
	sigArmorBg = "-----BEGIN SSH SIGNATURE-----"
	sigArmorEn = "-----END SSH SIGNATURE-----"
)

// signer is one usable allowed_signers line.
type signer struct {
	key ssh.PublicKey
}

// parseAllowedSigners keeps the lines whose principals match SigPrincipal
// and whose namespaces (if restricted) include SigNamespace. Lines with
// options we don't implement (cert-authority, validity windows) are
// dropped rather than trusted more widely than ssh-keygen would.
func parseAllowedSigners(text string) ([]signer, error) {
	if strings.Contains(text, Placeholder) {
		return nil, ErrNoSigningKey
	}
	var out []signer
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 || !principalMatch(f[0], SigPrincipal) {
			continue
		}
		var key ssh.PublicKey
		j := 1
		for ; j < len(f)-1; j++ {
			raw, err := base64.StdEncoding.DecodeString(f[j+1])
			if err != nil {
				continue
			}
			if k, err := ssh.ParsePublicKey(raw); err == nil && k.Type() == f[j] {
				key = k
				break
			}
		}
		if key == nil || !optionsAllow(strings.Join(f[1:j], " "), SigNamespace) {
			continue
		}
		out = append(out, signer{key: key})
	}
	if len(out) == 0 {
		return nil, errors.New("no usable release signing key in allowed signers")
	}
	return out, nil
}

func principalMatch(list, principal string) bool {
	for _, p := range strings.Split(list, ",") {
		if ok, err := path.Match(p, principal); err == nil && ok {
			return true
		}
	}
	return false
}

// optionsAllow parses the comma-separated options field ("" for none).
func optionsAllow(opts, namespace string) bool {
	if opts == "" {
		return true
	}
	for _, o := range splitOptions(opts) {
		name, val, _ := strings.Cut(o, "=")
		switch strings.ToLower(name) {
		case "namespaces":
			ok := false
			for _, n := range strings.Split(strings.Trim(val, `"`), ",") {
				if m, err := path.Match(n, namespace); err == nil && m {
					ok = true
				}
			}
			if !ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func splitOptions(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case r == ',' && !quoted:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(out, cur.String())
}

// VerifySignature checks that armored is an SSHSIG signature of message in
// namespace SigNamespace by a key in allowedSigners.
func VerifySignature(allowedSigners string, message, armored []byte) error {
	signers, err := parseAllowedSigners(allowedSigners)
	if err != nil {
		return err
	}
	if err := verifySSHSIG(signers, SigNamespace, message, armored); err != nil {
		return fmt.Errorf("%w: %v", ErrSignature, err)
	}
	return nil
}

func verifySSHSIG(signers []signer, namespace string, message, armored []byte) error {
	blob, err := dearmor(armored)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(blob, []byte(sigMagic)) {
		return errors.New("not an SSH signature")
	}
	r := &sshParser{b: blob[len(sigMagic):]}
	version := r.uint32()
	pubBlob := r.string()
	ns := r.string()
	reserved := r.string()
	hashAlg := r.string()
	sigBlob := r.string()
	if r.err != nil || len(r.b) != 0 {
		return errors.New("malformed SSH signature")
	}
	if version != 1 {
		return fmt.Errorf("unsupported SSH signature version %d", version)
	}
	if string(ns) != namespace {
		return fmt.Errorf("signature is for namespace %q, not %q", ns, namespace)
	}
	var digest []byte
	switch string(hashAlg) {
	case "sha512":
		h := sha512.Sum512(message)
		digest = h[:]
	case "sha256":
		h := sha256.Sum256(message)
		digest = h[:]
	default:
		return fmt.Errorf("unsupported signature hash %q", hashAlg)
	}
	pub, err := ssh.ParsePublicKey(pubBlob)
	if err != nil {
		return fmt.Errorf("signature key: %w", err)
	}
	if _, isCert := pub.(*ssh.Certificate); isCert {
		return errors.New("certificate signatures aren't supported")
	}
	trusted := false
	for _, s := range signers {
		if bytes.Equal(s.key.Marshal(), pub.Marshal()) {
			trusted = true
			break
		}
	}
	if !trusted {
		return fmt.Errorf("signed by an unknown key %s", ssh.FingerprintSHA256(pub))
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(sigBlob, &sig); err != nil {
		return errors.New("malformed signature")
	}
	// PROTOCOL.sshsig: RSA signatures must not use SHA-1.
	if sig.Format == ssh.KeyAlgoRSA {
		return errors.New("ssh-rsa (SHA-1) signatures aren't accepted")
	}
	if err := pub.Verify(SignedData(namespace, string(reserved), string(hashAlg), digest), &sig); err != nil {
		return errors.New("signature doesn't match")
	}
	return nil
}

// SignedData is the blob an SSHSIG signature covers (PROTOCOL.sshsig).
func SignedData(namespace, reserved, hashAlg string, digest []byte) []byte {
	var b bytes.Buffer
	b.WriteString(sigMagic)
	for _, s := range [][]byte{[]byte(namespace), []byte(reserved), []byte(hashAlg), digest} {
		writeSSHString(&b, s)
	}
	return b.Bytes()
}

func writeSSHString(b *bytes.Buffer, s []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	b.Write(n[:])
	b.Write(s)
}

func dearmor(armored []byte) ([]byte, error) {
	s := strings.TrimSpace(string(armored))
	body, ok := strings.CutPrefix(s, sigArmorBg)
	if !ok {
		return nil, errors.New("not an armored SSH signature")
	}
	body, ok = strings.CutSuffix(body, sigArmorEn)
	if !ok {
		return nil, errors.New("not an armored SSH signature")
	}
	b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(body), ""))
	if err != nil {
		return nil, errors.New("bad SSH signature encoding")
	}
	return b, nil
}

type sshParser struct {
	b   []byte
	err error
}

func (p *sshParser) uint32() uint32 {
	if p.err != nil || len(p.b) < 4 {
		p.err = errors.New("short")
		return 0
	}
	v := binary.BigEndian.Uint32(p.b)
	p.b = p.b[4:]
	return v
}

func (p *sshParser) string() []byte {
	n := p.uint32()
	if p.err != nil || uint64(n) > uint64(len(p.b)) {
		p.err = errors.New("short")
		return nil
	}
	s := p.b[:n]
	p.b = p.b[n:]
	return s
}
