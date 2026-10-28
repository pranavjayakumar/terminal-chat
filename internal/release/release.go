// Package release finds, verifies and installs tc releases published on
// GitHub Releases (.github/workflows/release.yml). install.sh uses the same
// layout:
//
//	<base>/latest                      → redirects to <base>/tag/<tag>
//	<base>/download/<tag>/checksums.txt  sha256sum format
//	<base>/download/<tag>/tc_<os>_<arch>.tar.gz  holds the tc binary
//	<base>/download/<tag>/tc-voice_<os>_<arch>.tar.gz  the voice build (cgo, -tags voice)
//
// <base> is $TC_RELEASE_BASE (tests point it at a local server). Every URL,
// the base and each redirect, must be https; plain http is allowed only to
// loopback hosts (CheckURL).
package release

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultBase is where releases live unless TC_RELEASE_BASE says otherwise.
const DefaultBase = "https://github.com/pranavjayakumar/terminal-chat/releases"

// Size caps: a release tarball, the binary in it, and everything the
// tarball decompresses to (a gzip bomb in another member stops here).
const (
	maxArchive  = 64 << 20
	maxBinary   = 96 << 20
	maxUnpacked = 128 << 20
	maxSmallDoc = 64 << 10
)

// Base returns $TC_RELEASE_BASE or DefaultBase, without a trailing slash.
func Base() string {
	if b := os.Getenv("TC_RELEASE_BASE"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return DefaultBase
}

// Source fetches releases from Base.
type Source struct {
	Base string
	HTTP *http.Client // nil: a client with a 60 s timeout
	// Signers is the allowed_signers text release signatures are checked
	// against; "" means the embedded AllowedSigners.
	Signers string
	// Voice fetches the voice build (tc-voice_*), so tc update keeps the
	// flavor that is running.
	Voice bool
}

func (s *Source) signers() string {
	if s.Signers != "" {
		return s.Signers
	}
	return AllowedSigners
}

// NewSource uses Base().
func NewSource() *Source { return &Source{Base: Base()} }

// client is s.HTTP (or a 60 s-timeout client) that refuses redirects to
// anything CheckURL rejects, so a hop can't downgrade to plain http.
func (s *Source) client() *http.Client {
	c := http.Client{Timeout: 60 * time.Second}
	if s.HTTP != nil {
		c = *s.HTTP
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return CheckURL(req.URL)
	}
	return &c
}

// ErrInsecureURL means a release URL wasn't https (or loopback http).
var ErrInsecureURL = errors.New("release downloads must use https")

// CheckURL accepts https URLs, and http ones only for loopback hosts
// (localhost, 127.0.0.0/8, ::1), which tests and local mirrors use.
func CheckURL(u *url.URL) error {
	switch {
	case u.Scheme == "https" && u.Host != "":
		return nil
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		return nil
	}
	return fmt.Errorf("%w: refusing %s://%s", ErrInsecureURL, u.Scheme, u.Host)
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkBase validates s.Base before any request is made.
func (s *Source) checkBase() error {
	u, err := url.Parse(s.Base)
	if err != nil {
		return fmt.Errorf("release base: %w", err)
	}
	if u.User != nil {
		return fmt.Errorf("%w: release base must not hold user info", ErrInsecureURL)
	}
	if err := CheckURL(u); err != nil {
		return fmt.Errorf("TC_RELEASE_BASE: %w", err)
	}
	return nil
}

// tagRE is the tags we accept: v1.2.3 with an optional suffix. Anything else
// (a path, a quote) from a redirect is refused.
var tagRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]{1,32})?$`)

// ValidTag reports whether tag looks like a release tag.
func ValidTag(tag string) bool { return tagRE.MatchString(tag) }

// Latest returns the newest release's tag, read from where <base>/latest
// redirects (no API call, so no API rate limit).
func (s *Source) Latest(ctx context.Context) (string, error) {
	if err := s.checkBase(); err != nil {
		return "", err
	}
	c := *s.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.Base+"/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("checking latest release: %w", err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
		return "", fmt.Errorf("checking latest release: HTTP %d (no releases yet?)", resp.StatusCode)
	}
	tag := path.Base(strings.TrimRight(strings.SplitN(loc, "?", 2)[0], "/"))
	if !strings.Contains(loc, "/tag/") || !ValidTag(tag) {
		return "", fmt.Errorf("checking latest release: unexpected redirect to %q", loc)
	}
	return tag, nil
}

func (s *Source) get(ctx context.Context, tag, name string, limit int64) ([]byte, error) {
	if !ValidTag(tag) {
		return nil, fmt.Errorf("bad release tag %q", tag)
	}
	if err := s.checkBase(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Base+"/download/"+tag+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s %s: HTTP %d", tag, name, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", name, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("downloading %s: larger than %d bytes", name, limit)
	}
	return b, nil
}

// AssetName is the release asset for an OS/arch, e.g. tc_linux_amd64.tar.gz.
func AssetName(goos, goarch string) string { return "tc_" + goos + "_" + goarch + ".tar.gz" }

// VoiceAssetName is the voice build's asset, e.g. tc-voice_linux_amd64.tar.gz.
// Its tarball holds a binary named tc too, so either replaces the other.
func VoiceAssetName(goos, goarch string) string {
	return "tc-voice_" + goos + "_" + goarch + ".tar.gz"
}

// Supported reports whether releases include goos/goarch.
func Supported(goos, goarch string) bool {
	return (goos == "linux" || goos == "darwin") && (goarch == "amd64" || goarch == "arm64")
}

// ParseChecksums reads sha256sum output: "<hex>  <name>" (or "*<name>").
func ParseChecksums(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 || len(f[0]) != 64 {
			continue
		}
		if _, err := hex.DecodeString(f[0]); err != nil {
			continue
		}
		out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
	}
	return out
}

// LegacyUnversioned is the newest release whose checksums.txt may lack the
// "version <tag>" line: every release up to it was built before the workflow
// wrote one (v0.6.2 was published by the old workflow). Anything newer must carry it.
const LegacyUnversioned = "v0.6.2"

// ErrVersionLine means a signed checksums.txt isn't bound to the tag being
// installed: an old signed file republished under a newer tag (a rollback).
var ErrVersionLine = errors.New("checksums.txt is not for this release")

var plainTagRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// CheckVersionLine checks that the signed checksums.txt names tag: exactly
// one line whose first field is "version", and it is "version <tag>". The
// signature alone covers only the hashes, so without this anyone who can
// edit releases could serve an old, signed checksums.txt (and its old
// binaries) as a newer tag. Plain tags up to LegacyUnversioned may lack
// the line; nothing newer may.
func CheckVersionLine(sums []byte, tag string) error {
	n, exact := 0, 0
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == "version" {
			n++
			if line == "version "+tag {
				exact++
			}
		}
	}
	switch {
	case n == 1 && exact == 1:
		return nil
	case n == 0 && plainTagRE.MatchString(tag) && !Newer(tag, LegacyUnversioned):
		return nil
	case n == 0:
		return fmt.Errorf("%w: %s has no \"version %s\" line; not installing", ErrVersionLine, tag, tag)
	}
	return fmt.Errorf("%w: its version line doesn't match %s; not installing", ErrVersionLine, tag)
}

// ErrChecksum means a download didn't match checksums.txt.
var ErrChecksum = errors.New("checksum mismatch")

// Fetch downloads tag's binary for goos/goarch, checks checksums.txt's
// signature (SigFile) against the allowed signers, checks the archive
// against checksums.txt and returns the tc binary from it.
func (s *Source) Fetch(ctx context.Context, tag, goos, goarch string) ([]byte, error) {
	if !Supported(goos, goarch) {
		return nil, fmt.Errorf("no release builds for %s/%s", goos, goarch)
	}
	if _, err := parseAllowedSigners(s.signers()); err != nil {
		return nil, err
	}
	asset := AssetName(goos, goarch)
	if s.Voice {
		asset = VoiceAssetName(goos, goarch)
	}
	sums, err := s.get(ctx, tag, "checksums.txt", maxSmallDoc)
	if err != nil {
		return nil, err
	}
	sig, err := s.get(ctx, tag, SigFile, maxSmallDoc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v (unsigned release?); not installing", ErrSignature, err)
	}
	if err := VerifySignature(s.signers(), sums, sig); err != nil {
		return nil, fmt.Errorf("checksums.txt for %s: %w; not installing", tag, err)
	}
	if err := CheckVersionLine(sums, tag); err != nil {
		return nil, err
	}
	want, ok := ParseChecksums(sums)[asset]
	if !ok {
		return nil, fmt.Errorf("checksums.txt for %s has no entry for %s", tag, asset)
	}
	arc, err := s.get(ctx, tag, asset, maxArchive)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(arc)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s: %w (expected %s, got %x); not installing", asset, ErrChecksum, want, got)
	}
	return extract(ctx, arc)
}

// errUnpacked: the archive decompresses to more than maxUnpacked.
var errUnpacked = errors.New("archive unpacks to too much data")

// capReader reads at most n bytes from r, then fails, and stops once ctx is
// done; it bounds the work of skipping over members we don't want.
type capReader struct {
	ctx context.Context
	r   io.Reader
	n   int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	if c.n <= 0 {
		return 0, errUnpacked
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

// extract returns the regular file named "tc" from a .tar.gz, reading at
// most maxUnpacked decompressed bytes and stopping when ctx is done.
func extract(ctx context.Context, arc []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(arc))
	if err != nil {
		return nil, fmt.Errorf("reading archive: %w", err)
	}
	tr := tar.NewReader(&capReader{ctx: ctx, r: zr, n: maxUnpacked})
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("archive has no tc binary")
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		if path.Clean(strings.TrimPrefix(h.Name, "./")) != "tc" || h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > maxBinary {
			return nil, errors.New("tc binary in archive is too large")
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxBinary+1))
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		if len(b) == 0 || len(b) > maxBinary {
			return nil, errors.New("tc binary in archive has a bad size")
		}
		return b, nil
	}
}

// version is a parsed vMAJOR.MINOR.PATCH (anything after is ignored).
func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || strconv.Itoa(n) != p {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// IsRelease reports whether v is a release version (not "dev").
func IsRelease(v string) bool { _, ok := parse(v); return ok }

// Newer reports whether latest is a newer release than current. A current
// that isn't a release version (dev builds) is never behind.
func Newer(latest, current string) bool {
	l, ok1 := parse(latest)
	c, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// Replace atomically replaces the file at exe with bin: a temp file in the
// same directory, fchmod, fsync, rename. It fails without touching exe if the
// directory isn't writable.
func Replace(exe string, bin []byte) error { return replace(exe, bin, nil) }

// replace is Replace; afterClose (tests) runs between closing the temp file
// and renaming it, where another user of a shared directory could act.
func replace(exe string, bin []byte, afterClose func(tmp string)) error {
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	mode := os.FileMode(0o755)
	if st, err := os.Stat(exe); err == nil {
		// Never hand the new binary group/world write, even if the old
		// one had it: anyone in that group could then replace tc.
		mode = (st.Mode().Perm() | 0o111) &^ 0o022
	}
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".tc-update-*")
	if err != nil {
		return fmt.Errorf("can't write to %s (%w); reinstall with install.sh or rerun with the permissions that installed it", dir, err)
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(name)
		}
	}()
	// chmod the open file, not the path: in a shared directory the path
	// could be swapped between Close and a chmod by name.
	for _, step := range []func() error{
		func() error { _, err := tmp.Write(bin); return err },
		func() error { return tmp.Chmod(mode) },
		tmp.Sync,
	} {
		if err := step(); err != nil {
			tmp.Close()
			return fmt.Errorf("writing update: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing update: %w", err)
	}
	if afterClose != nil {
		afterClose(name)
	}
	if err := os.Rename(name, exe); err != nil {
		return fmt.Errorf("replacing %s: %w", exe, err)
	}
	ok = true
	return nil
}

// Executable is the running binary's real path.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return exe, nil
}

// IsHomebrew reports whether exe (a symlink-resolved path, see Executable)
// lives in a Homebrew Cellar, where brew owns upgrades and a self-replace
// would leave its receipts and checksums stale.
func IsHomebrew(exe string) bool {
	p := filepath.ToSlash(exe)
	i := strings.LastIndex(p, "/Cellar/tc/")
	return i >= 0 && strings.HasSuffix(p, "/bin/tc") && strings.Count(p[i+len("/Cellar/tc/"):], "/") == 2
}

// ---- background update check ----

// CheckFile is the cache of the last update check in TC_HOME.
const CheckFile = "update-check.json"

// CheckEvery is how often Check goes to the network.
const CheckEvery = 24 * time.Hour

type checkCache struct {
	Checked int64  `json:"checked"` // Unix ms of the last attempt
	Latest  string `json:"latest,omitempty"`
}

// Disabled reports whether TC_NO_UPDATE_CHECK turns the check off.
func Disabled() bool {
	v := os.Getenv("TC_NO_UPDATE_CHECK")
	return v != "" && v != "0"
}

// Check returns the newest release tag if it is newer than current, else "".
// It goes to the network at most once per CheckEvery (successful or not),
// remembering the answer in home/update-check.json; errors and offline
// mean "". Dev builds and TC_NO_UPDATE_CHECK never check.
func Check(ctx context.Context, home, current string, src *Source, now time.Time) string {
	if Disabled() || !IsRelease(current) || runtime.GOOS == "windows" {
		return ""
	}
	file := filepath.Join(home, CheckFile)
	var c checkCache
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	last := time.UnixMilli(c.Checked)
	if c.Checked == 0 || now.Sub(last) >= CheckEvery || now.Before(last) {
		c.Checked = now.UnixMilli()
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		tag, err := src.Latest(ctx)
		cancel()
		if err == nil {
			c.Latest = tag
		}
		if b, err := json.Marshal(c); err == nil {
			writeFileAtomic(file, b)
		}
	}
	if ValidTag(c.Latest) && Newer(c.Latest, current) {
		return c.Latest
	}
	return ""
}

func writeFileAtomic(file string, b []byte) {
	dir := filepath.Dir(file)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".update-check-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), file) != nil {
		os.Remove(tmp.Name())
	}
}
