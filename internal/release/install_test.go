package release_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/release"
	"github.com/skailama/tc/internal/release/releasetest"
)

// The tests run the real install.sh (repo root) with sh against a fake
// release host, a fake uname and a stub "tc" that records how it was run.

const installInvite = "tc://chat.example.com:7443/abcdefghijklmnopqrstuvwxyz#Pin-_x"

// stubTC logs its os/arch, its argv, then "TC_INVITE=<value>".
func stubTC(osArch string) []byte {
	return []byte("#!/bin/sh\nprintf '%s\\n' " + osArch + " \"$@\" \"TC_INVITE=${TC_INVITE-}\" > \"$TC_STUB_LOG\"\ncat > \"$TC_STUB_LOG.stdin\"\n")
}

func publishAll(t *testing.T, srv *releasetest.Server, tag string) {
	bins := map[string][]byte{}
	for _, oa := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
		bins[oa] = stubTC(oa + "@" + tag)
	}
	srv.Publish(t, tag, bins)
}

type installRun struct {
	home, log, dir string
	stdout, stderr string
	err            error
}

type installOpts struct {
	unameS, unameM string
	env            []string // extra KEY=VALUE
	args           []string
	piped          bool // cat install.sh | sh -s -- args, like the one-liner
	installDir     string
	onPath         bool
	signers        string            // ALLOWED_SIGNERS in the script; "": the server's key
	stubs          map[string]string // extra commands on PATH: name → script
}

var allowedSignersRE = regexp.MustCompile(`(?m)^ALLOWED_SIGNERS='([^']*)'$`)

// scriptSigners returns install.sh and its embedded ALLOWED_SIGNERS.
func scriptSigners(t *testing.T) (script []byte, signers string) {
	t.Helper()
	b, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := allowedSignersRE.FindAllSubmatch(b, -1)
	if len(m) != 1 {
		t.Fatalf("install.sh has %d ALLOWED_SIGNERS lines, want 1", len(m))
	}
	return b, string(m[0][1])
}

