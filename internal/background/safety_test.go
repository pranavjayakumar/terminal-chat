package background

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// A group-writable directory (or binary) lets every member of the group
// swap the binary, unless the group is one we trust.
func TestSafeExeGroupWritable(t *testing.T) {
	uid := os.Getuid()
	if uid == 0 {
		t.Skip("root owns everything")
	}
	base, _ := filepath.EvalSymlinks(t.TempDir())
	shared := filepath.Join(base, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(shared, "tc")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	gid := fi.Sys().(*syscall.Stat_t).Gid
	// Only our test dir's group is judged: the real ancestors are
	// whatever they are on this machine.
	untrusted := func(g uint32) bool { return g != gid }
	trusted := func(uint32) bool { return true }
	// refused reports whether safeExe blamed p itself.
	refused := func(p string, groupOK func(uint32) bool) bool {
		err := safeExe(exe, uid, groupOK)
		return err != nil && strings.Contains(err.Error(), p+" could be replaced")
	}

	if refused(shared, untrusted) || refused(exe, untrusted) {
		t.Fatal("refused a 0755 dir and binary")
	}
	os.Chmod(shared, 0o775)
	if !refused(shared, untrusted) {
		t.Fatal("accepted a group-writable dir whose group isn't trusted")
	}
	if refused(shared, trusted) {
		t.Fatal("refused a group-writable dir whose group is trusted")
	}
	os.Chmod(shared, 0o755)
	os.Chmod(exe, 0o775)
	if !refused(exe, untrusted) {
		t.Fatal("accepted a group-writable binary whose group isn't trusted")
	}
	// World-writable is never fine, whatever the group.
	os.Chmod(exe, 0o757)
	if !refused(exe, trusted) {
		t.Fatal("accepted a world-writable binary")
	}
}

func TestTrustedGroup(t *testing.T) {
	ok := trustedGroup(os.Getuid())
	if !ok(0) {
		t.Error("gid 0 (root/wheel) not trusted")
	}
	if ok(4000000123) {
		t.Error("trusted a group that doesn't exist")
	}
	if runtime.GOOS == "darwin" {
		// Homebrew's prefix is admin-writable; staff is every user.
		if !ok(80) {
			t.Error("admin (80) not trusted: Homebrew installs would be refused")
		}
		if ok(20) {
			t.Error("staff (20) trusted: every local user is in it")
		}
	}
}

// The service never points at a binary another user could swap in.
func TestSafeExe(t *testing.T) {
	uid := os.Getuid()
	if uid == 0 {
		t.Skip("root owns everything")
	}
	base, _ := filepath.EvalSymlinks(t.TempDir())
	if err := SafeExe("/usr/bin/true", uid); err != nil && !strings.Contains(err.Error(), "checking") {
		t.Fatalf("/usr/bin/true: %v", err)
	}
	os.WriteFile("/tmp/tc-safeexe-test", []byte("x"), 0o755)
	defer os.Remove("/tmp/tc-safeexe-test")
	if err := SafeExe("/tmp/tc-safeexe-test", uid); err == nil {
		t.Fatal("accepted a binary in /tmp")
	}
	shared := filepath.Join(base, "shared")
	os.Mkdir(shared, 0o755)
	os.Chmod(shared, 0o777)
	exe := filepath.Join(shared, "tc")
	os.WriteFile(exe, []byte("x"), 0o755)
	if err := SafeExe(exe, uid); err == nil || !strings.Contains(err.Error(), "another user") {
		t.Fatalf("world-writable dir: %v", err)
	}
	if err := SafeExe(exe, uid+12345); err == nil {
		t.Fatal("accepted a binary owned by someone else")
	}
	os.Chmod(shared, 0o755)
	if err := SafeExe(exe, uid); err != nil && !strings.Contains(err.Error(), "another user") {
		t.Fatalf("private dir: %v", err)
	}
}

func TestSSHAgentSocketPolicy(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		keep bool
	}{
		{map[string]string{"SSH_AUTH_SOCK": "/run/user/1000/keyring/ssh"}, true},
		{map[string]string{"SSH_AUTH_SOCK": "/tmp/ssh-AbCd/agent.123"}, false},
		{map[string]string{"SSH_AUTH_SOCK": "/run/user/1000/gnupg/S.gpg-agent.ssh", "SSH_CONNECTION": "10.0.0.1 22 10.0.0.2 22"}, false},
	} {
		sv, err := NewService(ServiceSpec{GOOS: "linux", Home: "/h/tc", Exe: "/x/tc", UserHome: "/h", Getenv: env(c.env)})
		if err != nil {
			t.Fatal(err)
		}
		if _, kept := sv.Env["SSH_AUTH_SOCK"]; kept != c.keep {
			t.Errorf("%v: kept=%v", c.env, kept)
		}
	}
}
