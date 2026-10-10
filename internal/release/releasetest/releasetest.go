// Package releasetest serves a fake GitHub Releases layout (see package
// release) for tests of tc update and install.sh.
package releasetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Server is a fake release host. Base is what TC_RELEASE_BASE should be.
type Server struct {
	*httptest.Server
	Base string

	mu     sync.Mutex
	latest string
	files  map[string][]byte // "<tag>/<name>"
	hits   []string
}

// New starts a server whose latest release is tag, with no assets yet.
func New(t testing.TB, tag string) *Server {
	s := &Server{latest: tag, files: map[string][]byte{}}
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
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	s.Server = httptest.NewServer(mux)
	s.Base = s.URL + "/releases"
	t.Cleanup(s.Close)
	return s
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

// Publish adds a release: one tc_<os>_<arch>.tar.gz per bins entry (key
// "os_arch", value the tc binary) and a matching checksums.txt.
func (s *Server) Publish(tag string, bins map[string][]byte) {
	var sums strings.Builder
	for osArch, bin := range bins {
		name := "tc_" + osArch + ".tar.gz"
		arc := Tarball(bin)
		s.Put(tag, name, arc)
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(arc), name)
	}
	s.Put(tag, "checksums.txt", []byte(sums.String()))
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