func runInstall(t *testing.T, srv *releasetest.Server, o installOpts) installRun {
	t.Helper()
	for _, tool := range []string{"curl", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	base := srv.Base
	b, _ := scriptSigners(t)
	signers := o.signers
	if signers == "" {
		signers = srv.Signers
	}
	b = allowedSignersRE.ReplaceAllLiteral(b, []byte("ALLOWED_SIGNERS='"+signers+"'"))
	script := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(script, b, 0o644); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	stubs := t.TempDir()
	uname := "#!/bin/sh\ncase \"$1\" in -s) echo '" + o.unameS + "' ;; -m) echo '" + o.unameM + "' ;; *) echo '" + o.unameS + "' ;; esac\n"
	if err := os.WriteFile(filepath.Join(stubs, "uname"), []byte(uname), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range o.stubs {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(home, ".local", "bin")
	if o.installDir != "" {
		dir = o.installDir
	}
	path := stubs + ":" + os.Getenv("PATH")
	if o.onPath {
		path = dir + ":" + path
	}
	r := installRun{home: home, log: filepath.Join(home, "stub.log"), dir: dir}
	var cmd *exec.Cmd
	if o.piped {
		cmd = exec.Command("sh", append([]string{"-s", "--"}, o.args...)...)
		cmd.Stdin = bytes.NewReader(b)
	} else {
		cmd = exec.Command("sh", append([]string{script}, o.args...)...)
	}
	cmd.Dir = home
	cmd.Env = append([]string{
		"HOME=" + home, "PATH=" + path, "SHELL=/bin/zsh",
		"TC_RELEASE_BASE=" + base, "TC_STUB_LOG=" + r.log,
	}, o.env...)
	if o.installDir != "" {
		cmd.Env = append(cmd.Env, "TC_INSTALL_DIR="+o.installDir)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	r.err = cmd.Run()
	r.stdout, r.stderr = out.String(), errb.String()
	return r
}

func (r installRun) stubLog(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(r.log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func TestInstallPicksArchAndJoins(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	publishAll(t, srv, "v0.4.0")
	for _, c := range []struct{ s, m, want string }{
		{"Linux", "x86_64", "linux_amd64"},
		{"Linux", "aarch64", "linux_arm64"},
		{"Darwin", "x86_64", "darwin_amd64"},
		{"Darwin", "arm64", "darwin_arm64"},
		{"Linux", "amd64", "linux_amd64"},
	} {
		r := runInstall(t, srv, installOpts{unameS: c.s, unameM: c.m, args: []string{installInvite}})
		if r.err != nil {
			t.Fatalf("%s/%s: %v\n%s", c.s, c.m, r.err, r.stderr)
		}
		got := r.stubLog(t)
		// The invite goes in the environment, never argv (ps shows argv).
		want := []string{c.want + "@v0.4.0", "join", "TC_INVITE=" + installInvite}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%s/%s: tc ran as %q, want %q", c.s, c.m, got, want)
		}
		st, err := os.Stat(filepath.Join(r.dir, "tc"))
		if err != nil || st.Mode().Perm() != 0o755 {
			t.Fatalf("installed binary: %v %v", st, err)
		}
		// Not on PATH: the exact line to add.
		if !strings.Contains(r.stderr, `export PATH="`+r.dir+`:$PATH"`) || !strings.Contains(r.stderr, ".zshrc") {
			t.Fatalf("no PATH hint:\n%s", r.stderr)
		}
		ents, _ := os.ReadDir(r.dir)
		if len(ents) != 1 {
			t.Fatalf("leftovers in %s: %v", r.dir, ents)
		}
	}
}

// The real one-liner pipes the script into sh (sh -s -- '<invite>'). The
// main-at-the-end layout means sh has read the whole script before tc join
// runs, so join sees no script bytes on stdin.
func TestInstallPiped(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	publishAll(t, srv, "v0.4.0")
	r := runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64", args: []string{installInvite}, piped: true})
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.stderr)
	}
	if got := r.stubLog(t); len(got) != 3 || got[2] != "TC_INVITE="+installInvite {
		t.Fatalf("tc ran as %q", got)
	}
	if b, _ := os.ReadFile(r.log + ".stdin"); len(b) != 0 {
		t.Fatalf("tc join read %d bytes of stdin (the script?)", len(b))
	}
}

func TestInstallChecksumMismatchAborts(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	publishAll(t, srv, "v0.4.0")
	srv.Put("v0.4.0", "tc_linux_amd64.tar.gz", releasetest.Tarball(stubTC("evil")))
	r := runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64", args: []string{installInvite}})
	if r.err == nil || !strings.Contains(r.stderr, "checksum mismatch") {
		t.Fatalf("err %v\n%s", r.err, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "tc")); !os.IsNotExist(err) {
		t.Fatal("installed despite checksum mismatch")
	}
	if r.stubLog(t) != nil {
		t.Fatal("ran tc despite checksum mismatch")
	}
	// No entry at all for the asset is also fatal.
	srv.PutChecksums(t, "v0.4.0", []byte(strings.Repeat("0", 64)+"  tc_darwin_arm64.tar.gz\n"))
	r = runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64"})
	if r.err == nil || !strings.Contains(r.stderr, "no entry for tc_linux_amd64.tar.gz") {
		t.Fatalf("err %v\n%s", r.err, r.stderr)
	}
}

func TestInstallVersionDirAndNoInvite(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	publishAll(t, srv, "v0.4.0")
	publishAll(t, srv, "v0.3.0")
	dir := filepath.Join(t.TempDir(), "my bin") // a space: everything is quoted
	r := runInstall(t, srv, installOpts{unameS: "Darwin", unameM: "arm64", installDir: dir, onPath: true,
		env: []string{"TC_VERSION=0.3.0"}})
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.stderr)
	}
	b, err := os.ReadFile(filepath.Join(dir, "tc"))
	if err != nil || !bytes.Contains(b, []byte("darwin_arm64@v0.3.0")) {
		t.Fatalf("installed %q %v", b, err)
	}
	if strings.Contains(r.stderr, "not on your PATH") {
		t.Fatalf("PATH warning with dir on PATH:\n%s", r.stderr)
	}
	if r.stubLog(t) != nil || !strings.Contains(r.stderr, "tc join") {
		t.Fatalf("no invite: tc ran or no hint\n%s", r.stderr)
	}
	for _, h := range srv.Hits() {
		if h == "/releases/latest" {
			t.Fatal("TC_VERSION set but latest was queried")
		}
	}
}

// The invite reaches tc join as one literal argument: nothing in it is run.
func TestInstallInviteIsNotEvaluated(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	publishAll(t, srv, "v0.4.0")
	nasty := "tc://h:7443/x$(touch PWNED)`touch PWNED2`;touch PWNED3 #p'\""
	r := runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64", args: []string{nasty}})
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.stderr)
	}
	if got := r.stubLog(t); len(got) != 3 || got[2] != "TC_INVITE="+nasty {
		t.Fatalf("tc ran as %q", got)
	}
	for _, f := range []string{"PWNED", "PWNED2", "PWNED3"} {
		if _, err := os.Stat(filepath.Join(r.home, f)); err == nil {
			t.Fatalf("%s created: the invite was evaluated", f)
		}
	}
}

