// Package work runs an agent's code work on the tc repo: a clone in the agent
// home, one git worktree per piece of work, draft PRs and issues through the
// owner's gh account.
//
// The agent edits its worktree from a sandboxed shell; everything here runs
// outside the sandbox as the owner. So every git call goes through
// internal/agent/safegit (no hooks, fsmonitor or credential helpers from
// config, git dirs pinned, a worktree's .git pointer checked before use),
// only the steps that talk to GitHub as the owner see gh's credentials, and
// a push sends the exact commit the owner approved.
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
	"slices"
	"strings"
	"time"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/agent/safegit"
)

// DefaultRepo is the GitHub repo agents work on.
const DefaultRepo = "pranavjayakumar/terminal-chat"

// Cmd is one external command.
type Cmd struct {
	Dir  string
	Name string
	Args []string
	Env  []string // added to the scrubbed environment
	// Creds lets the command use the owner's GitHub login (gh's tokens).
	// Without it the command gets no credentials at all.
	Creds bool
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
	cmd.Env = append(safegit.Env(os.Environ(), c.Creds), c.Env...)
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
	CloneURL     string // clone, fetch and push here instead of https://github.com/Repo
	Base         string // branch work starts from and PRs target
	BranchPrefix string // every work branch starts with this
	// Author is the name and email on the commits tc makes for the agent.
	AuthorName, AuthorEmail string
	// ModProxy is the module cache Prefetch fills and the sandbox reads as
	// its GOPROXY; only tc writes it.
	ModProxy string
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
		AuthorName:   "tc agent",
		AuthorEmail:  "agent@tc.invalid",
		ModProxy:     p.ModProxy,
	}
}

// Worktree is a checkout for one piece of work.
type Worktree struct {
	Dir    string
	Branch string
}

var (
	slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	shaRe  = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
)

// ValidSlug reports whether slug can name a worktree and branch.
func ValidSlug(slug string) bool { return slugRe.MatchString(slug) }

func (w *Work) url() string {
	if w.CloneURL != "" {
		return w.CloneURL
	}
	return "https://github.com/" + w.Repo + ".git"
}

func (w *Work) exec(ctx context.Context, c Cmd) (string, error) {
	out, err := w.Runner.Run(ctx, c)
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", c.Name, subcommand(c), err, tail(out, 500))
	}
	return out, nil
}

// git runs git on the repo or a worktree: gitDir and workTree pinned
// (workTree "" for repo-wide commands), safegit's flags, and gh's credential
// helper only when creds.
func (w *Work) git(ctx context.Context, gitDir, workTree string, creds bool, args ...string) (string, error) {
	a := []string{"--git-dir=" + gitDir}
	if workTree != "" {
		a = append(a, "--work-tree="+workTree)
	}
	a = append(a, safegit.Flags...)
	if creds {
		a = append(a, safegit.GHCredential...)
	}
	a = append(a, "-c", "user.name="+w.AuthorName, "-c", "user.email="+w.AuthorEmail)
	dir := workTree
	if dir == "" {
		dir = filepath.Dir(gitDir)
	}
	return w.exec(ctx, Cmd{Dir: dir, Name: "git", Args: append(a, args...), Creds: creds})
}

// subcommand is the git or gh subcommand in c, for errors.
func subcommand(c Cmd) string {
	for _, a := range c.Args {
		if a == "-c" || strings.HasPrefix(a, "-") || strings.Contains(a, "=") {
			continue
		}
		return a
	}
	return ""
}

// EnsureClone clones the repo into RepoDir when it is not there yet.
func (w *Work) EnsureClone(ctx context.Context) error {
	if _, err := os.Lstat(filepath.Join(w.RepoDir, ".git")); err == nil {
		_, err := safegit.RepoGitDir(w.RepoDir)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(w.RepoDir), 0o700); err != nil {
		return fmt.Errorf("creating repo parent: %w", err)
	}
	a := append(append(append([]string{}, safegit.Flags...), safegit.GHCredential...), "clone", "-q", "--no-tags", "--template=", "--", w.url(), w.RepoDir)
	_, err := w.exec(ctx, Cmd{Dir: filepath.Dir(w.RepoDir), Name: "git", Args: a, Creds: true})
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
	gitDir, err := safegit.RepoGitDir(w.RepoDir)
	if err != nil {
		return Worktree{}, err
	}
	if _, err := os.Lstat(filepath.Join(wt.Dir, ".git")); err == nil {
		if _, err := safegit.WorktreeGitDir(w.RepoDir, wt.Dir); err != nil {
			return Worktree{}, err
		}
		return wt, nil
	}
	if strings.HasPrefix(w.Base, "-") {
		return Worktree{}, fmt.Errorf("bad base branch %q", w.Base)
	}
	if _, err := w.git(ctx, gitDir, "", true, "fetch", "-q", "--no-tags", "--", w.url(), "+refs/heads/"+w.Base+":refs/remotes/origin/"+w.Base); err != nil {
		return Worktree{}, err
	}
	if err := os.MkdirAll(w.WorktreesDir, 0o700); err != nil {
		return Worktree{}, fmt.Errorf("creating worktrees dir: %w", err)
	}
	_, _ = w.git(ctx, gitDir, "", false, "worktree", "prune")
	if _, err := w.git(ctx, gitDir, "", false, "rev-parse", "--verify", "--quiet", "refs/heads/"+wt.Branch); err == nil {
		// An existing branch holds what the agent wrote: no prefetch.
		_, err = w.git(ctx, gitDir, "", false, "worktree", "add", "-q", wt.Dir, wt.Branch)
		return wt, err
	}
	if _, err := w.git(ctx, gitDir, "", false, "worktree", "add", "-q", "-b", wt.Branch, wt.Dir, "origin/"+w.Base); err != nil {
		return wt, err
	}
	_ = w.Prefetch(ctx, wt.Dir) // best effort: a missing module fails the agent's build, not the worktree
	return wt, nil
}

