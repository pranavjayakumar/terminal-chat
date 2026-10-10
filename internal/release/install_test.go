package release_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/release/releasetest"
)

// The tests run the real install.sh (repo root) with sh against a fake
// release host, a fake uname and a stub "tc" that records how it was run.

const installInvite = "tc://chat.example.com:7443/abcdefghijklmnopqrstuvwxyz#Pin-_x"

func stubTC(osArch string) []byte {
	return []byte("#!/bin/sh\nprintf '%s\\n' " + osArch + " \"$@\" > \"$TC_STUB_LOG\"\ncat > \"$TC_STUB_LOG.stdin\"\n")
}

func publishAll(srv *releasetest.Server, tag string) {
	bins := map[string][]byte{}
	for _, oa := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
		bins[oa] = stubTC(oa + "@" + tag)
	}
	srv.Publish(tag, bins)
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
}

func runInstall(t *testing.T, base string, o installOpts) installRun {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	script, err := filepath.Abs("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	stubs := t.TempDir()
	uname := "#!/bin/sh\ncase \"$1\" in -s) echo '" + o.unameS + "' ;; -m) echo '" + o.unameM + "' ;; *) echo '" + o.unameS + "' ;; esac\n"
	if err := os.WriteFile(filepath.Join(stubs, "uname"), []byte(uname), 0o755); err != nil {
		t.Fatal(err)
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
		b, _ := os.ReadFile(script)
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
	publishAll(srv, "v0.4.0")
	for _, c := range []struct{ s, m, want string }{
		{"Linux", "x86_64", "linux_amd64"},
		{"Linux", "aarch64", "linux_arm64"},
		{"Darwin", "x86_64", "darwin_amd64"},
		{"Darwin", "arm64", "darwin_arm64"},
		{"Linux", "amd64", "linux_amd64"},
	} {
		r := runInstall(t, srv.Base, installOpts{unameS: c.s, unameM: c.m, args: []string{installInvite}})
		if r.err != nil {
			t.Fatalf("%s/%s: %v\n%s", c.s, c.m, r.err, r.stderr)
		}
		got := r.stubLog(t)
		want := []string{c.want + "@v0.4.0", "join", installInvite}
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
	publishAll(srv, "v0.4.0")
	r := runInstall(t, srv.Base, installOpts{unameS: "Linux", unameM: "x86_64", args: []string{installInvite}, piped: true})
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.stderr)
	}
	if got := r.stubLog(t); len(got) != 3 || got[2] != installInvite {
		t.Fatalf("tc ran as %q", got)
	}
	if b, _ := os.ReadFile(r.log + ".stdin"); len(b) != 0 {
		t.Fatalf("tc join read %d bytes of stdin (the script?)", len(b))
	}
}

func TestInstallChecksumMismatchAborts(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	publishAll(srv, "v0.4.0")
	srv.Put("v0.4.0", "tc_linux_amd64.tar.gz", releasetest.Tarball(stubTC("evil")))
	r := runInstall(t, srv.Base, installOpts{unameS: "Linux", unameM: "x86_64", args: []string{installInvite}})
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
	srv.Put("v0.4.0", "checksums.txt", []byte(strings.Repeat("0", 64)+"  tc_darwin_arm64.tar.gz\n"))
	r = runInstall(t, srv.Base, installOpts{unameS: "Linux", unameM: "x86_64"})
	if r.err == nil || !strings.Contains(r.stderr, "no entry for tc_linux_amd64.tar.gz") {
		t.Fatalf("err %v\n%s", r.err, r.stderr)
	}
}

func TestInstallVersionDirAndNoInvite(t *testing.T) {
	srv := releasetest.New(t, "v0.4.0")
	publishAll(srv, "v0.4.0")
	publishAll(srv, "v0.3.0")
	dir := filepath.Join(t.TempDir(), "my bin") // a space: everything is quoted
	r := runInstall(t, srv.Base, installOpts{unameS: "Darwin", unameM: "arm64", installDir: dir, onPath: true,
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
	publishAll(srv, "v0.4.0")
	nasty := "tc://h:7443/x$(touch PWNED)`touch PWNED2`;touch PWNED3 #p'\""
	r := runInstall(t, srv.Base, installOpts{unameS: "Linux", unameM: "x86_64", args: []string{nasty}})
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.stderr)
	}
	if got := r.stubLog(t); len(got) != 3 || got[2] != nasty {
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
	publishAll(srv, "v0.4.0")
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
		r := runInstall(t, srv.Base, c.o)
		if r.err == nil || !strings.Contains(r.stderr, c.want) {
			t.Errorf("%s: err %v, stderr:\n%s", c.name, r.err, r.stderr)
		}
		if _, err := os.Stat(filepath.Join(r.dir, "tc")); err == nil {
			t.Errorf("%s: installed anyway", c.name)
		}
	}
	srv.SetLatest("v1.0.0/../../x")
	if r := runInstall(t, srv.Base, installOpts{unameS: "Linux", unameM: "x86_64"}); r.err == nil {
		t.Errorf("bad latest tag accepted:\n%s", r.stderr)
	}
	srv.SetLatest("")
	if r := runInstall(t, srv.Base, installOpts{unameS: "Linux", unameM: "x86_64"}); r.err == nil || !strings.Contains(r.stderr, "no releases yet") {
		t.Errorf("no releases: %v\n%s", r.err, r.stderr)
	}
}
