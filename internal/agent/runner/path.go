package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ErrNoClaude: Claude Code isn't installed where this computer's PATH, a
// login shell's PATH or the usual install places can find it.
var ErrNoClaude = errors.New("claude (Claude Code) isn't installed on this computer: install it from https://claude.com/claude-code, then run claude once to sign in")

// installDirs are where Claude Code, node, git and gh usually live. A
// service manager (launchd, systemd) starts tc with a bare system PATH
// that has none of the ones under ~.
func installDirs(home string) []string {
	var d []string
	if home != "" {
		for _, p := range []string{".local/bin", ".claude/local", "bin", ".npm-global/bin", ".bun/bin", ".volta/bin", ".local/share/mise/shims", ".asdf/shims", ".nix-profile/bin"} {
			d = append(d, filepath.Join(home, p))
		}
	}
	return append(d, "/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin", "/nix/var/nix/profiles/default/bin", "/snap/bin", "/usr/bin", "/bin")
}

// pathEnv is how EnsurePath reads and changes the environment.
type pathEnv struct {
	getenv func(string) string
	setenv func(string, string) error
	home   string
	// shellPath is the PATH a login shell of the user sets up; "" when
	// it can't tell.
	shellPath func(ctx context.Context) string
}

// EnsurePath makes sure claude (and the tools it runs, like node, git
// and gh) can be found from this process: it adds the usual install
// places and, when claude is still missing, a login shell's PATH. It
// returns ErrNoClaude when claude can't be found at all.
func EnsurePath(ctx context.Context) error {
	home, _ := os.UserHomeDir()
	return ensurePath(ctx, pathEnv{getenv: os.Getenv, setenv: os.Setenv, home: home, shellPath: loginShellPath})
}

func ensurePath(ctx context.Context, e pathEnv) error {
	cur := e.getenv("PATH")
	next := mergePath(cur, installDirs(e.home))
	if lookIn(next, "claude") == "" && e.shellPath != nil {
		next = mergePath(next, filepath.SplitList(e.shellPath(ctx)))
	}
	if next != cur {
		if err := e.setenv("PATH", next); err != nil {
			return err
		}
	}
	if lookIn(next, "claude") == "" {
		return ErrNoClaude
	}
	return nil
}

// mergePath appends the absolute dirs in add that exist and aren't in
// path yet, keeping path's own order first.
func mergePath(path string, add []string) string {
	have := map[string]bool{}
	var out []string
	for _, d := range filepath.SplitList(path) {
		if d != "" && !have[d] {
			have[d] = true
			out = append(out, d)
		}
	}
	for _, d := range add {
		d = filepath.Clean(d)
		if d == "" || !filepath.IsAbs(d) || have[d] {
			continue
		}
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			continue
		}
		have[d] = true
		out = append(out, d)
	}
	return strings.Join(out, string(filepath.ListSeparator))
}

// lookIn is exec.LookPath against path instead of $PATH.
func lookIn(path, name string) string {
	for _, d := range filepath.SplitList(path) {
		if d == "" || !filepath.IsAbs(d) {
			continue
		}
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

const pathMark = "__tc_path__"

// loginShellPath asks the user's shell for the PATH it sets up, as an
// interactive login shell (where most installers add their line), then
// as a plain login shell. A shell that hangs or prints noise is cut off.
func loginShellPath(ctx context.Context) string {
	sh := os.Getenv("SHELL")
	if sh == "" || !filepath.IsAbs(sh) {
		sh = "/bin/sh"
	}
	for _, flags := range [][]string{{"-l", "-i", "-c"}, {"-l", "-c"}} {
		if p := runShell(ctx, sh, append(flags, `printf '\n`+pathMark+`%s\n' "$PATH"`)); p != "" {
			return p
		}
	}
	return ""
}

func runShell(ctx context.Context, sh string, args []string) string {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	cmd := exec.Command(sh, args...)
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return ""
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-done:
		}
	}()
	_ = cmd.Wait()
	close(done)
	return parseShellPath(out.String())
}

func parseShellPath(out string) string {
	i := strings.LastIndex(out, pathMark)
	if i < 0 {
		return ""
	}
	line, _, _ := strings.Cut(out[i+len(pathMark):], "\n")
	return strings.TrimSpace(line)
}
