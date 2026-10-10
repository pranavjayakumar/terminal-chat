package release_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/release"
	"github.com/skailama/tc/internal/release/releasetest"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.2.0", "v0.1.0", true},
		{"v0.1.1", "v0.1.0", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.10.0", "v0.9.0", true},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.2.0", false},
		{"v0.1.0", "v0.1.0-5-gabcdef-dirty", false},
		{"v0.2.0", "v0.1.0-5-gabcdef", true},
		{"v0.2.0", "dev", false},
		{"garbage", "v0.1.0", false},
		{"v01.2.3", "v0.1.0", false},
	} {
		if got := release.Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.latest, c.current, got)
		}
	}
}

func TestValidTag(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "v12.3.4", "v1.2.3-rc.1"} {
		if !release.ValidTag(tag) {
			t.Errorf("%q rejected", tag)
		}
	}
	for _, tag := range []string{"", "0.1.0", "v1.2", "v1.2.3/../x", "v1.2.3'", "v1.2.3 ", "latest"} {
		if release.ValidTag(tag) {
			t.Errorf("%q accepted", tag)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	h := strings.Repeat("ab", 32)
	m := release.ParseChecksums([]byte(h + "  tc_linux_amd64.tar.gz\n" + strings.ToUpper(h) + " *tc_darwin_arm64.tar.gz\nbad line\nzz  x\n"))
	if m["tc_linux_amd64.tar.gz"] != h || m["tc_darwin_arm64.tar.gz"] != h || len(m) != 2 {
		t.Fatalf("%v", m)
	}
}

func TestLatestAndFetch(t *testing.T) {
	srv := releasetest.New(t, "v1.2.3")
	srv.Publish("v1.2.3", map[string][]byte{"linux_amd64": []byte("LINUX"), "darwin_arm64": []byte("DARWIN")})
	src := &release.Source{Base: srv.Base}
	ctx := context.Background()
	tag, err := src.Latest(ctx)
	if err != nil || tag != "v1.2.3" {
		t.Fatalf("Latest: %q %v", tag, err)
	}
	bin, err := src.Fetch(ctx, tag, "darwin", "arm64")
	if err != nil || string(bin) != "DARWIN" {
		t.Fatalf("Fetch darwin: %q %v", bin, err)
	}
	bin, err = src.Fetch(ctx, tag, "linux", "amd64")
	if err != nil || string(bin) != "LINUX" {
		t.Fatalf("Fetch linux: %q %v", bin, err)
	}
	if _, err := src.Fetch(ctx, tag, "linux", "arm64"); err == nil {
		t.Fatal("missing asset fetched")
	}
	if _, err := src.Fetch(ctx, tag, "windows", "amd64"); err == nil {
		t.Fatal("unsupported OS fetched")
	}
	srv.SetLatest("")
	if _, err := src.Latest(ctx); err == nil {
		t.Fatal("no releases: no error")
	}
}

func TestLatestRejectsBadRedirect(t *testing.T) {
	srv := releasetest.New(t, "v1.2.3/../../evil")
	if tag, err := (&release.Source{Base: srv.Base}).Latest(context.Background()); err == nil {
		t.Fatalf("accepted %q", tag)
	}
}

func TestFetchChecksumMismatch(t *testing.T) {
	srv := releasetest.New(t, "v1.2.3")
	srv.Publish("v1.2.3", map[string][]byte{"linux_amd64": []byte("GOOD")})
	srv.Put("v1.2.3", "tc_linux_amd64.tar.gz", releasetest.Tarball([]byte("EVIL")))
	_, err := (&release.Source{Base: srv.Base}).Fetch(context.Background(), "v1.2.3", "linux", "amd64")
	if !errors.Is(err, release.ErrChecksum) {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
}

func TestReplace(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tc")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "tc-link")
	os.Symlink(exe, link)
	if err := release.Replace(link, []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(exe)
	st, _ := os.Stat(exe)
	if string(b) != "new" || st.Mode().Perm() != 0o755 {
		t.Fatalf("after replace: %q %v", b, st.Mode())
	}
	if l, err := os.Lstat(link); err != nil || l.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced instead of its target")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Fatalf("leftover temp files: %v", ents)
	}
}

func TestReplaceUnwritableDir(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "tc")
	os.WriteFile(exe, []byte("old"), 0o755)
	os.Chmod(dir, 0o555)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	err := release.Replace(exe, []byte("new"))
	if err == nil || !strings.Contains(err.Error(), "can't write") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatal("binary changed")
	}
}

func TestCheckCachedDaily(t *testing.T) {
	t.Setenv("TC_NO_UPDATE_CHECK", "")
	srv := releasetest.New(t, "v0.3.0")
	src := &release.Source{Base: srv.Base}
	home := t.TempDir()
	ctx := context.Background()
	now := time.Now()
	latestHits := func() int {
		n := 0
		for _, h := range srv.Hits() {
			if h == "/releases/latest" {
				n++
			}
		}
		return n
	}
	if got := release.Check(ctx, home, "v0.2.0", src, now); got != "v0.3.0" {
		t.Fatalf("first check: %q", got)
	}
	if got := release.Check(ctx, home, "v0.2.0", src, now.Add(time.Hour)); got != "v0.3.0" || latestHits() != 1 {
		t.Fatalf("cached check: %q, %d network checks", got, latestHits())
	}
	if got := release.Check(ctx, home, "v0.3.0", src, now.Add(2*time.Hour)); got != "" {
		t.Fatalf("up to date: %q", got)
	}
	srv.SetLatest("v0.4.0")
	if got := release.Check(ctx, home, "v0.3.0", src, now.Add(25*time.Hour)); got != "v0.4.0" || latestHits() != 2 {
		t.Fatalf("next day: %q, %d network checks", got, latestHits())
	}
	// Offline: the attempt is remembered, the old answer kept, nothing blocks.
	srv.Close()
	start := time.Now()
	if got := release.Check(ctx, home, "v0.3.0", src, now.Add(50*time.Hour)); got != "v0.4.0" {
		t.Fatalf("offline: %q", got)
	}
	if time.Since(start) > 6*time.Second {
		t.Fatal("offline check took too long")
	}
	// Dev builds and the opt-out never touch the network.
	empty := t.TempDir()
	if got := release.Check(ctx, empty, "dev", src, now); got != "" {
		t.Fatalf("dev: %q", got)
	}
	t.Setenv("TC_NO_UPDATE_CHECK", "1")
	if got := release.Check(ctx, empty, "v0.1.0", src, now); got != "" {
		t.Fatalf("disabled: %q", got)
	}
	if _, err := os.Stat(filepath.Join(empty, release.CheckFile)); !os.IsNotExist(err) {
		t.Fatal("disabled check wrote a cache file")
	}
}
