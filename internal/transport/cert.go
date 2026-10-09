package transport

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// ErrPinMismatch is returned (wrapped) when the server certificate's SPKI pin
// does not equal the expected pin.
var ErrPinMismatch = errors.New("server certificate pin mismatch")

// LoadOrCreateCert loads server.key/server.crt from dir, generating a
// self-signed ed25519 certificate on first use. The key never rotates
// (PROTOCOL.md §2), so a half-present pair is an error rather than a reason
// to regenerate.
func LoadOrCreateCert(dir string) (tls.Certificate, error) {
	return loadOrCreate(dir, "server", func() (any, crypto.Signer, error) { return ed25519.GenerateKey(rand.Reader) })
}

// LoadOrCreateHTTPCert is LoadOrCreateCert for https.key/https.crt, an ECDSA
// P-256 certificate served to webhook (HTTP) clients: common TLS stacks, e.g.
// LibreSSL in macOS curl, can't verify Ed25519 (PROTOCOL.md §12).
func LoadOrCreateHTTPCert(dir string) (tls.Certificate, error) {
	return loadOrCreate(dir, "https", func() (any, crypto.Signer, error) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		return k.Public(), k, nil
	})
}

func loadOrCreate(dir, base string, gen func() (any, crypto.Signer, error)) (tls.Certificate, error) {
	keyPath, crtPath := filepath.Join(dir, base+".key"), filepath.Join(dir, base+".crt")
	_, kerr := os.Stat(keyPath)
	_, cerr := os.Stat(crtPath)
	switch {
	case kerr == nil && cerr == nil:
		cert, err := tls.LoadX509KeyPair(crtPath, keyPath)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("loading server cert: %w", err)
		}
		return cert, nil
	case errors.Is(kerr, os.ErrNotExist) && errors.Is(cerr, os.ErrNotExist):
	case kerr != nil && !errors.Is(kerr, os.ErrNotExist):
		return tls.Certificate{}, fmt.Errorf("stat server key: %w", kerr)
	case cerr != nil && !errors.Is(cerr, os.ErrNotExist):
		return tls.Certificate{}, fmt.Errorf("stat server cert: %w", cerr)
	default:
		return tls.Certificate{}, fmt.Errorf("only one of %s and %s exists; refusing to regenerate", keyPath, crtPath)
	}

	keyPEM, crtPEM, err := generateCert(gen)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("creating cert dir: %w", err)
	}
	if err := writeExclusive(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeExclusive(crtPath, crtPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	cert, err := tls.X509KeyPair(crtPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parsing generated cert: %w", err)
	}
	return cert, nil
}

func generateCert(gen func() (any, crypto.Signer, error)) (keyPEM, crtPEM []byte, err error) {
	pub, priv, err := gen()
	if err != nil {
		return nil, nil, fmt.Errorf("generating key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, fmt.Errorf("generating serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "tc server"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(100, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("creating cert: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// Pin returns the base64url (unpadded) SHA-256 of the leaf certificate's
// SubjectPublicKeyInfo, or "" if the certificate can't be parsed.
func Pin(cert tls.Certificate) string {
	leaf := cert.Leaf
	if leaf == nil {
		if len(cert.Certificate) == 0 {
			return ""
		}
		var err error
		if leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return ""
		}
	}
	return spkiPin(leaf)
}

func spkiPin(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func verifyPin(pin string) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) != 1 {
			return fmt.Errorf("%w: expected exactly one certificate, got %d", ErrPinMismatch, len(cs.PeerCertificates))
		}
		got := spkiPin(cs.PeerCertificates[0])
		if subtle.ConstantTimeCompare([]byte(got), []byte(pin)) != 1 {
			return fmt.Errorf("%w: got %s, want %s", ErrPinMismatch, got, pin)
		}
		return nil
	}
}
