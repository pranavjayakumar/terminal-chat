package work

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/skailama/tc/internal/agent/safegit"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls []Cmd
	// reply maps a command prefix ("gh pr create") to output and error.
	reply map[string]fakeReply
	// real runs commands with these names for real (git in the real-git
	// tests).
	real map[string]bool
}

type fakeReply struct {
	out string
	err error
}

func (f *fakeRunner) Run(ctx context.Context, c Cmd) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	real := f.real[c.Name]
	f.mu.Unlock()
	if real {
		return ExecRunner{}.Run(ctx, c)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	line := c.String()
	best := ""
	for k := range f.reply {
		if strings.HasPrefix(line, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return "", nil
	}
	r := f.reply[best]
	return r.out, r.err
}

func (f *fakeRunner) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.String())
	}
	return out
}

func TestOpenDraftPRRefuses(t *testing.T) {
	sha := strings.Repeat("a", 40)
	tests := []struct {
		name        string
		pr          PR
		title, body string
	}{
		{"main", PR{Branch: "main", SHA: sha}, "t", ""},
		{"other prefix", PR{Branch: "feature/x", SHA: sha}, "t", ""},
		{"bad slug", PR{Branch: "agent/X Y", SHA: sha}, "t", ""},
		{"flag branch", PR{Branch: "agent/--force", SHA: sha}, "t", ""},
		{"empty title", PR{Branch: "agent/x", SHA: sha}, "  ", ""},
		{"multiline title", PR{Branch: "agent/x", SHA: sha}, "a\nb", ""},
		{"long title", PR{Branch: "agent/x", SHA: sha}, strings.Repeat("t", 201), ""},
		{"long body", PR{Branch: "agent/x", SHA: sha}, "t", strings.Repeat("b", 60001)},
		{"no sha", PR{Branch: "agent/x"}, "t", ""},
		{"ref not sha", PR{Branch: "agent/x", SHA: "HEAD"}, "t", ""},
		{"not a worktree", PR{Branch: "agent/x", SHA: sha, Dir: "/elsewhere/x"}, "t", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRunner{}
			w := New(t.TempDir())
			w.Runner = f
			if _, err := w.OpenDraftPR(context.Background(), tt.pr, tt.title, tt.body); err == nil {
				t.Error("want error")
			}
			if len(f.calls) != 0 {
				t.Errorf("ran %v", f.lines())
			}
		})
	}
}

func TestOpenIssue(t *testing.T) {
	f := &fakeRunner{reply: map[string]fakeReply{"gh issue create": {out: "https://github.com/o/r/issues/7\n"}}}
	w := New(t.TempDir())
	w.Runner = f
	w.Repo = "o/r"
	url, err := w.OpenIssue(context.Background(), "-bug", "steps")
	if err != nil || url != "https://github.com/o/r/issues/7" {
		t.Fatalf("url = %q, %v", url, err)
	}
	if got := f.lines()[0]; got != "gh issue create --repo=o/r --title=-bug --body=steps" {
		t.Errorf("call = %s", got)
	}
	// gh never runs inside a repo the agent can write: it would read its config.
	if c := f.calls[0]; !c.Creds || c.Dir == "" {
		t.Errorf("gh ran with creds=%v in %q", c.Creds, c.Dir)
	}
	f.reply["gh issue create"] = fakeReply{out: "something odd"}
	if _, err := w.OpenIssue(context.Background(), "t", "b"); err == nil {
		t.Error("no URL accepted")
	}
}

