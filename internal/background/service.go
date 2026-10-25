package background

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/skailama/tc/internal/release"
)

// Autostart (docs/TUI.md): a LaunchAgent on macOS, a systemd user unit on
// Linux, each running `tc background run` with KeepAlive/Restart and a 30 s
// pause between restarts, so a crash or a server that's down never turns
// into a busy loop.

const (
	launchdLabel = "dev.tc.background"
	systemdName  = "tc-background"
	restartPause = 30 // seconds between restarts
)

// passEnv are the variables `tc background on` copies into the service:
// the service manager starts it without the user's shell environment.
var passEnv = []string{"TC_NOTIFY", "TC_SOUND", "TC_NOTIFY_CONTENT", "TC_QUIET", "SSH_AUTH_SOCK"}

// Service is one TC_HOME's autostart entry.
type Service struct {
	GOOS  string
	Label string // launchd label, or the systemd unit name without .service
	Exe   string // the tc binary the service runs
	Home  string // TC_HOME, absolute
	Env   map[string]string
	File  string // the plist or unit file
	Log   string // where launchd sends stdout/stderr (panics); the process logs to LogFile itself
}

// ServiceSpec is what NewService needs.
type ServiceSpec struct {
	GOOS        string
	Home        string // TC_HOME
	DefaultHome bool   // Home is the default (~/.config/tc): plain label
	Exe         string
	UserHome    string // ~
	XDGConfig   string // $XDG_CONFIG_HOME ("" : ~/.config)
	Getenv      func(string) string
}

// NewService describes the autostart entry for s.
func NewService(s ServiceSpec) (Service, error) {
	if s.Getenv == nil {
		s.Getenv = func(string) string { return "" }
	}
	for what, p := range map[string]string{"TC_HOME": s.Home, "the tc binary": s.Exe, "the home directory": s.UserHome} {
		if !filepath.IsAbs(p) {
			return Service{}, fmt.Errorf("%s (%q) must be an absolute path", what, p)
		}
		if hasControl(p) {
			return Service{}, fmt.Errorf("%s has a control character in its path; can't write a service file for it", what)
		}
	}
	sv := Service{GOOS: s.GOOS, Exe: s.Exe, Home: filepath.Clean(s.Home), Env: map[string]string{}}
	suffix := ""
	if !s.DefaultHome {
		sum := sha256.Sum256([]byte(sv.Home))
		suffix = hex.EncodeToString(sum[:4])
	}
	sv.Env["TC_HOME"] = sv.Home
	for _, k := range passEnv {
		v := s.Getenv(k)
		if v == "" || hasControl(v) {
			continue
		}
		// launchd hands every agent the system ssh-agent's socket; a
		// copy of this login's would go stale at the next reboot.
		if k == "SSH_AUTH_SOCK" && s.GOOS == "darwin" &&
			(strings.HasPrefix(v, "/private/tmp/com.apple.launchd.") || strings.HasPrefix(v, "/tmp/com.apple.launchd.")) {
			continue
		}
		// A forwarded or per-login agent (/tmp/ssh-XXXX/agent.N) is gone
		// at logout, and its path in a readable unit invites a squatter.
		if k == "SSH_AUTH_SOCK" && (s.Getenv("SSH_CONNECTION") != "" || strings.HasPrefix(v, "/tmp/ssh-")) {
			continue
		}
		sv.Env[k] = v
	}
	// The service manager's PATH is bare (/usr/bin:/bin…): the agent it
	// runs needs this login's, where claude, node, git and gh live.
	if p := servicePath(s.Getenv("PATH")); p != "" {
		sv.Env["PATH"] = p
	}
	switch s.GOOS {
	case "darwin":
		sv.Label = launchdLabel
		if suffix != "" {
			sv.Label += "." + suffix
		}
		sv.File = filepath.Join(s.UserHome, "Library", "LaunchAgents", sv.Label+".plist")
		sv.Log = filepath.Join(sv.Home, "background.err.log")
	case "linux":
		sv.Label = systemdName
		if suffix != "" {
			sv.Label += "-" + suffix
		}
		cfg := s.XDGConfig
		if cfg == "" || !filepath.IsAbs(cfg) {
			cfg = filepath.Join(s.UserHome, ".config")
		}
		if hasControl(cfg) {
			return Service{}, errors.New("XDG_CONFIG_HOME has a control character in it")
		}
		sv.File = filepath.Join(cfg, "systemd", "user", sv.Label+".service")
	default:
		return Service{}, fmt.Errorf("tc background needs macOS or Linux (this is %s)", s.GOOS)
	}
	return sv, nil
}

