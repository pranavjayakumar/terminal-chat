// Package releasetest serves a fake GitHub Releases layout (see package
// release) for tests of tc update and install.sh.
package releasetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/release"
)

// Server is a fake release host. Base is what TC_RELEASE_BASE should be;
// Signers is the allowed_signers text for its release key (Key), which
// signs every checksums.txt it publishes.
type Server struct {
	*httptest.Server
	Base    string
	Signers string
	Key     ssh.Signer

	mu        sync.Mutex
	latest    string
	files     map[string][]byte // "<tag>/<name>"
	redirects map[string]string // "<tag>/<name>" → absolute URL
	hits      []string
}

// New starts a plain-http (loopback) server whose latest release is tag,
// with no assets yet.
func New(t testing.TB, tag string) *Server { return start(t, tag, false) }

// NewTLS is New over https with httptest's self-signed certificate: use
// s.Source() (it trusts it) or CertFile for curl.
func NewTLS(t testing.TB, tag string) *Server { return start(t, tag, true) }

func start(t testing.TB, tag string, tls bool) *Server {
	s := &Server{latest: tag, files: map[string][]byte{}, redirects: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		s.hit(r.URL.Path)
		s.mu.Lock()
		tag := s.latest
		s.mu.Unlock()
		if tag == "" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/releases/tag/", func(w http.ResponseWriter, r *http.Request) {
		s.hit(r.URL.Path)
		fmt.Fprintln(w, "release page")
	})
	mux.HandleFunc("/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		s.hit(r.URL.Path)
		key := strings.TrimPrefix(r.URL.Path, "/releases/download/")
		s.mu.Lock()
		b, ok := s.files[key]
		to, redirect := s.redirects[key]
		s.mu.Unlock()
		if redirect {
			http.Redirect(w, r, to, http.StatusFound)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	if tls {
		s.Server = httptest.NewTLSServer(mux)
	} else {
		s.Server = httptest.NewServer(mux)
	}
	s.Base = s.URL + "/releases"
	t.Cleanup(s.Close)
	s.Key = NewKey(t)
	s.Signers = AllowedSigners(s.Key.PublicKey())
	return s
}

// NewKey is a fresh ed25519 release key.
func NewKey(t testing.TB) ssh.Signer {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// AllowedSigners is the allowed_signers line trusting pub for releases.
func AllowedSigners(pub ssh.PublicKey) string {
	return release.SigPrincipal + ` namespaces="` + release.SigNamespace + `" ` +
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " test-release-key"
}

// Sign makes an armored SSHSIG signature of msg, like
// ssh-keygen -Y sign -n namespace (sha512).
func Sign(t testing.TB, key ssh.Signer, namespace string, msg []byte) []byte {
	h := sha512.Sum512(msg)
	sig, err := key.Sign(rand.Reader, release.SignedData(namespace, "", "sha512", h[:]))
	if err != nil {
		t.Fatal(err)
	}
	var blob bytes.Buffer
	blob.WriteString("SSHSIG")
	blob.Write([]byte{0, 0, 0, 1})
	for _, f := range [][]byte{key.PublicKey().Marshal(), []byte(namespace), nil, []byte("sha512"), ssh.Marshal(sig)} {
		blob.Write([]byte{byte(len(f) >> 24), byte(len(f) >> 16), byte(len(f) >> 8), byte(len(f))})
		blob.Write(f)
	}
	enc := base64.StdEncoding.EncodeToString(blob.Bytes())
	var out strings.Builder
	out.WriteString("-----BEGIN SSH SIGNATURE-----\n")
	for len(enc) > 70 {
		out.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	out.WriteString(enc + "\n-----END SSH SIGNATURE-----\n")
	return []byte(out.String())
}

func (s *Server) hit(p string) {
	s.mu.Lock()
	s.hits = append(s.hits, p)
	s.mu.Unlock()
}

// Hits returns the paths requested so far.
func (s *Server) Hits() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hits...)
}

// SetLatest changes what /releases/latest points at ("" → 404).
func (s *Server) SetLatest(tag string) {
	s.mu.Lock()
	s.latest = tag
	s.mu.Unlock()
}

// Put serves b as <tag>/<name>.
func (s *Server) Put(tag, name string, b []byte) {
	s.mu.Lock()
	s.files[tag+"/"+name] = b
	s.mu.Unlock()
}

// PutChecksums serves sums as tag's checksums.txt, signed with s.Key.
func (s *Server) PutChecksums(t testing.TB, tag string, sums []byte) {
	s.Put(tag, "checksums.txt", sums)
	s.Put(tag, release.SigFile, Sign(t, s.Key, release.SigNamespace, sums))
}

// Publish adds a release: one tc_<os>_<arch>.tar.gz per bins entry (key
// "os_arch", value the tc binary) and a matching, signed checksums.txt.
func (s *Server) Publish(t testing.TB, tag string, bins map[string][]byte) {
	var sums strings.Builder
	for osArch, bin := range bins {
		name := "tc_" + osArch + ".tar.gz"
		if strings.HasPrefix(osArch, "tc-voice_") { // the voice build's asset
			name = osArch + ".tar.gz"
		}
		arc := Tarball(bin)
		s.Put(tag, name, arc)
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(arc), name)
	}
	s.PutChecksums(t, tag, []byte(sums.String()))
}

// SymlinkTarball is a .tar.gz whose "tc" member is a symlink to target.
func SymlinkTarball(target string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "tc", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: target})
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// Tarball is a .tar.gz holding bin as an executable "tc" plus a README.
func Tarball(bin []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	readme := []byte("tc — terminal chat\n")
	tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: int64(len(readme)), Typeflag: tar.TypeReg})
	tw.Write(readme)
	tw.WriteHeader(&tar.Header{Name: "tc", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg})
	tw.Write(bin)
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// Source is a release.Source for this server that trusts its release key
// (and, for NewTLS, its certificate).
func (s *Server) Source() *release.Source {
	src := &release.Source{Base: s.Base, Signers: s.Signers}
	if s.TLS != nil {
		src.HTTP = s.Client()
	}
	return src
}

// Redirect makes <tag>/<name> redirect to url ("" stops redirecting).
func (s *Server) Redirect(tag, name, url string) {
	s.mu.Lock()
	if url == "" {
		delete(s.redirects, tag+"/"+name)
	} else {
		s.redirects[tag+"/"+name] = url
	}
	s.mu.Unlock()
}

// CertFile writes a NewTLS server's certificate as PEM (for CURL_CA_BUNDLE).
func (s *Server) CertFile(t testing.TB) string {
	f := filepath.Join(t.TempDir(), "ca.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// Remove stops serving <tag>/<name>.
func (s *Server) Remove(tag, name string) {
	s.mu.Lock()
	delete(s.files, tag+"/"+name)
	s.mu.Unlock()
}