func TestWorktreeFake(t *testing.T) {
	agent := t.TempDir()
	w := New(agent)
	if err := os.MkdirAll(filepath.Join(w.RepoDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{reply: map[string]fakeReply{}}
	w.Runner = f
	f.reply["git --git-dir="+filepath.Join(w.RepoDir, ".git")+" "+strings.Join(safegit.Flags, " ")+" -c user.name=tc agent -c user.email=agent@tc.invalid rev-parse"] = fakeReply{err: errors.New("exit 1")}
	wt, err := w.Worktree(context.Background(), "poll-cmd")
	if err != nil {
		t.Fatal(err)
	}
	if wt.Dir != filepath.Join(agent, "worktrees", "poll-cmd") || wt.Branch != "agent/poll-cmd" {
		t.Errorf("wt = %+v", wt)
	}
	got := strings.Join(f.lines(), "\n")
	for _, want := range []string{
		"fetch -q --no-tags -- https://github.com/pranavjayakumar/terminal-chat.git +refs/heads/main:refs/remotes/origin/main",
		"worktree add -q -b agent/poll-cmd " + wt.Dir + " origin/main",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("calls missing %q:\n%s", want, got)
		}
	}
	for _, c := range f.calls {
		if c.Name != "git" {
			continue
		}
		a := strings.Join(c.Args, " ")
		if !strings.HasPrefix(a, "--git-dir=") || !strings.Contains(a, "-c core.hooksPath=/dev/null -c core.fsmonitor=false") {
			t.Errorf("git without pinned dir or safe flags: %s", a)
		}
		helper := strings.Contains(a, "credential.helper=!gh")
		if c.Creds != helper || helper != slices.Contains(c.Args, "fetch") {
			t.Errorf("creds=%v helper=%v for %s", c.Creds, helper, a)
		}
	}
	for _, bad := range []string{"", "Poll", "-x", "a/b", "../x", strings.Repeat("a", 41)} {
		if _, err := w.Worktree(context.Background(), bad); err == nil {
			t.Errorf("slug %q accepted", bad)
		}
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// realRepo makes a source repo with one commit on main and a Work cloning
// it, with git real and gh faked.
func realRepo(t *testing.T) (src string, w *Work, f *fakeRunner) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	src = filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, src, "init", "-q", "-b", "main")
	write(t, filepath.Join(src, "README.md"), "tc\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "init")
	w = New(filepath.Join(root, "agent"))
	w.CloneURL = src
	f = &fakeRunner{real: map[string]bool{"git": true}, reply: map[string]fakeReply{
		"gh pr create": {out: "https://github.com/o/r/pull/9\n"},
	}}
	w.Runner = f
	return src, w, f
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeRealGit(t *testing.T) {
	_, w, _ := realRepo(t)
	ctx := context.Background()
	wt, err := w.Worktree(ctx, "poll")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(wt.Dir, "README.md")); err != nil || string(b) != "tc\n" {
		t.Fatalf("worktree content = %q, %v", b, err)
	}
	if br := strings.TrimSpace(git(t, wt.Dir, "branch", "--show-current")); br != "agent/poll" {
		t.Errorf("branch = %q", br)
	}
	again, err := w.Worktree(ctx, "poll")
	if err != nil || again != wt {
		t.Errorf("second call = %+v, %v", again, err)
	}
	// A removed worktree whose branch survives comes back on that branch.
	git(t, w.RepoDir, "worktree", "remove", "--force", wt.Dir)
	if _, err := w.Worktree(ctx, "poll"); err != nil {
		t.Fatalf("recreate on existing branch: %v", err)
	}
	if br := strings.TrimSpace(git(t, wt.Dir, "branch", "--show-current")); br != "agent/poll" {
		t.Errorf("branch after recreate = %q", br)
	}
}

func TestPreparePRAndPush(t *testing.T) {
	src, w, f := realRepo(t)
	ctx := context.Background()
	wt, err := w.Worktree(ctx, "poll")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt.Dir, "poll.go"), "package tc\n")
	write(t, filepath.Join(wt.Dir, ".github", "workflows", "ci.yml"), "on: push\n")
	pr, err := w.PreparePR(ctx, wt.Dir, wt.Branch, "Add /poll")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pr.Files, []string{".github/workflows/ci.yml", "poll.go"}) || !slices.Equal(pr.CI, []string{".github/workflows/ci.yml"}) {
		t.Errorf("files %v, ci %v", pr.Files, pr.CI)
	}
	if !strings.Contains(pr.Stat, "poll.go") || !strings.Contains(pr.Stat, "2 files changed") {
		t.Errorf("stat = %q", pr.Stat)
	}
	if !strings.Contains(pr.Diff, "+package tc") || !strings.Contains(pr.Diff, "+on: push") {
		t.Errorf("diff = %q", pr.Diff)
	}
	if author := git(t, wt.Dir, "log", "-1", "--format=%an <%ae>|%s"); author != "tc agent <agent@tc.invalid>|Add /poll\n" {
		t.Errorf("commit = %q", author)
	}
	// What the agent changes after the card went up is not pushed.
	write(t, filepath.Join(wt.Dir, "later.go"), "package tc\n")
	url, err := w.OpenDraftPR(ctx, pr, "Add /poll", "body")
	if err != nil || url != "https://github.com/o/r/pull/9" {
		t.Fatalf("url %q, %v", url, err)
	}
	if got := strings.TrimSpace(git(t, src, "rev-parse", "refs/heads/agent/poll")); got != pr.SHA {
		t.Errorf("pushed %s, approved %s", got, pr.SHA)
	}
	last := f.calls[len(f.calls)-1]
	if last.Name != "gh" || !last.Creds || last.String() != "gh pr create --draft --repo=pranavjayakumar/terminal-chat --base=main --head=agent/poll --title=Add /poll --body=body" {
		t.Errorf("gh call = %+v", last)
	}
	for _, c := range f.calls {
		if c.Name == "git" && c.Creds != (slices.Contains(c.Args, "push") || slices.Contains(c.Args, "fetch") || slices.Contains(c.Args, "clone")) {
			t.Errorf("creds=%v for %s", c.Creds, c)
		}
	}

	// A branch with nothing new has nothing to push.
	other, err := w.Worktree(ctx, "empty")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.PreparePR(ctx, other.Dir, other.Branch, "nothing"); err == nil || !strings.Contains(err.Error(), "nothing to push") {
		t.Errorf("empty branch: %v", err)
	}
}