func TestInstallFailsClearly(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	publishAll(t, srv, "v0.4.0")
	for _, c := range []struct {
		name string
		o    installOpts
		want string
	}{
		{"arch", installOpts{unameS: "Linux", unameM: "i686"}, "unsupported CPU"},
		{"os", installOpts{unameS: "FreeBSD", unameM: "amd64"}, "unsupported OS"},
		{"invite", installOpts{unameS: "Linux", unameM: "x86_64", args: []string{"https://evil"}}, "should be an invite"},
		{"args", installOpts{unameS: "Linux", unameM: "x86_64", args: []string{installInvite, "x"}}, "usage"},
		{"version", installOpts{unameS: "Linux", unameM: "x86_64", env: []string{"TC_VERSION=v1.0.0;id"}}, "bad version"},
		{"missing", installOpts{unameS: "Linux", unameM: "x86_64", env: []string{"TC_VERSION=v9.9.9"}}, "can't download"},
	} {
		r := runInstall(t, srv, c.o)
		if r.err == nil || !strings.Contains(r.stderr, c.want) {
			t.Errorf("%s: err %v, stderr:\n%s", c.name, r.err, r.stderr)
		}
		if _, err := os.Stat(filepath.Join(r.dir, "tc")); err == nil {
			t.Errorf("%s: installed anyway", c.name)
		}
	}
	srv.SetLatest("v1.0.0/../../x")
	if r := runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64"}); r.err == nil {
		t.Errorf("bad latest tag accepted:\n%s", r.stderr)
	}
	srv.SetLatest("")
	if r := runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64"}); r.err == nil || !strings.Contains(r.stderr, "no releases yet") {
		t.Errorf("no releases: %v\n%s", r.err, r.stderr)
	}
}

// install.sh and tc update trust the same release keys.
func TestInstallSignersMatch(t *testing.T) {
	_, signers := scriptSigners(t)
	if signers != release.AllowedSigners {
		t.Fatalf("install.sh ALLOWED_SIGNERS\n  %q\ndiffers from release.AllowedSigners\n  %q", signers, release.AllowedSigners)
	}
}

func TestInstallRequiresSignature(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	publishAll(t, srv, "v0.4.0")
	if r := runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64"}); r.err != nil || !strings.Contains(r.stderr, "signature OK") {
		t.Fatalf("signed release: %v\n%s", r.err, r.stderr)
	}
	// Release-write access: swap the tarball and checksums.txt together.
	evil := releasetest.Tarball(stubTC("evil"))
	srv.Put("v0.4.0", "tc_linux_amd64.tar.gz", evil)
	sums := []byte(fmt.Sprintf("%x  tc_linux_amd64.tar.gz\n", sha256.Sum256(evil)))
	srv.Put("v0.4.0", "checksums.txt", sums)
	other := releasetest.NewKey(t)
	for _, c := range []struct {
		name string
		sig  []byte // nil: no .sig served
		o    installOpts
		want string
	}{
		{"stale signature", releasetest.Sign(t, srv.Key, release.SigNamespace, []byte("old")), installOpts{}, "not signed by the tc release key"},
		{"attacker key", releasetest.Sign(t, other, release.SigNamespace, sums), installOpts{}, "not signed by the tc release key"},
		{"wrong namespace", releasetest.Sign(t, srv.Key, "git", sums), installOpts{}, "not signed by the tc release key"},
		{"no signature", nil, installOpts{}, "can't download checksums.txt.sig"},
		{"placeholder key", releasetest.Sign(t, srv.Key, release.SigNamespace, sums), installOpts{signers: release.Placeholder}, "no release signing key"},
	} {
		if c.sig == nil {
			srv.Remove("v0.4.0", release.SigFile)
		} else {
			srv.Put("v0.4.0", release.SigFile, c.sig)
		}
		c.o.unameS, c.o.unameM, c.o.args = "Linux", "x86_64", []string{installInvite}
		r := runInstall(t, srv, c.o)
		if r.err == nil || !strings.Contains(r.stderr, c.want) {
			t.Errorf("%s: err %v\n%s", c.name, r.err, r.stderr)
		}
		if _, err := os.Stat(filepath.Join(r.dir, "tc")); !os.IsNotExist(err) || r.stubLog(t) != nil {
			t.Errorf("%s: installed or ran tc", c.name)
		}
	}
	// The explicit, loud opt-out.
	srv.Remove("v0.4.0", release.SigFile)
	r := runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64", env: []string{"TC_INSECURE_NO_SIG=1"}})
	if r.err != nil || !strings.Contains(r.stderr, "WARNING: TC_INSECURE_NO_SIG=1") {
		t.Fatalf("opt-out: %v\n%s", r.err, r.stderr)
	}
}

