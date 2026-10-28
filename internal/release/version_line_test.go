package release_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/release"
	"github.com/skailama/tc/internal/release/releasetest"
)

func TestCheckVersionLine(t *testing.T) {
	h := strings.Repeat("a", 64) + "  tc_linux_amd64.tar.gz\n"
	for _, c := range []struct {
		sums, tag string
		ok        bool
	}{
		{"version v0.7.0\n" + h, "v0.7.0", true},
		{h + "version v0.7.0\n", "v0.7.0", true},
		{"version v0.7.0", "v0.7.0", true},
		// Releases built before the line existed.
		{h, "v0.6.1", true},
		{h, "v0.6.0", true},
		{h, "v0.1.0", true},
		// Anything newer must carry it: a rollback under a new tag.
		{h, "v0.6.2", false},
		{h, "v0.7.0", false},
		{h, "v1.0.0", false},
		{h, "v99.0.0", false},
		{h, "v0.6.1-rc1", false},
		{"version v0.4.0\n" + h, "v0.7.0", false},
		{"version v0.7.0\nversion v0.7.0\n" + h, "v0.7.0", false},
		{"version v0.7.0\nversion v0.8.0\n" + h, "v0.7.0", false},
		{"version v0.7.0\r\n" + h, "v0.7.0", false},
		{" version v0.7.0\n" + h, "v0.7.0", false},
		{"version  v0.7.0\n" + h, "v0.7.0", false},
		{"version v0.7.0 x\n" + h, "v0.7.0", false},
		{"version\n" + h, "v0.7.0", false},
		// A present line binds even an old tag.
		{"version v0.5.0\n" + h, "v0.4.0", false},
		{"version v0.4.0\n" + h, "v0.4.0", true},
	} {
		err := release.CheckVersionLine([]byte(c.sums), c.tag)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, release.ErrVersionLine)) {
			t.Errorf("%q for %s: err %v, want ok=%v", c.sums, c.tag, err, c.ok)
		}
	}
}

// Someone with release-write access republishes an old, genuinely signed
// checksums.txt (and its binaries) as a newer tag: refused.
func TestFetchRefusesRollback(t *testing.T) {
	srv := releasetest.New(t, "v0.9.0")
	old := releasetest.Tarball([]byte("OLD"))
	legacy := []byte(fmt.Sprintf("%x  tc_linux_amd64.tar.gz\n", sha256.Sum256(old)))
	ctx := context.Background()
	for _, c := range []struct {
		tag  string
		sums []byte
		ok   bool
	}{
		{"v0.5.0", legacy, true}, // a real legacy release still installs
		{"v0.9.0", legacy, false},
		{"v0.9.0", append([]byte("version v0.8.0\n"), legacy...), false},
		{"v0.9.0", append([]byte("version v0.9.0\n"), legacy...), true},
	} {
		srv.Put(c.tag, "tc_linux_amd64.tar.gz", old)
		srv.PutChecksums(t, c.tag, c.sums)
		bin, err := srv.Source().Fetch(ctx, c.tag, "linux", "amd64")
		if c.ok && (err != nil || string(bin) != "OLD") {
			t.Errorf("%s %q: %v", c.tag, c.sums, err)
		}
		if !c.ok && !errors.Is(err, release.ErrVersionLine) {
			t.Errorf("%s %q: err %v, want ErrVersionLine", c.tag, c.sums, err)
		}
	}
}

func TestInstallLegacyMatchesGo(t *testing.T) {
	b, _ := scriptSigners(t)
	m := regexp.MustCompile(`(?m)^LEGACY_UNVERSIONED=(\S+)$`).FindAllSubmatch(b, -1)
	if len(m) != 1 || string(m[0][1]) != release.LegacyUnversioned {
		t.Fatalf("install.sh LEGACY_UNVERSIONED %q, want %s", m, release.LegacyUnversioned)
	}
}

func TestInstallRefusesRollback(t *testing.T) {
	srv := releasetest.New(t, "v0.9.0")
	old := releasetest.Tarball(stubTC("linux_amd64@old"))
	legacy := []byte(fmt.Sprintf("%x  tc_linux_amd64.tar.gz\n", sha256.Sum256(old)))
	for _, c := range []struct {
		tag  string
		sums []byte
		want string // "" installs
	}{
		{"v0.6.1", legacy, ""},
		{"v0.5.0", legacy, ""},
		{"v0.6.2", legacy, `no "version v0.6.2" line`},
		{"v0.9.0", legacy, `no "version v0.9.0" line`},
		{"v0.6.1-rc1", legacy, `no "version v0.6.1-rc1" line`},
		{"v0.9.0", append([]byte("version v0.8.0\n"), legacy...), "version line doesn't match v0.9.0"},
		{"v0.9.0", append([]byte("version v0.9.0\nversion v0.9.0\n"), legacy...), "version line doesn't match"},
		{"v0.9.0", append([]byte("version v0.9.0\r\n"), legacy...), "version line doesn't match"},
		{"v0.9.0", append([]byte("version v0.9.0\n"), legacy...), ""},
	} {
		srv.SetLatest(c.tag)
		srv.Put(c.tag, "tc_linux_amd64.tar.gz", old)
		srv.PutChecksums(t, c.tag, c.sums)
		r := runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64"})
		_, statErr := os.Stat(filepath.Join(r.dir, "tc"))
		if c.want == "" {
			if r.err != nil || statErr != nil {
				t.Errorf("%s %q: %v\n%s", c.tag, c.sums, r.err, r.stderr)
			}
			continue
		}
		if r.err == nil || !strings.Contains(r.stderr, c.want) || !os.IsNotExist(statErr) {
			t.Errorf("%s %q: err %v, installed=%v\n%s", c.tag, c.sums, r.err, statErr == nil, r.stderr)
		}
	}
}
