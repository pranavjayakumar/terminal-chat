package work

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls []Cmd
	// reply maps a command prefix ("gh pr create") to output and error.
	reply map[string]fakeReply
}

type fakeReply struct {
	out string
	err error
}

func (f *fakeRunner) Run(ctx context.Context, c Cmd) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
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
	var out []string
	for _, c := range f.calls {
		out = append(out, c.String())
	}
	return out
}

func TestOpenDraftPR(t *testing.T) {
	f := &fakeRunner{reply: map[string]fakeReply{
		"gh pr create": {out: "Warning: 1 uncommitted change\nhttps://github.com/pranavjayakumar/terminal-chat/pull/200\n"},
	}}
	w := New(t.TempDir())
	w.Runner = f
	url, err := w.OpenDraftPR(context.Background(), "/wt/poll", "agent/poll", "Add /poll", "-- body --")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/pranavjayakumar/terminal-chat/pull/200" {
		t.Errorf("url = %q", url)
	}
	want := []string{
		"git push -q -u origin agent/poll:agent/poll",
		"gh pr create --draft --repo=pranavjayakumar/terminal-chat --base=main --head=agent/poll --title=Add /poll --body=-- body --",
	}
	if strings.Join(f.lines(), "\n") != strings.Join(want, "\n") {
		t.Errorf("calls =\n%s\nwant\n%s", strings.Join(f.lines(), "\n"), strings.Join(want, "\n"))
	}
	for _, c := range f.calls {
		if c.Dir != "/wt/poll" {
			t.Errorf("%s ran in %q", c, c.Dir)
		}
	}
}

func TestOpenDraftPRRefuses(t *testing.T) {
	tests := []struct{ name, branch, title, body string }{
		{"main", "main", "t", ""},
		{"other prefix", "feature/x", "t", ""},
		{"bad slug", "agent/X Y", "t", ""},
		{"flag branch", "agent/--force", "t", ""},
		{"empty title", "agent/x", "  ", ""},
		{"multiline title", "agent/x", "a\nb", ""},
		{"long title", "agent/x", strings.Repeat("t", 201), ""},
		{"long body", "agent/x", "t", strings.Repeat("b", 60001)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRunner{}
			w := New(t.TempDir())
			w.Runner = f
			if _, err := w.OpenDraftPR(context.Background(), "/wt", tt.branch, tt.title, tt.body); err == nil {
				t.Error("want error")
			}
			if len(f.calls) != 0 {
				t.Errorf("ran %v", f.lines())
			}
		})
	}
}

func TestOpenDraftPRPushFails(t *testing.T) {
	f := &fakeRunner{reply: map[string]fakeReply{"git push": {out: "rejected", err: errors.New("exit 1")}}}
	w := New(t.TempDir())
	w.Runner = f
	if _, err := w.OpenDraftPR(context.Background(), "/wt", "agent/x", "t", "b"); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("err = %v", err)
	}
	if len(f.calls) != 1 {
		t.Error("PR opened after a failed push")
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
	f.reply["gh issue create"] = fakeReply{out: "something odd"}
	if _, err := w.OpenIssue(context.Background(), "t", "b"); err == nil {
		t.Error("no URL accepted")
	}
}

func TestWorktreeFake(t *testing.T) {
	agent := t.TempDir()
	f := &fakeRunner{reply: map[string]fakeReply{
		"git -C " + filepath.Join(agent, "repo") + " rev-parse": {err: errors.New("exit 1")},
	}}
	w := New(agent)
	w.Runner = f
	wt, err := w.Worktree(context.Background(), "poll-cmd")
	if err != nil {
		t.Fatal(err)
	}
	if wt.Dir != filepath.Join(agent, "worktrees", "poll-cmd") || wt.Branch != "agent/poll-cmd" {
		t.Errorf("wt = %+v", wt)
	}
	got := strings.Join(f.lines(), "\n")
	for _, want := range []string{
		"gh repo clone pranavjayakumar/terminal-chat " + filepath.Join(agent, "repo"),
		"fetch -q origin",
		"worktree add -q -b agent/poll-cmd " + wt.Dir + " origin/main",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("calls missing %q:\n%s", want, got)
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

func TestWorktreeRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, src, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("tc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "init")

	agent := filepath.Join(root, "agent")
	w := New(agent)
	w.CloneURL = src
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

func TestRunTests(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	dir := t.TempDir()
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/x\n\ngo 1.25\n")
	write("x_test.go", "package x\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n")
	w := New(t.TempDir())
	ctx := context.Background()
	res, err := w.RunTests(ctx, dir, 2*time.Minute)
	if err != nil || !res.Passed || res.TimedOut || !strings.Contains(res.Tail, "ok") {
		t.Fatalf("pass run = %+v, %v", res, err)
	}
	write("y_test.go", "package x\n\nimport \"testing\"\n\nfunc TestBad(t *testing.T) { t.Fatal(\"boom\") }\n")
	res, err = w.RunTests(ctx, dir, 2*time.Minute)
	if err != nil || res.Passed || !strings.Contains(res.Tail, "boom") {
		t.Fatalf("fail run = %+v, %v", res, err)
	}
}

type slowRunner struct{}

func (slowRunner) Run(ctx context.Context, c Cmd) (string, error) {
	<-ctx.Done()
	return "partial output\n", ctx.Err()
}

func TestRunTestsTimeoutAndCancel(t *testing.T) {
	w := New(t.TempDir())
	w.Runner = slowRunner{}
	res, err := w.RunTests(context.Background(), "/x", 20*time.Millisecond)
	if err != nil || res.Passed || !res.TimedOut || res.Tail != "partial output" {
		t.Errorf("timeout = %+v, %v", res, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.RunTests(ctx, "/x", time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel err = %v", err)
	}
}

func TestRunTestsEnvAndTail(t *testing.T) {
	long := strings.Repeat("line of output\n", 1000)
	f := &fakeRunner{reply: map[string]fakeReply{"go test": {out: long + "FAIL\n", err: errors.New("exit 1")}}}
	w := New(t.TempDir())
	w.Runner = f
	res, err := w.RunTests(context.Background(), "/wt", time.Minute, "./internal/x/...")
	if err != nil || res.Passed || res.TimedOut {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if len(res.Tail) > TestTailBytes || !strings.HasSuffix(res.Tail, "FAIL") || strings.HasPrefix(res.Tail, "ine") {
		t.Errorf("tail len %d", len(res.Tail))
	}
	c := f.calls[0]
	if c.String() != "go test ./internal/x/..." || c.Dir != "/wt" || strings.Join(c.Env, " ") != "GOTOOLCHAIN=local" {
		t.Errorf("cmd = %+v", c)
	}
}
