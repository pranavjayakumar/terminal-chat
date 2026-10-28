// Package safegit runs tc's own git (outside the agent's sandbox) on
// folders the agent can write to. Nothing the agent wrote may run as the
// owner: no hooks, no fsmonitor, no credential helper or config from the
// system or the owner's global files, git dirs pinned rather than found
// through a .git file the agent could rewrite, and an environment without
// the owner's credentials unless a step needs GitHub.
package safegit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Flags go before the subcommand on every tc-side git. Config files the
// agent can reach are already out of its reach to write (settings.go); these
// make sure that even a changed config never runs a program.
var Flags = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "core.untrackedCache=false",
	"-c", "credential.helper=",
	"-c", "commit.gpgsign=false",
	"-c", "tag.gpgsign=false",
	"-c", "core.pager=cat",
	"-c", "protocol.ext.allow=never",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
}

// GHCredential is the only credential helper a tc-side git may use, and
// only for steps that talk to GitHub as the owner (clone, fetch, push).
var GHCredential = []string{"-c", "credential.helper=!gh auth git-credential"}

// ghEnv is what a GitHub step keeps of the owner's credentials.
var ghEnv = map[string]bool{
	"GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_ENTERPRISE_TOKEN": true,
	"GITHUB_ENTERPRISE_TOKEN": true, "GH_HOST": true,
}

// Env is env for a tc-side git or gh: no GIT_* from the caller, no SSH
// agent, no secrets, and neither system nor global git config. creds keeps
// gh's tokens for the steps that need GitHub as the owner.
func Env(env []string, creds bool) []string {
	out := make([]string, 0, len(env)+4)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		switch {
		case creds && ghEnv[u]:
		case strings.HasPrefix(u, "GIT_"), u == "SSH_AUTH_SOCK", u == "SSH_AGENT_PID", u == "SSH_ASKPASS",
			ghEnv[u], Secret(u):
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GH_PROMPT_DISABLED=1",
	)
}

// Secret reports whether an environment variable name (upper case) looks
// like it holds a credential.
func Secret(u string) bool {
	switch {
	case strings.HasPrefix(u, "AWS_"), strings.HasPrefix(u, "GOOGLE_APPLICATION"), strings.HasPrefix(u, "AZURE_"),
		strings.Contains(u, "TOKEN"), strings.Contains(u, "SECRET"), strings.Contains(u, "PASSWORD"),
		strings.Contains(u, "PASSWD"), strings.HasSuffix(u, "_KEY"), strings.Contains(u, "CREDENTIAL"),
		strings.HasSuffix(u, "_PASS"), strings.HasSuffix(u, "_AUTH"), strings.HasSuffix(u, "_DSN"),
		u == "DATABASE_URL", u == "REDIS_URL", u == "KUBECONFIG", u == "NETRC", u == "PGPASSFILE":
		return true
	}
	return false
}

// RealDir checks that dir is a directory and not a symlink.
func RealDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is not a plain directory", dir)
	}
	return nil
}

// RepoGitDir is repoDir/.git after checking it is a real directory (the
// agent cannot swap it for a pointer or a link).
func RepoGitDir(repoDir string) (string, error) {
	g := filepath.Join(repoDir, ".git")
	if err := RealDir(g); err != nil {
		return "", fmt.Errorf("repo %s: %w", repoDir, err)
	}
	return g, nil
}

// ErrBadWorktree means a worktree's .git no longer points where git put it.
var ErrBadWorktree = errors.New("worktree .git was changed")

// WorktreeGitDir checks the worktree wt of the repo at repoDir and returns
// its git dir: wt is a plain directory, wt/.git a plain small file naming a
// directory right under repoDir/.git/worktrees. Callers pass that git dir
// and wt to git explicitly, so git never follows the pointer itself.
func WorktreeGitDir(repoDir, wt string) (string, error) {
	common, err := RepoGitDir(repoDir)
	if err != nil {
		return "", err
	}
	if err := RealDir(wt); err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadWorktree, err)
	}
	ptr := filepath.Join(wt, ".git")
	fi, err := os.Lstat(ptr)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadWorktree, err)
	}
	if !fi.Mode().IsRegular() || fi.Size() > 4096 {
		return "", fmt.Errorf("%w: %s is not a plain pointer file", ErrBadWorktree, ptr)
	}
	b, err := os.ReadFile(ptr)
	if err != nil {
		return "", err
	}
	line, ok := bytes.CutPrefix(bytes.TrimSpace(b), []byte("gitdir: "))
	if !ok || bytes.ContainsAny(line, "\n\r\x00") {
		return "", fmt.Errorf("%w: %s", ErrBadWorktree, ptr)
	}
	gd := string(line)
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(wt, gd)
	}
	gd = filepath.Clean(gd)
	// git writes the real path; the caller may name it through a symlinked
	// parent (/var on macOS).
	want := filepath.Join(common, "worktrees")
	real, err := filepath.EvalSymlinks(common)
	if err != nil {
		return "", err
	}
	if d := filepath.Dir(gd); d != want && d != filepath.Join(real, "worktrees") {
		return "", fmt.Errorf("%w: it points outside %s", ErrBadWorktree, want)
	}
	if err := RealDir(gd); err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadWorktree, err)
	}
	return gd, nil
}

// Linked reports whether path is a regular file with more than one hard
// link. A sandboxed shell can link a file it cannot read into a worktree;
// tc's unsandboxed git would then read it and push it.
func Linked(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Nlink > 1
}
