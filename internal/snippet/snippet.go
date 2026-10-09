package snippet

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

const (
	// MaxBytes caps the sliced file content: 64 KiB less room for the
	// wrapper so the whole message always fits proto.MaxBody.
	MaxBytes = proto.MaxBody - 1024
	// DefaultContext is the number of lines around a single-line reference.
	DefaultContext = 3
	// scanLimit bounds how much of a big file is read to find a range.
	scanLimit  = 16 << 20
	gitTimeout = 5 * time.Second
)

var langs = map[string]string{
	"go": "go", "py": "py", "js": "js", "ts": "ts", "rs": "rs", "sh": "sh", "md": "md",
	"json": "json", "yaml": "yaml", "yml": "yaml", "toml": "toml", "sql": "sql",
	"c": "c", "h": "h", "cpp": "cpp", "cc": "cpp", "cxx": "cpp", "hpp": "cpp",
	"java": "java", "rb": "rb", "txt": "txt", "diff": "diff", "patch": "diff",
}

// Lang is the fence language for a file name, or "" when unknown.
func Lang(path string) string {
	return langs[strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))]
}

var refRE = regexp.MustCompile(`^(.+):(\d+)(?:-(\d+))?$`)

// Ref is a parsed "path[:start[-end]]". Start 0 means the whole file;
// Single marks "path:N" (a line, shown with context).
type Ref struct {
	Path       string
	Start, End int
	Single     bool
}

// ParseRef splits a path reference. A suffix that isn't ":N" or ":N-M"
// belongs to the path.
func ParseRef(s string) (Ref, error) {
	if s == "" {
		return Ref{}, errors.New("empty path")
	}
	mm := refRE.FindStringSubmatch(s)
	if mm == nil {
		return Ref{Path: s}, nil
	}
	start, err1 := strconv.Atoi(mm[2])
	end := 0
	var err2 error
	if mm[3] != "" {
		end, err2 = strconv.Atoi(mm[3])
	}
	if err1 != nil || err2 != nil {
		return Ref{}, fmt.Errorf("bad line range in %q", s)
	}
	if start < 1 || (mm[3] != "" && end < 1) {
		return Ref{}, errors.New("line numbers start at 1")
	}
	if mm[3] != "" && start > end {
		return Ref{}, fmt.Errorf("bad range %d-%d: start is after end", start, end)
	}
	r := Ref{Path: mm[1], Start: start, End: end}
	if mm[3] == "" {
		r.Single, r.End = true, start
	}
	return r, nil
}

// Options configures Build.
type Options struct {
	Ref     string // path[:start[-end]]
	Context int    // lines around a single line; <0: DefaultContext
	Message string // optional text placed above the snippet
	Dir     string // base for relative paths; "" is the working directory
}

// Build reads the file and returns the message body: optional text, a header
// line (path:range plus a permalink when possible) and a fenced block.
func Build(o Options) (string, error) {
	ref, err := ParseRef(o.Ref)
	if err != nil {
		return "", err
	}
	ctxN := o.Context
	if ctxN < 0 {
		ctxN = DefaultContext
	}
	path := ref.Path
	if !filepath.IsAbs(path) && o.Dir != "" {
		path = filepath.Join(o.Dir, path)
	}
	lines, first, err := readLines(path, ref, ctxN)
	if err != nil {
		return "", err
	}
	last := first + len(lines) - 1

	content := cleanLines(lines)
	content = strings.ReplaceAll(content, "```", "ʼʼʼ")

	label := sanitize.Line(ref.Path)
	label = strings.ReplaceAll(label, "`", "'")
	anchor := ""
	if ref.Start != 0 {
		anchor = fmt.Sprintf(":%d", first)
		if last != first {
			anchor = fmt.Sprintf(":%d-%d", first, last)
		}
	}
	header := "`" + label + anchor + "`"
	if link, uncommitted := permalink(path, first, last, ref.Start != 0); uncommitted {
		header += " (uncommitted)"
	} else if link != "" {
		header += " " + link
	}

	var b strings.Builder
	if m := strings.TrimSpace(o.Message); m != "" {
		b.WriteString(m + "\n")
	}
	b.WriteString(header + "\n```" + Lang(ref.Path) + "\n" + content + "\n```")
	if b.Len() > proto.MaxBody {
		return "", errors.New("message too large; pick a smaller line range")
	}
	return b.String(), nil
}

// cleanLines joins lines, dropping terminal control characters but keeping
// tabs (the sanitizer would widen them to spaces).
func cleanLines(lines []string) string {
	parts := strings.Split(strings.Join(lines, "\n"), "\t")
	for i, p := range parts {
		parts[i] = sanitize.Text(p)
	}
	return strings.Join(parts, "\t")
}