// The agent's sandbox can't write the repo's git dir, but if anything ever
// did, tc's own git must still not run what it planted.
func TestPoisonedGitMetadataDoesNotRun(t *testing.T) {
	_, w, _ := realRepo(t)
	ctx := context.Background()
	wt, err := w.Worktree(ctx, "poll")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "pwned")
	evil := filepath.Join(t.TempDir(), "evil.sh")
	write(t, evil, "#!/bin/sh\necho ran >> "+marker+"\n")
	gitDir := filepath.Join(w.RepoDir, ".git")
	cfg, err := os.OpenFile(filepath.Join(gitDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = cfg.WriteString("[core]\n\tfsmonitor = " + evil + "\n[credential]\n\thelper = !" + evil + "\n")
	cfg.Close()
	for _, h := range []string{"pre-commit", "commit-msg", "post-commit", "pre-push", "post-checkout", "reference-transaction"} {
		write(t, filepath.Join(gitDir, "hooks", h), "#!/bin/sh\necho ran >> "+marker+"\n")
	}
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor'='"+evil+"'")
	t.Setenv("GIT_DIR", "/nonexistent")
	write(t, filepath.Join(wt.Dir, "x.go"), "package tc\n")
	pr, err := w.PreparePR(ctx, wt.Dir, wt.Branch, "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.OpenDraftPR(ctx, pr, "x", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Worktree(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("planted code ran: %s", b)
	}
}

func TestRewrittenWorktreeIsRefused(t *testing.T) {
	_, w, _ := realRepo(t)
	ctx := context.Background()
	wt, err := w.Worktree(ctx, "poll")
	if err != nil {
		t.Fatal(err)
	}
	ptr := filepath.Join(wt.Dir, ".git")
	orig, err := os.ReadFile(ptr)
	if err != nil {
		t.Fatal(err)
	}
	evilDir := filepath.Join(t.TempDir(), "evil.git")
	git(t, filepath.Dir(evilDir), "init", "-q", "--bare", evilDir)
	for name, content := range map[string]string{
		"elsewhere":    "gitdir: " + evilDir + "\n",
		"relative out": "gitdir: ../../../../evil.git\n",
		"common dir":   "gitdir: " + filepath.Join(w.RepoDir, ".git") + "\n",
		"garbage":      "nope",
	} {
		write(t, ptr, content)
		if _, err := w.PreparePR(ctx, wt.Dir, wt.Branch, "x"); !errors.Is(err, safegit.ErrBadWorktree) {
			t.Errorf("%s: err = %v", name, err)
		}
		if _, err := w.Worktree(ctx, "poll"); !errors.Is(err, safegit.ErrBadWorktree) {
			t.Errorf("%s: Worktree err = %v", name, err)
		}
	}
	write(t, ptr, string(orig))

	// A .git that is a symlink is refused too.
	if err := os.Remove(ptr); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evilDir, ptr); err != nil {
		t.Fatal(err)
	}
	if _, err := w.PreparePR(ctx, wt.Dir, wt.Branch, "x"); !errors.Is(err, safegit.ErrBadWorktree) {
		t.Errorf("symlinked .git: %v", err)
	}
	_ = os.Remove(ptr)
	write(t, ptr, string(orig))

	// A worktree swapped for a symlink to somewhere else is refused.
	moved := wt.Dir + "-real"
	if err := os.Rename(wt.Dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, wt.Dir); err != nil {
		t.Fatal(err)
	}
	if _, err := w.PreparePR(ctx, wt.Dir, wt.Branch, "x"); !errors.Is(err, safegit.ErrBadWorktree) {
		t.Errorf("symlinked worktree: %v", err)
	}

	// A dir that isn't under the worktrees folder is never used.
	if _, err := w.PreparePR(ctx, w.RepoDir, wt.Branch, "x"); err == nil {
		t.Error("repo dir accepted as a worktree")
	}
}