// TC_RELEASE_BASE must be https; http only to loopback; no user info.
func TestInstallRefusesInsecureBase(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	for _, c := range []struct{ base, want string }{
		{"http://example.invalid/releases", "must be an https:// URL"},
		{"http://127.0.0.1.example.invalid/releases", "must be an https:// URL"},
		{"http://localhost.example.invalid/releases", "must be an https:// URL"},
		{"ftp://127.0.0.1/releases", "must be an https:// URL"},
		{"http://localhost:80@example.invalid/releases", "must not contain '@'"},
	} {
		r := runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64", env: []string{"TC_RELEASE_BASE=" + c.base}})
		if r.err == nil || !strings.Contains(r.stderr, c.want) {
			t.Errorf("%s: err %v\n%s", c.base, r.err, r.stderr)
		}
	}
}

// Over https, curl must not follow a redirect down to plain http, and must
// ignore a ~/.curlrc (here one that turns certificate checks off).
func TestInstallHTTPSOnly(t *testing.T) {
	srv := releasetest.NewTLS(t, "v0.4.0")
	publishAll(t, srv, "v0.4.0")
	ca := "CURL_CA_BUNDLE=" + srv.CertFile(t)
	opts := func(env ...string) installOpts {
		return installOpts{unameS: "Linux", unameM: "x86_64", env: append([]string{"TC_RELEASE_BASE=" + srv.Base}, env...)}
	}
	if r := runInstall(t, srv, opts(ca)); r.err != nil {
		t.Skipf("this curl can't use CURL_CA_BUNDLE for the test server: %v\n%s", r.err, r.stderr)
	}

	// The same tarball, but served over plain http (loopback, so only the
	// redirect rule can stop it).
	plain := releasetest.New(t, "v0.4.0")
	arc := releasetest.Tarball(stubTC("linux_amd64@v0.4.0"))
	srv.Put("v0.4.0", "tc_linux_amd64.tar.gz", arc)
	plain.Put("v0.4.0", "tc_linux_amd64.tar.gz", arc)
	srv.PutChecksums(t, "v0.4.0", []byte(fmt.Sprintf("%x  tc_linux_amd64.tar.gz\n", sha256.Sum256(arc))))
	if r := runInstall(t, srv, opts(ca)); r.err != nil {
		t.Fatalf("https: %v\n%s", r.err, r.stderr)
	}
	srv.Redirect("v0.4.0", "tc_linux_amd64.tar.gz", plain.Base+"/download/v0.4.0/tc_linux_amd64.tar.gz")
	r := runInstall(t, srv, opts(ca))
	if r.err == nil || !strings.Contains(r.stderr, "can't download tc_linux_amd64.tar.gz") {
		t.Fatalf("followed an https → http redirect: %v\n%s", r.err, r.stderr)
	}
	for _, h := range plain.Hits() {
		t.Fatalf("plain-http server was contacted: %s", h)
	}

	// A ~/.curlrc with "insecure" must not make an untrusted certificate OK.
	srv.Redirect("v0.4.0", "tc_linux_amd64.tar.gz", "")
	if r := runInstall(t, srv, opts(ca)); r.err != nil {
		t.Fatalf("https again: %v\n%s", r.err, r.stderr)
	}
	r = runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64", env: []string{"TC_RELEASE_BASE=" + srv.Base, "CURL_HOME=" + writeCurlrc(t, "insecure\n")}})
	if r.err == nil {
		t.Fatalf("~/.curlrc insecure was honored:\n%s", r.stderr)
	}
}