// servicePath keeps PATH's absolute entries, once each.
func servicePath(path string) string {
	if hasControl(path) {
		return ""
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range filepath.SplitList(path) {
		if !filepath.IsAbs(d) || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return strings.Join(out, string(filepath.ListSeparator))
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func (sv Service) envKeys() []string {
	keys := make([]string, 0, len(sv.Env))
	for k := range sv.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Content is the plist or unit file.
func (sv Service) Content() []byte {
	if sv.GOOS == "darwin" {
		return sv.plist()
	}
	return sv.unit()
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (sv Service) plist() []byte {
	var b bytes.Buffer
	str := func(indent, s string) { b.WriteString(indent + "<string>" + xmlEscape(s) + "</string>\n") }
	key := func(indent, s string) { b.WriteString(indent + "<key>" + xmlEscape(s) + "</key>\n") }
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	key("\t", "Label")
	str("\t", sv.Label)
	key("\t", "ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range []string{sv.Exe, "background", "run"} {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	key("\t", "EnvironmentVariables")
	b.WriteString("\t<dict>\n")
	for _, k := range sv.envKeys() {
		key("\t\t", k)
		str("\t\t", sv.Env[k])
	}
	b.WriteString("\t</dict>\n")
	key("\t", "RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("\t", "KeepAlive")
	b.WriteString("\t<true/>\n")
	key("\t", "ThrottleInterval")
	b.WriteString("\t<integer>" + strconv.Itoa(restartPause) + "</integer>\n")
	key("\t", "StandardOutPath")
	str("\t", sv.Log)
	key("\t", "StandardErrorPath")
	str("\t", sv.Log)
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

// systemdQuote quotes s as one word for systemd.syntax(7): backslash and
// double quote escaped, % doubled (specifiers), and in a command line $
// doubled too (variable expansion).
func systemdQuote(s string, exec bool) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "%", "%%")
	if exec {
		s = strings.ReplaceAll(s, "$", "$$")
	}
	return `"` + s + `"`
}

func (sv Service) unit() []byte {
	var b bytes.Buffer
	b.WriteString("[Unit]\nDescription=tc background notifications (mentions, DMs, huddle rings while tc is closed)\n\n[Service]\nType=simple\n")
	b.WriteString("ExecStart=" + systemdQuote(sv.Exe, true) + " background run\n")
	for _, k := range sv.envKeys() {
		b.WriteString("Environment=" + systemdQuote(k+"="+sv.Env[k], false) + "\n")
	}
	b.WriteString("Restart=always\nRestartSec=" + strconv.Itoa(restartPause) + "\n\n[Install]\nWantedBy=default.target\n")
	return b.Bytes()
}

// ---- installing ----

// Runner runs a service-manager command (launchctl, systemctl); tests fake it.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner runs the real command.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return string(out), nil
}

// Manager installs and controls a Service.
type Manager struct {
	S   Service
	Run Runner
	UID int
	// Sleep waits between launchctl retries (tests: no-op).
	Sleep func(time.Duration)
	// CheckExe vets the binary before Install points a login service at
	// it (SafeExe); nil: no check (tests).
	CheckExe func(string) error
}

func (m Manager) domain() string { return "gui/" + strconv.Itoa(m.UID) }
func (m Manager) target() string { return m.domain() + "/" + m.S.Label }
func (m Manager) unitName() string {
	return m.S.Label + ".service"
}

// Installed reports whether the service file is there.
func (m Manager) Installed() bool {
	_, err := os.Lstat(m.S.File)
	return err == nil
}

// Install writes the service file and (re)starts the service. Running it
// again picks up a new binary path or environment.
func (m Manager) Install(ctx context.Context) error {
	if m.CheckExe != nil {
		if err := m.CheckExe(m.S.Exe); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(m.S.File), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(m.S.File), err)
	}
	if err := writeFileAtomic(m.S.File, m.S.Content(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", m.S.File, err)
	}
	switch m.S.GOOS {
	case "darwin":
		// Unload whatever is loaded (an older file, a crashed job), then
		// load the new file; bootout settles asynchronously, so retry.
		_, _ = m.Run(ctx, "launchctl", "bootout", m.target())
		_, _ = m.Run(ctx, "launchctl", "enable", m.target())
		var err error
		for i := 0; i < 10; i++ {
			if _, err = m.Run(ctx, "launchctl", "bootstrap", m.domain(), m.S.File); err == nil {
				return nil
			}
			m.sleep(300 * time.Millisecond)
		}
		return fmt.Errorf("starting the LaunchAgent (is this a desktop login?): %w", err)
	default:
		if _, err := m.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return fmt.Errorf("systemd user session unavailable (tc background needs one): %w", err)
		}
		if _, err := m.Run(ctx, "systemctl", "--user", "enable", m.unitName()); err != nil {
			return err
		}
		_, err := m.Run(ctx, "systemctl", "--user", "restart", m.unitName())
		return err
	}
}

// TurnOn installs and (re)starts the service for TC_HOME home, as `tc
// background on` does. Running it again picks up this login's PATH and
// binary.
func TurnOn(ctx context.Context, m Manager, home string) error {
	if _, err := OSSender(); err != nil {
		return err
	}
	// launchd would create its stderr log following symlinks, with the
	// default umask: make it ourselves first.
	if f, err := os.OpenFile(filepath.Join(home, "background.err.log"), os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600); err == nil {
		f.Close()
	}
	return m.Install(ctx)
}

// Uninstall stops the service and removes its file. Nothing installed is
// not an error.
func (m Manager) Uninstall(ctx context.Context) error {
	switch m.S.GOOS {
	case "darwin":
		_, _ = m.Run(ctx, "launchctl", "bootout", m.target())
	default:
		_, _ = m.Run(ctx, "systemctl", "--user", "disable", "--now", m.unitName())
	}
	if err := os.Remove(m.S.File); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", m.S.File, err)
	}
	if m.S.GOOS != "darwin" {
		_, _ = m.Run(ctx, "systemctl", "--user", "daemon-reload")
	}
	return nil
}

// Restart restarts the installed service (after tc update).
func (m Manager) Restart(ctx context.Context) error {
	if m.S.GOOS == "darwin" {
		_, err := m.Run(ctx, "launchctl", "kickstart", "-k", m.target())
		return err
	}
	_, err := m.Run(ctx, "systemctl", "--user", "restart", m.unitName())
	return err
}

func (m Manager) sleep(d time.Duration) {
	if m.Sleep != nil {
		m.Sleep(d)
		return
	}
	time.Sleep(d)
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Local is the Manager for home's service on this machine, running this
// binary.
func Local(home string) (Manager, error) {
	abs, err := filepath.Abs(home)
	if err != nil {
		return Manager{}, err
	}
	uh, err := os.UserHomeDir()
	if err != nil {
		return Manager{}, err
	}
	exe, err := ServiceExe()
	if err != nil {
		return Manager{}, err
	}
	def := filepath.Join(uh, ".config", "tc")
	sv, err := NewService(ServiceSpec{GOOS: runtime.GOOS, Home: abs, DefaultHome: filepath.Clean(abs) == def,
		Exe: exe, UserHome: uh, XDGConfig: os.Getenv("XDG_CONFIG_HOME"), Getenv: os.Getenv})
	if err != nil {
		return Manager{}, err
	}
	uid := os.Getuid()
	return Manager{S: sv, Run: ExecRunner, UID: uid, CheckExe: func(p string) error { return SafeExe(p, uid) }}, nil
}

// SafeExe refuses a binary another local user could replace: the service
// would run it as us at every login (and every restart). The file and every
// directory above it must be owned by us or root and not world-writable;
// a sticky /tmp doesn't help, since once our file there is cleaned up
// anyone can create the same path.
func SafeExe(exe string, uid int) error {
	bad := func(p string) error {
		return fmt.Errorf("%s could be replaced by another user of this machine, so tc background won't run it at every login; install tc first (install.sh or brew), then run tc background on with that tc", p)
	}
	// A Homebrew bin/tc link: its target's directories count too.
	if real, err := filepath.EvalSymlinks(exe); err == nil && real != exe {
		if err := safePath(real, uid, bad); err != nil {
			return err
		}
	}
	return safePath(exe, uid, bad)
}

func safePath(exe string, uid int, bad func(string) error) error {
	for p := exe; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("checking %s: %w", p, err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || (int(st.Uid) != uid && st.Uid != 0) || fi.Mode().Perm()&0o002 != 0 && fi.Mode()&os.ModeSymlink == 0 {
			return bad(p)
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

// ServiceExe is the path the service should run: this binary, except that
// a Homebrew install uses the prefix's stable bin/tc symlink, not the
// versioned Cellar path a `brew upgrade` deletes.
func ServiceExe() (string, error) {
	exe, err := release.Executable()
	if err != nil {
		return "", err
	}
	return stableExe(exe), nil
}

func stableExe(exe string) string {
	if !release.IsHomebrew(exe) {
		return exe
	}
	p := filepath.ToSlash(exe)
	prefix := p[:strings.LastIndex(p, "/Cellar/tc/")]
	link := filepath.FromSlash(prefix + "/bin/tc")
	if real, err := filepath.EvalSymlinks(link); err == nil && real == exe {
		return link
	}
	return exe
}
