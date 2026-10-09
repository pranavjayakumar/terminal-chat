package snippet

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

func TestLooksLikeDiff(t *testing.T) {
	gitDiff := "diff --git a/x.go b/x.go\nindex 1..2 100644\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-a\n+b\n"
	plain := "--- a.txt\t2026-01-01\n+++ b.txt\t2026-01-02\n@@ -1,2 +1,2 @@\n a\n-b\n+c\n"
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"git diff", gitDiff, true},
		{"git diff CRLF", strings.ReplaceAll(gitDiff, "\n", "\r\n"), true},
		{"leading blank", "\n\n" + gitDiff, true},
		{"diff -u", plain, true},
		{"diff -u CRLF", strings.ReplaceAll(plain, "\n", "\r\n"), true},
		{"git show", "commit abc\nAuthor: x\n\n    msg\n\n" + gitDiff, true},
		{"empty", "", false},
		{"prose", "hello\nworld\n", false},
		{"markdown hr", "title\n--- \nmore\n+++ plus\nand text\n", false},
		{"headers without hunk", "--- a\n+++ b\nnothing\n", false},
		{"hunk without headers", "@@ -1 +1 @@\n-a\n+b\n", false},
		{"minus lines", "- a\n- b\n+ c\n", false},
		{"diff --git not first, no hunk", "text\ndiff --git a b\n", false},
		{"split headers", "--- a\nx\n+++ b\n@@ -1 +1 @@\n", false},
	}
	for _, c := range cases {
		if got := LooksLikeDiff(c.in); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	// Huge diffs stay linear and detected; a huge non-diff is rejected.
	big := gitDiff + strings.Repeat("+line\n-line\n context\n", 200000)
	if !LooksLikeDiff(big) {
		t.Error("huge diff not detected")
	}
	if LooksLikeDiff(strings.Repeat("just a log line\n", 300000)) {
		t.Error("huge log detected as diff")
	}
}

func TestLang(t *testing.T) {
	want := map[string]string{
		"a.go": "go", "a.py": "py", "a.js": "js", "a.ts": "ts", "a.rs": "rs", "a.sh": "sh",
		"README.md": "md", "a.json": "json", "a.yaml": "yaml", "a.yml": "yaml", "a.toml": "toml",
		"a.sql": "sql", "a.c": "c", "a.h": "h", "a.cpp": "cpp", "A.JAVA": "java", "a.rb": "rb",
		"notes.txt": "txt", "Makefile": "", "a.unknown": "", "dir.go/file": "", "x.tar.gz": "",
	}
	for p, w := range want {
		if got := Lang(p); got != w {
			t.Errorf("Lang(%q)=%q want %q", p, got, w)
		}
	}
}