func writeCurlrc(t *testing.T, body string) string {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".curlrc"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Every curl call goes through fetch, which pins the protocol (for
// redirects too), TLS ≥ 1.2 and ignores ~/.curlrc.
func TestInstallCurlFlags(t *testing.T) {
	b, _ := scriptSigners(t)
	var calls []string
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "#") || !regexp.MustCompile(`(^|[\s(])curl\s`).MatchString(l) {
			continue
		}
		calls = append(calls, l)
	}
	if len(calls) != 1 || calls[0] != `curl -q --proto "$PROTO" --proto-redir "$PROTO" --tlsv1.2 "$@"` {
		t.Fatalf("curl calls outside fetch, or fetch flags changed: %q", calls)
	}
	if !strings.Contains(string(b), "\nPROTO='=https'\n") {
		t.Fatal("PROTO must default to =https")
	}
}

// The binary is copied into a file mktemp already created (O_EXCL, so not
// a symlink someone planted in a shared dir), never to a predictable
// .tc.new.$$ path that cp would create or follow. A stub cp checks.
func TestInstallCopiesIntoMktempFile(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	publishAll(t, srv, "v0.4.0")
	dir := filepath.Join(t.TempDir(), "shared")
	cpLog := filepath.Join(t.TempDir(), "cp.log")
	cp := "#!/bin/sh\n" +
		"if [ -f \"$2\" ] && [ ! -L \"$2\" ] && [ ! -s \"$2\" ]; then echo \"precreated $2\" >> '" + cpLog + "'; else echo \"fresh $2\" >> '" + cpLog + "'; fi\n" +
		"exec /bin/cp \"$@\"\n"
	r := runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64", installDir: dir, stubs: map[string]string{"cp": cp}})
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.stderr)
	}
	b, _ := os.ReadFile(cpLog)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "precreated "+filepath.Join(dir, ".tc.new.")) {
		t.Fatalf("cp calls: %q", lines)
	}
	if suffix := strings.TrimPrefix(lines[0], "precreated "+filepath.Join(dir, ".tc.new.")); regexp.MustCompile(`^[0-9]+$`).MatchString(suffix) {
		t.Fatalf("temp name is a pid: %q", lines[0])
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("leftovers: %v", ents)
	}
}

// A (correctly signed and checksummed) archive whose tc member is a
// symlink is refused: [ -f ] alone follows it.
func TestInstallRejectsSymlinkMember(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	target := filepath.Join(t.TempDir(), "elsewhere")
	os.WriteFile(target, stubTC("evil"), 0o755)
	arc := releasetest.SymlinkTarball(target)
	srv.Put("v0.4.0", "tc_linux_amd64.tar.gz", arc)
	srv.PutChecksums(t, "v0.4.0", []byte(fmt.Sprintf("%x  tc_linux_amd64.tar.gz\n", sha256.Sum256(arc))))
	r := runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64", args: []string{installInvite}})
	if r.err == nil || !strings.Contains(r.stderr, "has no tc binary") {
		t.Fatalf("symlink member: %v\n%s", r.err, r.stderr)
	}
	if _, err := os.Lstat(filepath.Join(r.dir, "tc")); !os.IsNotExist(err) || r.stubLog(t) != nil {
		t.Fatal("installed or ran the symlink target")
	}
}

// A redirect URL from the release host is shown without its escape bytes.
// (Recent curl refuses raw control bytes in a Location, so a stub curl
// reports one as url_effective, as a laxer curl or proxy could.)
func TestInstallSanitizesRedirectURL(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	curl := "#!/bin/sh\nprintf 'https://evil.example/elsewhere/\\033]0;pwned\\007\\033[2Jx'\n"
	r := runInstall(t, srv, installOpts{unameS: "Linux", unameM: "x86_64", stubs: map[string]string{"curl": curl}})
	if r.err == nil || !strings.Contains(r.stderr, "unexpected answer") {
		t.Fatalf("%v\n%q", r.err, r.stderr)
	}
	if strings.ContainsAny(r.stderr, "\x1b\x07") {
		t.Fatalf("control bytes reached the terminal: %q", r.stderr)
	}
}
