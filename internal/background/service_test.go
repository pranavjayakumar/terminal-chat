package background

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// A path with spaces, quotes, %, $, & and < must reach launchd and systemd
// as exactly one unchanged argument.
const nastyDir = `/Users/Jo O'Neil & "Co"/<50%$HOME>`

func TestPlistGolden(t *testing.T) {
	sv, err := NewService(ServiceSpec{
		GOOS: "darwin", Home: nastyDir + "/tc", Exe: nastyDir + "/bin/tc", UserHome: nastyDir,
		Getenv: env(map[string]string{
			"TC_SOUND": "off", "TC_QUIET": "22:00-08:00",
			"SSH_AUTH_SOCK": "/private/tmp/com.apple.launchd.abc/Listeners", // launchd gives its own
			"TC_NOTIFY":     "bad\nvalue",                                   // control characters: dropped
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sv.Label, "dev.tc.background.") || len(sv.Label) != len("dev.tc.background.")+8 {
		t.Fatalf("non-default home label %q", sv.Label)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + sv.Label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/Jo O&apos;Neil &amp; &quot;Co&quot;/&lt;50%$HOME&gt;/bin/tc</string>
		<string>background</string>
		<string>run</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>TC_HOME</key>
		<string>/Users/Jo O&apos;Neil &amp; &quot;Co&quot;/&lt;50%$HOME&gt;/tc</string>
		<key>TC_QUIET</key>
		<string>22:00-08:00</string>
		<key>TC_SOUND</key>
		<string>off</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>StandardOutPath</key>
	<string>/Users/Jo O&apos;Neil &amp; &quot;Co&quot;/&lt;50%$HOME&gt;/tc/background.err.log</string>
	<key>StandardErrorPath</key>
	<string>/Users/Jo O&apos;Neil &amp; &quot;Co&quot;/&lt;50%$HOME&gt;/tc/background.err.log</string>
</dict>
</plist>
`
	if got := string(sv.Content()); got != want {
		t.Fatalf("plist:\n%s\nwant:\n%s", got, want)
	}
	if sv.File != filepath.Join(nastyDir, "Library", "LaunchAgents", sv.Label+".plist") {
		t.Fatalf("plist path %q", sv.File)
	}
	if plutil, err := lookPlutil(); err == nil {
		p := filepath.Join(t.TempDir(), "x.plist")
		os.WriteFile(p, sv.Content(), 0o600)
		if out, err := runCmd(plutil, "-lint", p); err != nil {
			t.Fatalf("plutil -lint: %v %s", err, out)
		}
	}
}

func TestUnitGolden(t *testing.T) {
	sv, err := NewService(ServiceSpec{
		GOOS: "linux", Home: nastyDir + "/tc", Exe: nastyDir + "/bin/tc", UserHome: "/home/jo",
		XDGConfig: "/home/jo/my config",
		Getenv:    env(map[string]string{"SSH_AUTH_SOCK": "/run/user/1000/keyring/ssh", "TC_NOTIFY_CONTENT": "name"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `[Unit]
Description=tc background notifications (mentions, DMs, huddle rings while tc is closed)

[Service]
Type=simple
ExecStart="/Users/Jo O'Neil & \"Co\"/<50%%$$HOME>/bin/tc" background run
Environment="SSH_AUTH_SOCK=/run/user/1000/keyring/ssh"
Environment="TC_HOME=/Users/Jo O'Neil & \"Co\"/<50%%$HOME>/tc"
Environment="TC_NOTIFY_CONTENT=name"
Restart=always
RestartSec=30

[Install]
WantedBy=default.target
`
	if got := string(sv.Content()); got != want {
		t.Fatalf("unit:\n%s\nwant:\n%s", got, want)
	}
	if sv.File != "/home/jo/my config/systemd/user/"+sv.Label+".service" || !strings.HasPrefix(sv.Label, "tc-background-") {
		t.Fatalf("unit %q at %q", sv.Label, sv.File)
	}
}

func lookPlutil() (string, error) { return exec.LookPath("plutil") }

func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

func TestDefaultHomeLabelsAndValidation(t *testing.T) {
	for goos, want := range map[string]string{"darwin": "dev.tc.background", "linux": "tc-background"} {
		sv, err := NewService(ServiceSpec{GOOS: goos, Home: "/home/jo/.config/tc", DefaultHome: true, Exe: "/home/jo/.local/bin/tc", UserHome: "/home/jo"})
		if err != nil || sv.Label != want {
			t.Fatalf("%s: label %q err %v", goos, sv.Label, err)
		}
	}
	a, _ := NewService(ServiceSpec{GOOS: "linux", Home: "/tmp/a", Exe: "/x/tc", UserHome: "/home/jo"})
	b, _ := NewService(ServiceSpec{GOOS: "linux", Home: "/tmp/b", Exe: "/x/tc", UserHome: "/home/jo"})
	if a.Label == b.Label {
		t.Fatal("two homes share a unit name")
	}
	for _, bad := range []ServiceSpec{
		{GOOS: "linux", Home: "rel/tc", Exe: "/x/tc", UserHome: "/home/jo"},
		{GOOS: "linux", Home: "/tmp/a", Exe: "tc", UserHome: "/home/jo"},
		{GOOS: "linux", Home: "/tmp/a\n[Service]", Exe: "/x/tc", UserHome: "/home/jo"},
		{GOOS: "darwin", Home: "/tmp/a", Exe: "/x/t\x00c", UserHome: "/home/jo"},
		{GOOS: "windows", Home: "/tmp/a", Exe: "/x/tc", UserHome: "/home/jo"},
	} {
		if _, err := NewService(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	// A login's own ssh-agent socket is kept on macOS (1Password, Secretive).
	sv, _ := NewService(ServiceSpec{GOOS: "darwin", Home: "/tmp/a", Exe: "/x/tc", UserHome: "/Users/jo",
		Getenv: env(map[string]string{"SSH_AUTH_SOCK": "/Users/jo/Library/Containers/com.maxgoedjen.Secretive.SecretAgent/Data/socket.ssh"})})
	if sv.Env["SSH_AUTH_SOCK"] == "" {
		t.Fatal("custom agent socket dropped")
	}
}

type call []string

func fakeRunner(calls *[]call, fail map[string]int) Runner {
	return func(_ context.Context, name string, args ...string) (string, error) {
		c := append(call{name}, args...)
		*calls = append(*calls, c)
		key := strings.Join(c[:min(len(c), 3)], " ")
		if fail[key] > 0 {
			fail[key]--
			return "", context.DeadlineExceeded
		}
		return "", nil
	}
}

func TestManagerLaunchd(t *testing.T) {
	uh := t.TempDir()
	sv, err := NewService(ServiceSpec{GOOS: "darwin", Home: filepath.Join(uh, "tc home"), DefaultHome: true, Exe: "/opt/homebrew/bin/tc", UserHome: uh})
	if err != nil {
		t.Fatal(err)
	}
	var calls []call
	// bootout of a fresh service settles asynchronously: bootstrap fails twice.
	m := Manager{S: sv, UID: 501, Run: fakeRunner(&calls, map[string]int{"launchctl bootstrap gui/501": 2}), Sleep: func(time.Duration) {}}
	if m.Installed() {
		t.Fatal("installed before Install")
	}
	for range 2 { // idempotent
		calls = nil
		if err := m.Install(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if !m.Installed() {
		t.Fatal("no plist")
	}
	b, _ := os.ReadFile(sv.File)
	if string(b) != string(sv.Content()) {
		t.Fatal("plist content")
	}
	fi, _ := os.Stat(sv.File)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("plist mode %v", fi.Mode())
	}
	want := []call{
		{"launchctl", "bootout", "gui/501/dev.tc.background"},
		{"launchctl", "enable", "gui/501/dev.tc.background"},
		{"launchctl", "bootstrap", "gui/501", sv.File},
	}
	if !slices.EqualFunc(calls, want, slices.Equal) {
		t.Fatalf("calls %q\nwant %q", calls, want)
	}
	calls = nil
	if err := m.Restart(context.Background()); err != nil || !slices.Equal(calls[0], call{"launchctl", "kickstart", "-k", "gui/501/dev.tc.background"}) {
		t.Fatalf("restart: %v %q", err, calls)
	}
	for range 2 {
		calls = nil
		if err := m.Uninstall(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if m.Installed() {
		t.Fatal("plist left behind")
	}
	ents, _ := os.ReadDir(filepath.Dir(sv.File))
	if len(ents) != 0 {
		t.Fatalf("leftovers: %v", ents)
	}
}

func TestManagerSystemd(t *testing.T) {
	uh := t.TempDir()
	sv, err := NewService(ServiceSpec{GOOS: "linux", Home: filepath.Join(uh, ".config", "tc"), DefaultHome: true, Exe: filepath.Join(uh, ".local", "bin", "tc"), UserHome: uh})
	if err != nil {
		t.Fatal(err)
	}
	var calls []call
	m := Manager{S: sv, Run: fakeRunner(&calls, nil)}
	if err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "tc-background.service"},
		{"systemctl", "--user", "restart", "tc-background.service"},
	}
	if !slices.EqualFunc(calls, want, slices.Equal) {
		t.Fatalf("calls %q\nwant %q", calls, want)
	}
	calls = nil
	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	want = []call{
		{"systemctl", "--user", "disable", "--now", "tc-background.service"},
		{"systemctl", "--user", "daemon-reload"},
	}
	if !slices.EqualFunc(calls, want, slices.Equal) || m.Installed() {
		t.Fatalf("uninstall calls %q (installed %v)", calls, m.Installed())
	}
	// No systemd user session: a clear error, not a half install.
	calls = nil
	m.Run = fakeRunner(&calls, map[string]int{"systemctl --user daemon-reload": 1})
	if err := m.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "systemd user session") {
		t.Fatalf("no systemd: %v", err)
	}
}

// A Homebrew binary runs from the prefix's bin/tc link, which survives a
// brew upgrade; anything else runs from where it is.
func TestStableExe(t *testing.T) {
	prefix := t.TempDir()
	real := filepath.Join(prefix, "Cellar", "tc", "0.5.0", "bin", "tc")
	os.MkdirAll(filepath.Dir(real), 0o755)
	os.WriteFile(real, []byte("x"), 0o755)
	os.MkdirAll(filepath.Join(prefix, "bin"), 0o755)
	link := filepath.Join(prefix, "bin", "tc")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	real, _ = filepath.EvalSymlinks(real)
	if got := stableExe(real); got != filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(real))))), "bin", "tc") {
		t.Fatalf("homebrew exe %q", got)
	}
	plain := filepath.Join(prefix, "home", ".local", "bin", "tc")
	if got := stableExe(plain); got != plain {
		t.Fatalf("plain exe %q", got)
	}
	// A Cellar path whose bin/tc link points elsewhere stays as it is.
	os.Remove(link)
	if got := stableExe(real); got != real {
		t.Fatalf("dangling link: %q", got)
	}
}

func TestLogRotates(t *testing.T) {
	home := t.TempDir()
	w, err := OpenLog(home)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.Repeat("x", 1023) + "\n")
	for range 1500 {
		w.Write(line)
	}
	w.Close()
	a, _ := os.Stat(filepath.Join(home, LogFile))
	b, err := os.Stat(filepath.Join(home, LogFile+".1"))
	if err != nil || a.Size() > maxLog || b.Size() > maxLog {
		t.Fatalf("rotation: %v %v %v", a, b, err)
	}
}

func TestServiceKeepsLoginPath(t *testing.T) {
	sv, err := NewService(ServiceSpec{GOOS: "darwin", Home: "/Users/jo/.config/tc", DefaultHome: true, Exe: "/Users/jo/.local/bin/tc", UserHome: "/Users/jo",
		Getenv: env(map[string]string{"PATH": "/Users/jo/.local/bin:bin:/opt/homebrew/bin:/Users/jo/.local/bin:/usr/bin"})})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sv.Env["PATH"], "/Users/jo/.local/bin:/opt/homebrew/bin:/usr/bin"; got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	if !strings.Contains(string(sv.Content()), "<key>PATH</key>") {
		t.Fatal("the plist doesn't set PATH")
	}
}
