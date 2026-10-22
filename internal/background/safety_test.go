package background

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