// Prefetch downloads the Go modules of a worktree fresh from Base into
// ModProxy, from outside the sandbox (which has no internet). Only go.mod
// at Base is read (no go.work, no env file), only proxy.golang.org is
// asked (no VCS, no private-module bypass), and the cache is one the
// sandbox can't write, so it can't plant links for tc's go to follow.
// Downloading runs no code, and nothing outside the sandbox builds from it.
func (w *Work) Prefetch(ctx context.Context, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil || w.ModProxy == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	_, err := w.exec(ctx, Cmd{Dir: dir, Name: "go", Args: []string{"mod", "download"}, Env: []string{
		"GOTOOLCHAIN=local", "GOMODCACHE=" + w.ModProxy, "GOWORK=off", "GOENV=off", "GOFLAGS=-mod=mod",
		"GOPROXY=https://proxy.golang.org", "GOVCS=*:off", "GOSUMDB=sum.golang.org",
		"GOPRIVATE=", "GONOPROXY=", "GONOSUMDB=", "GOINSECURE=", "GOAUTH=off"}})
	return err
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

func (w *Work) checkBranch(branch string) error {
	if !strings.HasPrefix(branch, w.BranchPrefix) || !ValidSlug(strings.TrimPrefix(branch, w.BranchPrefix)) {
		return fmt.Errorf("branch %q is not a work branch", branch)
	}
	return nil
}

// worktreeGitDir checks that dir is a worktree of ours on branch and
// returns its git dir.
func (w *Work) worktreeGitDir(ctx context.Context, dir, branch string) (string, error) {
	if filepath.Dir(filepath.Clean(dir)) != filepath.Clean(w.WorktreesDir) {
		return "", fmt.Errorf("%s is not one of the agent's worktrees", dir)
	}
	gitDir, err := safegit.WorktreeGitDir(w.RepoDir, dir)
	if err != nil {
		return "", err
	}
	head, err := w.git(ctx, gitDir, dir, false, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(head) != "refs/heads/"+branch {
		return "", fmt.Errorf("the worktree is on %s, not %s", strings.TrimSpace(head), branch)
	}
	return gitDir, nil
}

// PR is a branch ready to push: the exact commit, what it changes against
// the base, and whether it touches CI.
type PR struct {
	Dir, Branch string
	SHA         string // the commit that will be pushed
	BaseSHA     string // merge base with origin/Base
	Stat        string // git diff --stat BaseSHA..SHA
	Diff        string // the whole diff BaseSHA..SHA
	Files       []string
	CI          []string // changed files under .github/
}

// MaxPRDiff is the largest diff a PR card carries; a bigger change is
// refused rather than shown in part.
const MaxPRDiff = 256 << 10

// checkStaged makes sure what `git add` staged is what the worktree holds
// now, through no hard link: a sandboxed process running in the background
// could swap a link to a secret in and out around the add.
func (w *Work) checkStaged(ctx context.Context, gitDir, dir string) error {
	ls, err := w.git(ctx, gitDir, dir, false, "ls-files", "-s", "-z")
	if err != nil {
		return err
	}
	for _, rec := range strings.Split(ls, "\x00") {
		meta, rel, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta) // mode, blob, stage
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if len(f) < 2 || f[0] == "120000" || f[0] == "160000" {
			continue // symlinks are stored as links; gitlinks were refused before
		}
		if safegit.Linked(path) {
			return fmt.Errorf("%s is a hard link to a file elsewhere: refusing to commit it", rel)
		}
		got, err := w.git(ctx, gitDir, dir, false, "hash-object", "--no-filters", "--", rel)
		if err != nil {
			return err
		}
		if strings.TrimSpace(got) != f[1] {
			return fmt.Errorf("%s changed while tc was committing it: try again once nothing is writing there", rel)
		}
	}
	return nil
}

// PreparePR commits whatever the agent left uncommitted in the worktree
// (its sandbox cannot write the repo's git dir, so tc commits for it, with
// message as the commit message) and describes the result for the owner's
// approval card.
func (w *Work) PreparePR(ctx context.Context, dir, branch, message string) (PR, error) {
	if err := w.checkBranch(branch); err != nil {
		return PR{}, err
	}
	if err := checkText(message, ""); err != nil {
		return PR{}, err
	}
	gitDir, err := w.worktreeGitDir(ctx, dir, branch)
	if err != nil {
		return PR{}, err
	}
	g := func(args ...string) (string, error) { return w.git(ctx, gitDir, dir, false, args...) }
	st, err := g("status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=all")
	if err != nil {
		return PR{}, err
	}
	for _, rec := range strings.Split(st, "\x00") {
		if len(rec) <= 3 {
			continue
		}
		rel := rec[3:]
		switch {
		case strings.HasSuffix(rel, "/") || slices.Contains(strings.Split(rel, "/"), ".git"):
			return PR{}, fmt.Errorf("%s is a git repository inside the worktree: refusing to commit it", rel)
		case safegit.Linked(filepath.Join(dir, filepath.FromSlash(rel))):
			return PR{}, fmt.Errorf("%s is a hard link to a file elsewhere: refusing to commit it", rel)
		}
	}
	if _, err := g("add", "-A"); err != nil {
		return PR{}, err
	}
	if err := w.checkStaged(ctx, gitDir, dir); err != nil {
		return PR{}, err
	}
	if _, err := g("diff", "--cached", "--quiet"); err != nil {
		if _, err := g("commit", "-q", "--no-verify", "-m", message); err != nil {
			return PR{}, err
		}
	}
	sha, err := g("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return PR{}, err
	}
	base, err := g("merge-base", "HEAD", "refs/remotes/origin/"+w.Base)
	if err != nil {
		return PR{}, err
	}
	pr := PR{Dir: dir, Branch: branch, SHA: strings.TrimSpace(sha), BaseSHA: strings.TrimSpace(base)}
	if pr.SHA == pr.BaseSHA {
		return PR{}, errors.New("nothing to push: the branch has no changes")
	}
	if pr.Stat, err = g("diff", "--no-color", "--no-ext-diff", "--no-textconv", "--stat=100", pr.BaseSHA, pr.SHA); err != nil {
		return PR{}, err
	}
	if pr.Diff, err = g("diff", "--no-color", "--no-ext-diff", "--no-textconv", "--full-index", pr.BaseSHA, pr.SHA); err != nil {
		return PR{}, err
	}
	if len(pr.Diff) > MaxPRDiff {
		return PR{}, fmt.Errorf("the change is %d bytes of diff, over the %d your owner can review on a card: split it", len(pr.Diff), MaxPRDiff)
	}
	names, err := g("diff", "--no-ext-diff", "--no-textconv", "--name-only", "-z", pr.BaseSHA, pr.SHA)
	if err != nil {
		return PR{}, err
	}
	for _, f := range strings.Split(names, "\x00") {
		if f == "" {
			continue
		}
		pr.Files = append(pr.Files, f)
		if f == ".github" || strings.HasPrefix(f, ".github/") {
			pr.CI = append(pr.CI, f)
		}
	}
	return pr, nil
}

// OpenDraftPR pushes exactly pr.SHA to pr.Branch on GitHub and opens a
// draft PR into Base. Returns the PR URL.
func (w *Work) OpenDraftPR(ctx context.Context, pr PR, title, body string) (string, error) {
	if err := checkText(title, body); err != nil {
		return "", err
	}
	if err := w.checkBranch(pr.Branch); err != nil {
		return "", err
	}
	if !shaRe.MatchString(pr.SHA) {
		return "", fmt.Errorf("bad commit %q", pr.SHA)
	}
	gitDir, err := w.worktreeGitDir(ctx, pr.Dir, pr.Branch)
	if err != nil {
		return "", err
	}
	if _, err := w.git(ctx, gitDir, pr.Dir, true, "push", "-q", "--no-verify", "--", w.url(), pr.SHA+":refs/heads/"+pr.Branch); err != nil {
		return "", err
	}
	out, err := w.gh(ctx, "pr", "create", "--draft", "--repo="+w.Repo,
		"--base="+w.Base, "--head="+pr.Branch, "--title="+title, "--body="+body)
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
	out, err := w.gh(ctx, "issue", "create", "--repo="+w.Repo, "--title="+title, "--body="+body)
	if err != nil {
		return "", err
	}
	return lastURL(out)
}

// gh runs gh as the owner in an empty folder of its own, so neither gh nor
// a git it starts finds a repository the agent could have written.
func (w *Work) gh(ctx context.Context, args ...string) (string, error) {
	dir, err := os.MkdirTemp("", "tc-gh-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	return w.exec(ctx, Cmd{Dir: dir, Name: "gh", Creds: true, Args: args, Env: []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir)}})
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