func TestParseRef(t *testing.T) {
	ok := map[string]Ref{
		"a.go":        {Path: "a.go"},
		"a.go:40":     {Path: "a.go", Start: 40, End: 40, Single: true},
		"a.go:40-60":  {Path: "a.go", Start: 40, End: 60},
		"a.go:5-5":    {Path: "a.go", Start: 5, End: 5},
		"dir/a:b.go":  {Path: "dir/a:b.go"},
		"a:1:2":       {Path: "a:1", Start: 2, End: 2, Single: true},
		"weird:name-": {Path: "weird:name-"},
	}
	for in, want := range ok {
		got, err := ParseRef(in)
		if err != nil || got != want {
			t.Errorf("%q: got %+v, %v want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "a.go:60-40", "a.go:0", "a.go:0-3", "a.go:99999999999999999999"} {
		if _, err := ParseRef(in); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line%03d\n", i)
	}
	return b.String()
}

func build(t *testing.T, dir, ref string, ctx int) (string, error) {
	t.Helper()
	return Build(Options{Ref: ref, Context: ctx, Dir: dir})
}

func TestBuildSlicing(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", numbered(100))

	body, err := build(t, dir, "a.go:10-12", -1)
	if err != nil {
		t.Fatal(err)
	}
	if body != "`a.go:10-12`\n```go\nline010\nline011\nline012\n```" {
		t.Fatalf("range: %q", body)
	}
	body, _ = build(t, dir, "a.go:50", -1)
	if !strings.Contains(body, "`a.go:47-53`") || !strings.Contains(body, "line047\n") || !strings.Contains(body, "line053\n```") {
		t.Fatalf("single with default context: %q", body)
	}
	body, _ = build(t, dir, "a.go:50", 0)
	if body != "`a.go:50`\n```go\nline050\n```" {
		t.Fatalf("context 0: %q", body)
	}
	body, _ = build(t, dir, "a.go:2", 5) // clamped at line 1
	if !strings.Contains(body, "`a.go:1-7`") {
		t.Fatalf("clamp start: %q", body)
	}
	body, _ = build(t, dir, "a.go:99-500", -1) // end past EOF is clamped
	if !strings.Contains(body, "`a.go:99-100`") {
		t.Fatalf("clamp end: %q", body)
	}
	body, _ = build(t, dir, "a.go:99", -1)
	if !strings.Contains(body, "`a.go:96-100`") {
		t.Fatalf("single near EOF: %q", body)
	}
	body, _ = build(t, dir, "a.go", -1)
	if !strings.HasPrefix(body, "`a.go`\n```go\nline001\n") || !strings.HasSuffix(body, "line100\n```") {
		t.Fatalf("whole file: %q", body)
	}
	body, _ = Build(Options{Ref: "a.go:1", Context: 0, Dir: dir, Message: "look  "})
	if !strings.HasPrefix(body, "look\n`a.go:1`\n") {
		t.Fatalf("message above: %q", body)
	}

	for _, ref := range []string{"a.go:30-20", "a.go:101", "a.go:200-300", "missing.go", ".", "a.go:0"} {
		if b, err := build(t, dir, ref, -1); err == nil {
			t.Errorf("%s: want error, got %q", ref, b)
		}
	}
}

func TestBuildFileEdgeCases(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "bin.dat", "abc\x00def\n")
	if _, err := build(t, dir, "bin.dat", -1); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Errorf("binary: %v", err)
	}
	write(t, dir, "bad.txt", "ok\n\xff\xfe\n")
	if _, err := build(t, dir, "bad.txt", -1); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("invalid utf8: %v", err)
	}
	write(t, dir, "empty.txt", "")
	if _, err := build(t, dir, "empty.txt", -1); err == nil {
		t.Error("empty file should error")
	}

	// Size cap: whole file over the cap is refused; a range of a big file works.
	big := strings.Repeat("x", 99) + "\n"
	write(t, dir, "big.txt", strings.Repeat(big, MaxBytes/100+50))
	if _, err := build(t, dir, "big.txt", -1); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("big whole file: %v", err)
	}
	b, err := build(t, dir, "big.txt:5-6", -1)
	if err != nil || !strings.Contains(b, "`big.txt:5-6`") {
		t.Errorf("range of big file: %v", err)
	}
	if _, err := build(t, dir, "big.txt:1-100000", -1); err == nil {
		t.Error("oversized selection should error")
	}
	// Just under the cap fits in a message.
	write(t, dir, "fit.txt", strings.Repeat("y", MaxBytes-1))
	b, err = build(t, dir, "fit.txt", -1)
	if err != nil || len(b) > proto.MaxBody || !sanitize.Valid(b) {
		t.Errorf("max-size file: err=%v len=%d", err, len(b))
	}

	// Unicode, CRLF, tabs, control characters, fences inside content.
	write(t, dir, "u.md", "日本語 ✓\r\n\tindent\x1b[31m red\r\n```\r\nend")
	b, err = build(t, dir, "u.md", -1)
	if err != nil {
		t.Fatal(err)
	}
	if b != "`u.md`\n```md\n日本語 ✓\n\tindent[31m red\nʼʼʼ\nend\n```" {
		t.Errorf("unicode: %q", b)
	}
	if !sanitize.Valid(b) {
		t.Error("body not valid")
	}
	// No trailing newline on the last line.
	write(t, dir, "nonl.txt", "a\nb")
	if b, _ := build(t, dir, "nonl.txt:2", 0); b != "`nonl.txt:2`\n```txt\nb\n```" {
		t.Errorf("no trailing newline: %q", b)
	}
	// Header label can't break out of its backticks.
	write(t, dir, "we`ird.txt", "x\n")
	if b, _ := build(t, dir, "we`ird.txt", -1); !strings.HasPrefix(b, "`we'ird.txt`\n") {
		t.Errorf("label: %q", b)
	}
}

