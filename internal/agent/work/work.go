// Package work runs an agent's code work on the tc repo: a clone in the agent
// home, one git worktree per piece of work, tests, draft PRs and issues
// through the owner's gh account.
package work

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/skailama/tc/internal/agent/home"
)

// DefaultRepo is the GitHub repo agents work on.
const DefaultRepo = "pranavjayakumar/terminal-chat"

// Cmd is one external command.
type Cmd struct {
	Dir  string
	Name string
	Args []string
	Env  []string // added to the current environment
}

func (c Cmd) String() string { return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " ")) }

// Runner runs external commands. Output is stdout and stderr combined.
type Runner interface {
	Run(ctx context.Context, c Cmd) (string, error)
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, c Cmd) (string, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), append([]string{"GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1"}, c.Env...)...)
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return out.String(), err
}

// Work holds where and how an agent works on the tc repo.
type Work struct {
	Runner       Runner
	Repo         string // owner/name on GitHub
	RepoDir      string // the clone
	WorktreesDir string // one folder per worktree
	CloneURL     string // clone from here with git instead of `gh repo clone Repo`
	Base         string // branch work starts from and PRs target
	BranchPrefix string // every work branch starts with this
}

// New returns the Work for the agent home at agentDir.
func New(agentDir string) *Work {
	p := home.PathsFor(agentDir)
	return &Work{
		Runner:       ExecRunner{},
		Repo:         DefaultRepo,
		RepoDir:      p.Repo,
		WorktreesDir: p.Worktrees,
		Base:         "main",
		BranchPrefix: "agent/",
	}
}

// Worktree is a checkout for one piece of work.
type Worktree struct {
	Dir    string
	Branch string
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ValidSlug reports whether slug can name a worktree and branch.
func ValidSlug(slug string) bool { return slugRe.MatchString(slug) }

func (w *Work) run(ctx context.Context, dir, name string, args ...string) (string, error) {
	out, err := w.Runner.Run(ctx, Cmd{Dir: dir, Name: name, Args: args})
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, firstArg(args), err, tail(out, 500))
	}
	return out, nil
}

func firstArg(a []string) string {
	if len(a) == 0 {
		return ""
	}
	if a[0] == "-C" && len(a) > 2 {
		return a[2]
	}
	return a[0]
}

// EnsureClone clones the repo into RepoDir when it is not there yet.
func (w *Work) EnsureClone(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(w.RepoDir, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(w.RepoDir), 0o700); err != nil {
		return fmt.Errorf("creating repo parent: %w", err)
	}
	if w.CloneURL != "" {
		_, err := w.run(ctx, "", "git", "clone", "-q", w.CloneURL, w.RepoDir)
		return err
	}
	_, err := w.run(ctx, "", "gh", "repo", "clone", w.Repo, w.RepoDir, "--", "-q")
	return err
}

// Worktree returns the worktree for slug, creating it from the freshly
// fetched base branch when needed. It clones the repo first if missing.
func (w *Work) Worktree(ctx context.Context, slug string) (Worktree, error) {
	if !ValidSlug(slug) {
		return Worktree{}, fmt.Errorf("bad work slug %q: lowercase letters, digits and dashes, up to 40", slug)
	}
	wt := Worktree{Dir: filepath.Join(w.WorktreesDir, slug), Branch: w.BranchPrefix + slug}
	if err := w.EnsureClone(ctx); err != nil {
		return Worktree{}, err
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, ".git")); err == nil {
		return wt, nil
	}
	if _, err := w.run(ctx, "", "git", "-C", w.RepoDir, "fetch", "-q", "origin"); err != nil {
		return Worktree{}, err
	}
	if err := os.MkdirAll(w.WorktreesDir, 0o700); err != nil {
		return Worktree{}, fmt.Errorf("creating worktrees dir: %w", err)
	}
	_, _ = w.run(ctx, "", "git", "-C", w.RepoDir, "worktree", "prune")
	if _, err := w.run(ctx, "", "git", "-C", w.RepoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+wt.Branch); err == nil {
		_, err = w.run(ctx, "", "git", "-C", w.RepoDir, "worktree", "add", "-q", wt.Dir, wt.Branch)
		return wt, err
	}
	_, err := w.run(ctx, "", "git", "-C", w.RepoDir, "worktree", "add", "-q", "-b", wt.Branch, wt.Dir, "origin/"+w.Base)
	return wt, err
}

// TestResult is the outcome of a test run.
type TestResult struct {
	Passed   bool
	TimedOut bool
	Duration time.Duration
	Tail     string // the last lines of output
}

// TestTailBytes is how much test output a TestResult keeps.
const TestTailBytes = 4000

// RunTests runs `go test` on pkgs (default ./...) in dir with a timeout. A
// failing or timed-out run is a result, not an error; the error is for
// a cancelled context.
func (w *Work) RunTests(ctx context.Context, dir string, timeout time.Duration, pkgs ...string) (TestResult, error) {
	if len(pkgs) == 0 {
		pkgs = []string{"./..."}
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	out, err := w.Runner.Run(tctx, Cmd{Dir: dir, Name: "go", Args: append([]string{"test"}, pkgs...), Env: []string{"GOTOOLCHAIN=local"}})
	res := TestResult{Passed: err == nil, Duration: time.Since(start), Tail: tail(out, TestTailBytes)}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) || tctx.Err() != nil && err != nil {
		res.TimedOut = true
	}
	return res, nil
}

func tail(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func checkText(title, body string) error {
	switch {
	case strings.TrimSpace(title) == "" || strings.ContainsAny(title, "\n\r"):
		return errors.New("title must be one non-empty line")
	case len(title) > 200:
		return errors.New("title is over 200 bytes")
	case len(body) > 60000:
		return errors.New("body is over 60000 bytes")
	}
	return nil
}

// OpenDraftPR pushes branch from dir to origin and opens a draft PR into
// Base. Only work branches (BranchPrefix) can be pushed. Returns the PR URL.
func (w *Work) OpenDraftPR(ctx context.Context, dir, branch, title, body string) (string, error) {
	if err := checkText(title, body); err != nil {
		return "", err
	}
	if !strings.HasPrefix(branch, w.BranchPrefix) || !ValidSlug(strings.TrimPrefix(branch, w.BranchPrefix)) {
		return "", fmt.Errorf("branch %q is not a work branch", branch)
	}
	if _, err := w.run(ctx, dir, "git", "push", "-q", "-u", "origin", branch+":"+branch); err != nil {
		return "", err
	}
	out, err := w.run(ctx, dir, "gh", "pr", "create", "--draft", "--repo="+w.Repo,
		"--base="+w.Base, "--head="+branch, "--title="+title, "--body="+body)
	if err != nil {
		return "", err
	}
	return lastURL(out)
}

// OpenIssue opens an issue on Repo. Returns the issue URL.
func (w *Work) OpenIssue(ctx context.Context, title, body string) (string, error) {
	if err := checkText(title, body); err != nil {
		return "", err
	}
	out, err := w.run(ctx, "", "gh", "issue", "create", "--repo="+w.Repo, "--title="+title, "--body="+body)
	if err != nil {
		return "", err
	}
	return lastURL(out)
}

func lastURL(out string) (string, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "https://") {
			return l, nil
		}
	}
	return "", fmt.Errorf("gh printed no URL: %s", tail(out, 300))
}