// readLines returns the requested lines (CR stripped) and the 1-based number
// of the first one.
func readLines(path string, ref Ref, ctxN int) ([]string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		if u := errors.Unwrap(err); u != nil {
			err = u
		}
		return nil, 0, fmt.Errorf("reading %s: %w", sanitize.Line(ref.Path), err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	name := sanitize.Line(ref.Path)
	if !st.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", name)
	}
	if ref.Start == 0 && st.Size() > MaxBytes {
		return nil, 0, fmt.Errorf("%s is %d bytes; the limit is %d. Give a line range (path:start-end)", name, st.Size(), MaxBytes)
	}
	br := bufio.NewReaderSize(io.LimitReader(f, scanLimit), 64<<10)
	head, _ := br.Peek(8192)
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, 0, fmt.Errorf("%s looks like a binary file", name)
	}

	lo, hi := 1, int(^uint(0)>>1)
	if ref.Start != 0 {
		lo, hi = ref.Start, ref.End
		if ref.Single {
			lo, hi = max(ref.Start-ctxN, 1), ref.Start+ctxN
		}
	}
	var out []string
	size, n := 0, 0
	for {
		ln, err := br.ReadString('\n')
		if ln != "" {
			n++
			if n >= lo && n <= hi {
				ln = strings.TrimSuffix(strings.TrimSuffix(ln, "\n"), "\r")
				if size += len(ln) + 1; size > MaxBytes {
					return nil, 0, fmt.Errorf("selection is over %d bytes; pick a smaller line range", MaxBytes)
				}
				out = append(out, ln)
			}
		}
		if err != nil || n >= hi {
			break
		}
	}
	if n == 0 {
		return nil, 0, fmt.Errorf("%s is empty", name)
	}
	if len(out) == 0 || n < ref.Start {
		return nil, 0, fmt.Errorf("%s has only %d lines (asked for line %d)", name, n, ref.Start)
	}
	for _, l := range out {
		if !utf8.ValidString(l) {
			return nil, 0, fmt.Errorf("%s isn't valid UTF-8 text", name)
		}
	}
	return out, lo, nil
}

// permalink returns a GitHub/GitLab URL pinned to HEAD for the file's lines.
// uncommitted is true (and link empty) when the repo qualifies but the file
// is untracked or modified, so a link would show different content. Any git
// failure or a non-GitHub/GitLab origin yields ("", false). git is run with
// an argument vector, never a shell.
func permalink(path string, first, last int, ranged bool) (link string, uncommitted bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	dir, base := filepath.Dir(abs), filepath.Base(abs)
	prefix, ok := git(dir, "rev-parse", "--show-prefix")
	if !ok {
		return "", false
	}
	remote, ok := git(dir, "remote", "get-url", "origin")
	if !ok {
		return "", false
	}
	host, repo, ok := parseRemote(remote)
	if !ok {
		return "", false
	}
	sha, ok := git(dir, "rev-parse", "HEAD")
	if !ok || len(sha) < 7 {
		return "", false
	}
	if _, tracked := git(dir, "ls-files", "--error-unmatch", "--", base); !tracked {
		return "", true
	}
	if out, ok := git(dir, "status", "--porcelain", "--", base); !ok || out != "" {
		return "", true
	}
	segs := strings.Split(prefix+base, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	u := "https://" + host + "/" + repo
	if host == "gitlab.com" {
		u += "/-"
	}
	u += "/blob/" + sha + "/" + strings.Join(segs, "/")
	if ranged {
		switch {
		case first == last:
			u += fmt.Sprintf("#L%d", first)
		case host == "gitlab.com":
			u += fmt.Sprintf("#L%d-%d", first, last)
		default:
			u += fmt.Sprintf("#L%d-L%d", first, last)
		}
	}
	return u, false
}

func git(dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// parseRemote extracts host and "org/repo" from an origin URL on GitHub or
// GitLab (https, ssh:// or scp-style). Credentials in the URL are dropped.
func parseRemote(raw string) (host, repo string, ok bool) {
	raw = strings.TrimSpace(raw)
	var path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", false
		}
		host, path = u.Hostname(), u.Path
	} else { // git@github.com:org/repo.git
		rest := raw[strings.LastIndexByte(raw, '@')+1:]
		h, p, found := strings.Cut(rest, ":")
		if !found {
			return "", "", false
		}
		host, path = h, p
	}
	host = strings.ToLower(host)
	if host != "github.com" && host != "gitlab.com" {
		return "", "", false
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || (host == "github.com" && len(parts) != 2) {
		return "", "", false
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", "", false
		}
	}
	return host, path, true
}