func TestParseRemote(t *testing.T) {
	good := map[string][2]string{
		"https://github.com/org/repo.git":           {"github.com", "org/repo"},
		"https://github.com/org/repo":               {"github.com", "org/repo"},
		"https://user:tok@github.com/org/repo.git":  {"github.com", "org/repo"},
		"git@github.com:org/repo.git":               {"github.com", "org/repo"},
		"ssh://git@github.com/org/repo.git":         {"github.com", "org/repo"},
		"https://gitlab.com/group/sub/repo.git":     {"gitlab.com", "group/sub/repo"},
		"git@gitlab.com:group/repo.git":             {"gitlab.com", "group/repo"},
		"https://GitHub.com/Org/Repo/":              {"github.com", "Org/Repo"},
		"ssh://git@ssh.github.com:443/org/repo.git": {"", ""},
		"https://example.com/org/repo.git":          {"", ""},
		"/local/path/repo.git":                      {"", ""},
		"https://github.com/onlyorg":                {"", ""},
		"https://github.com/org/repo/extra":         {"", ""},
		"https://evilgithub.com/org/repo":           {"", ""},
		"https://github.com.evil.com/org/repo":      {"", ""},
		"https://github.com/../repo":                {"", ""},
	}
	for in, w := range good {
		h, r, ok := parseRemote(in)
		if (w[0] != "") != ok || h != w[0] && ok || r != w[1] && ok {
			t.Errorf("%q: got %q %q %v want %v", in, h, r, ok, w)
		}
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitSHA(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestPermalink(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "remote", "add", "origin", "https://github.com/org/repo.git")
	write(t, repo, "pkg/a b.go", numbered(80))
	write(t, repo, "top.py", numbered(10))
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	sha := gitSHA(t, repo)

	b, err := Build(Options{Ref: "pkg/a b.go:40-60", Context: -1, Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	want := "`pkg/a b.go:40-60` https://github.com/org/repo/blob/" + sha + "/pkg/a%20b.go#L40-L60\n```go\n"
	if !strings.HasPrefix(b, want) {
		t.Fatalf("permalink header:\n got %q\nwant prefix %q", b, want)
	}
	// Single line, from a subdirectory (relative path resolved from Dir).
	b, _ = Build(Options{Ref: "a b.go:5", Context: 0, Dir: filepath.Join(repo, "pkg")})
	if !strings.Contains(b, "/blob/"+sha+"/pkg/a%20b.go#L5\n") {
		t.Fatalf("single line link: %q", b)
	}
	// Whole file: no line anchor.
	b, _ = Build(Options{Ref: "top.py", Context: -1, Dir: repo})
	if !strings.HasPrefix(b, "`top.py` https://github.com/org/repo/blob/"+sha+"/top.py\n```py\n") {
		t.Fatalf("whole file link: %q", b)
	}

	// Modified file: marked uncommitted, no link.
	write(t, repo, "top.py", numbered(10)+"extra\n")
	b, _ = Build(Options{Ref: "top.py:2", Context: 0, Dir: repo})
	if !strings.HasPrefix(b, "`top.py:2` (uncommitted)\n```py\n") || strings.Contains(b, "https://") {
		t.Fatalf("modified: %q", b)
	}
	// Untracked file likewise.
	write(t, repo, "new.go", "package x\n")
	b, _ = Build(Options{Ref: "new.go", Context: 0, Dir: repo})
	if !strings.HasPrefix(b, "`new.go` (uncommitted)\n") {
		t.Fatalf("untracked: %q", b)
	}

	// GitLab flavor, scp-style remote.
	gitRun(t, repo, "remote", "set-url", "origin", "git@gitlab.com:grp/sub/repo.git")
	b, _ = Build(Options{Ref: "pkg/a b.go:1-2", Context: 0, Dir: repo})
	if !strings.Contains(b, "https://gitlab.com/grp/sub/repo/-/blob/"+sha+"/pkg/a%20b.go#L1-2\n") {
		t.Fatalf("gitlab: %q", b)
	}

	// Other hosts and missing remotes: plain header.
	gitRun(t, repo, "remote", "set-url", "origin", "https://example.com/org/repo.git")
	b, _ = Build(Options{Ref: "pkg/a b.go:1-2", Context: 0, Dir: repo})
	if !strings.HasPrefix(b, "`pkg/a b.go:1-2`\n```go\n") {
		t.Fatalf("other host: %q", b)
	}
	gitRun(t, repo, "remote", "remove", "origin")
	b, _ = Build(Options{Ref: "pkg/a b.go:1-2", Context: 0, Dir: repo})
	if !strings.HasPrefix(b, "`pkg/a b.go:1-2`\n```go\n") {
		t.Fatalf("no remote: %q", b)
	}

	// Outside any repo.
	plain := t.TempDir()
	write(t, plain, "x.go", "package x\n")
	b, _ = Build(Options{Ref: "x.go", Context: -1, Dir: plain})
	if b != "`x.go`\n```go\npackage x\n```" {
		t.Fatalf("no repo: %q", b)
	}
}