// Prefetch reads only go.mod (no go.work, no env file), asks only the
// public proxy (no VCS), into the cache only tc writes.
func TestPrefetchEnv(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module x\n")
	f := &fakeRunner{}
	w := New(t.TempDir())
	w.Runner = f
	if err := w.Prefetch(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].String() != "go mod download" || f.calls[0].Creds {
		t.Fatalf("calls %+v", f.calls)
	}
	env := strings.Join(f.calls[0].Env, " ") + " "
	for _, want := range []string{"GOMODCACHE=" + w.ModProxy + " ", "GOWORK=off", "GOENV=off", "GOVCS=*:off", "GOPROXY=https://proxy.golang.org ", "GOPRIVATE= ", "GONOSUMDB= "} {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %q: %s", want, env)
		}
	}
}

func TestPreparePRRefusesLinksAndNestedRepos(t *testing.T) {
	_, w, _ := realRepo(t)
	ctx := context.Background()
	wt, err := w.Worktree(ctx, "poll")
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(filepath.Dir(w.RepoDir), "secret")
	write(t, secret, "owner's key\n")
	if err := os.Link(secret, filepath.Join(wt.Dir, "key.txt")); err != nil {
		t.Skip("no hard links here:", err)
	}
	if _, err := w.PreparePR(ctx, wt.Dir, wt.Branch, "x"); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Errorf("hard link: %v", err)
	}
	_ = os.Remove(filepath.Join(wt.Dir, "key.txt"))

	nested := filepath.Join(wt.Dir, "sub")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, nested, "init", "-q")
	if _, err := w.PreparePR(ctx, wt.Dir, wt.Branch, "x"); err == nil || !strings.Contains(err.Error(), "repository inside") {
		t.Errorf("nested repo: %v", err)
	}
}
