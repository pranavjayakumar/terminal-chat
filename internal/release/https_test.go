package release_test

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/skailama/tc/internal/release"
	"github.com/skailama/tc/internal/release/releasetest"
)

func TestCheckURL(t *testing.T) {
	for _, s := range []string{
		"https://github.com/x/y/releases", "https://objects.githubusercontent.com/a",
		"http://127.0.0.1:8080/releases", "http://127.9.9.9/x", "http://localhost:1/x", "http://LOCALHOST/x", "http://[::1]:9/x",
	} {
		u, _ := url.Parse(s)
		if err := release.CheckURL(u); err != nil {
			t.Errorf("%s refused: %v", s, err)
		}
	}
	for _, s := range []string{
		"http://github.com/x", "http://10.0.0.1/x", "http://127.0.0.1.evil.example/x", "http://localhost.evil.example/x",
		"ftp://127.0.0.1/x", "file:///etc/passwd", "https:///nohost", "//github.com/x", "x",
	} {
		u, _ := url.Parse(s)
		if err := release.CheckURL(u); !errors.Is(err, release.ErrInsecureURL) {
			t.Errorf("%s accepted (%v)", s, err)
		}
	}
}

// A hostile TC_RELEASE_BASE (http to a real host, or user-info tricks that
// make a loopback-looking URL point elsewhere) is refused before any request.
func TestSourceRefusesInsecureBase(t *testing.T) {
	ctx := context.Background()
	signers := releasetest.AllowedSigners(releasetest.NewKey(t).PublicKey())
	for _, base := range []string{
		"http://example.invalid/releases",
		"http://localhost:80@example.invalid/releases",
		"https://user@example.invalid/releases",
		"ftp://127.0.0.1/releases",
	} {
		src := &release.Source{Base: base, Signers: signers}
		if _, err := src.Latest(ctx); !errors.Is(err, release.ErrInsecureURL) {
			t.Errorf("Latest %s: %v", base, err)
		}
		if _, err := src.Fetch(ctx, "v1.2.3", "linux", "amd64"); !errors.Is(err, release.ErrInsecureURL) {
			t.Errorf("Fetch %s: %v", base, err)
		}
	}
	t.Setenv("TC_RELEASE_BASE", "http://example.invalid/releases/")
	if _, err := release.NewSource().Latest(ctx); !errors.Is(err, release.ErrInsecureURL) {
		t.Errorf("env base: %v", err)
	}
}

// An https release host that redirects a download to plain http is refused:
// a redirect can't downgrade the connection.
func TestFetchRefusesHTTPRedirect(t *testing.T) {
	ctx := context.Background()
	srv := releasetest.NewTLS(t, "v1.2.3")
	srv.Publish(t, "v1.2.3", map[string][]byte{"linux_amd64": []byte("GOOD")})
	if bin, err := srv.Source().Fetch(ctx, "v1.2.3", "linux", "amd64"); err != nil || string(bin) != "GOOD" {
		t.Fatalf("https release: %q %v", bin, err)
	}
	srv.Redirect("v1.2.3", "tc_linux_amd64.tar.gz", "http://example.invalid/tc_linux_amd64.tar.gz")
	if _, err := srv.Source().Fetch(ctx, "v1.2.3", "linux", "amd64"); !errors.Is(err, release.ErrInsecureURL) {
		t.Fatalf("redirect to http: err = %v", err)
	}
	// Redirects that stay on https are followed (GitHub sends downloads to a CDN).
	srv.Redirect("v1.2.3", "tc_linux_amd64.tar.gz", srv.URL+"/releases/download/v1.2.3/mirror.tar.gz")
	srv.Put("v1.2.3", "mirror.tar.gz", releasetest.Tarball([]byte("GOOD")))
	if bin, err := srv.Source().Fetch(ctx, "v1.2.3", "linux", "amd64"); err != nil || string(bin) != "GOOD" {
		t.Fatalf("https redirect: %q %v", bin, err)
	}
}
