package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

const sampleDiff = "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,3 +1,3 @@\n ctx\n-old\n+new"

func bodyOf(color bool, text string, bw int) []string {
	r := renderer{st: testStyles(color), me: proto.User{Name: "alice"}}
	return r.body(client.Message{Message: proto.Message{ID: 1, Author: 1, Body: text}}, bw)
}

func TestDiffRendering(t *testing.T) {
	for _, body := range []string{"```diff\n" + sampleDiff + "\n```", "```\n" + sampleDiff + "\n```"} {
		lines := bodyOf(true, body, 60)
		if len(lines) != 7 {
			t.Fatalf("want 7 lines, got %d: %q", len(lines), lines)
		}
		add, del, hunk, hdr := testStyles(true).diffAdd, testStyles(true).diffDel, testStyles(true).diffHunk, testStyles(true).diffHdr
		want := []struct {
			i    int
			text string
			sty  func(...string) string
		}{
			{0, "diff --git a/x.go b/x.go", hdr.Render},
			{1, "--- a/x.go", hdr.Render},
			{2, "+++ b/x.go", hdr.Render},
			{3, "@@ -1,3 +1,3 @@", hunk.Render},
			{5, "-old", del.Render},
			{6, "+new", add.Render},
		}
		for _, w := range want {
			if !strings.Contains(lines[w.i], w.sty(w.text)) {
				t.Errorf("line %d = %q, want styled %q", w.i, lines[w.i], w.sty(w.text))
			}
		}
		if !strings.Contains(lines[5], "\x1b[") || strings.Contains(lines[4], "\x1b[31m") {
			t.Errorf("styling: %q / %q", lines[4], lines[5])
		}
		if lines[5] == lines[6] {
			t.Error("add and del styled the same")
		}
	}

	// Not a diff: plain code styling, and an unlabeled non-diff stays plain.
	st := testStyles(true)
	plain := bodyOf(true, "```\n-not diff\n+really\n```", 60)
	if !strings.HasSuffix(plain[0], st.code.Render("-not diff")) {
		t.Errorf("non-diff colored as diff: %q", plain[0])
	}
	// A labeled "go" block holding diff-looking text is not a diff.
	g := bodyOf(true, "```go\n"+sampleDiff+"\n```", 60)
	if !strings.Contains(g[5], st.code.Render("-old")) {
		t.Errorf("go block treated as diff: %q", g[5])
	}
	// Hand-written ```diff with no headers: +/- still colored, not headers.
	h := bodyOf(true, "```diff\n-a\n+b\n```", 40)
	if !strings.Contains(h[0], st.diffDel.Render("-a")) || !strings.HasSuffix(h[1], st.diffAdd.Render("+b")) {
		t.Errorf("bare diff: %q", h)
	}
	// A deletion of a line starting with "-- " inside a hunk is a deletion.
	k := classifyDiff(strings.Split("@@ -1 +1 @@\n--- sql comment\n+x", "\n"))
	if k[1] != dDel || k[2] != dAdd {
		t.Errorf("--- inside hunk: %v", k)
	}
	// CRLF inside a diff block.
	c := classifyDiff(strings.Split("diff --git a b\r\n@@ -1 +1 @@\r\n-a\r\n+b\r\n", "\n"))
	if c[0] != dHdr || c[1] != dHunk || c[2] != dDel || c[3] != dAdd {
		t.Errorf("CRLF: %v", c)
	}
}

func TestDiffNoColor(t *testing.T) {
	lines := bodyOf(false, "```diff\n"+sampleDiff+"\n```", 60)
	for i, l := range lines {
		if strings.Contains(l, "\x1b[3") || strings.Contains(l, "\x1b[9") {
			t.Errorf("line %d has color under NO_COLOR: %q", i, l)
		}
	}
	if !strings.HasSuffix(lines[5], "-old") || !strings.HasSuffix(lines[6], "+new") {
		t.Errorf("markers lost: %q", lines[5:])
	}
}

func TestHugeDiffRenders(t *testing.T) {
	var b strings.Builder
	b.WriteString("```diff\n" + sampleDiff + "\n")
	for range 20000 {
		b.WriteString("+added line with some length to it\n-removed line\n")
	}
	b.WriteString("```")
	lines := bodyOf(true, b.String(), 50)
	if len(lines) != 7+40000 {
		t.Fatalf("lines=%d", len(lines))
	}
	for _, l := range lines {
		if strWidth(l) > 50 {
			t.Fatalf("overwide: %d", strWidth(l))
		}
	}
}

func TestLanguageLabel(t *testing.T) {
	label := func(bw int, text string) []string {
		var out []string
		for _, l := range bodyOf(true, text, bw) {
			out = append(out, ansi.Strip(l))
		}
		return out
	}
	got := label(30, "```go\nfunc main() {}\nsecond\n```")
	if len(got) != 2 {
		t.Fatalf("fence lines must not render: %q", got)
	}
	if want := "┃ func main() {}" + strings.Repeat(" ", 30-2-len("func main() {}")-2) + "go"; got[0] != want {
		t.Errorf("label placement:\n got %q\nwant %q", got[0], want)
	}
	if strings.Contains(got[1], "go") || strings.TrimRight(got[1], " ") != "┃ second" {
		t.Errorf("label only on first line: %q", got[1])
	}
	// Doesn't fit: omitted, content untouched.
	if g := label(19, "```go\nfunc main() {}\n```"); g[0] != "┃ func main() {}" {
		t.Errorf("tight width: %q", g[0])
	}
	// No info string: no label. Odd info strings: no label.
	if g := label(30, "```\nx\n```"); g[0] != "┃ x" {
		t.Errorf("no label expected: %q", g[0])
	}
	if g := label(30, "```<script>\nx\n```"); g[0] != "┃ x" {
		t.Errorf("odd info: %q", g[0])
	}
	// Extra words after the language are ignored.
	if g := label(30, "```python title=x\nx\n```"); !strings.HasSuffix(g[0], "python") {
		t.Errorf("first word: %q", g[0])
	}
	// Diff label.
	if g := label(40, "```diff\n+a\n```"); !strings.HasSuffix(g[0], "diff") {
		t.Errorf("diff label: %q", g[0])
	}
	// Empty block: nothing to hang a label on.
	if g := label(30, "```go\n```"); len(g) != 1 || g[0] != "" {
		t.Errorf("empty block: %q", g)
	}
}

func TestDiffAndLabelFitFrames(t *testing.T) {
	body := "look:\n```diff\n" + sampleDiff + "\n```\n```go\nfunc main() { println(\"hello 世界\") }\n```"
	for _, sz := range [][2]int{{40, 12}, {80, 24}, {20, 8}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		f.msgs[10] = []client.Message{{Message: proto.Message{ID: 900, Chan: 10, Author: 2, Time: time.Now().UnixMilli(), Body: body}}}
		run(m, m.openChannel(10))
		m.reloadPane(&m.main)
		v := checkView(t, m, "diff message")
		if sz[0] >= 40 && !strings.Contains(v, "+new") {
			t.Errorf("%dx%d: diff not visible:\n%s", sz[0], sz[1], v)
		}
	}
}

func TestViewerColorsDiff(t *testing.T) {
	m, _ := newTestModelFake(t, 80, 24)
	m.openViewer(client.Message{Message: proto.Message{ID: 1, Author: 1, Body: "hi\n```diff\n" + sampleDiff + "\n```\nbye"}})
	var del, add string
	for _, l := range m.view.lines {
		switch ansi.Strip(l) {
		case "-old":
			del = l
		case "+new":
			add = l
		}
	}
	st := testStyles(true)
	if del != st.diffDel.Render("-old") || add != st.diffAdd.Render("+new") {
		t.Fatalf("viewer lines not colored: %q %q", del, add)
	}
	if m.view.lines[1] != "```diff" || m.view.lines[len(m.view.lines)-2] != "```" {
		t.Fatalf("fences lost: %q", m.view.lines)
	}
	checkView(t, m, "viewer diff")
	for range 3 {
		m.viewerKey("right")
	}
	checkView(t, m, "viewer diff scrolled")

	// Hostile bodies stay sanitized in the viewer.
	m.openViewer(client.Message{Message: proto.Message{ID: 2, Author: 1, Body: "```diff\n+\x1b]0;pwn\x07x\n```"}})
	for _, l := range m.view.lines {
		if strings.Contains(l, "pwn") && strings.Contains(l, "\x1b]") {
			t.Fatalf("escape survived: %q", l)
		}
	}
}

// ---- /snippet ----

func snippetModel(t *testing.T, w, h int) (*Model, *fakeClient, string) {
	t.Helper()
	m, f := newTestModelFake(t, w, h)
	dir := t.TempDir()
	m.opt.SnippetDir = dir
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		b.WriteString("line " + string(rune('a'+i%26)) + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	run(m, m.openChannel(10))
	return m, f, dir
}

// drain runs a command synchronously and feeds its message back.
func drain(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	m.Update(cmd())
}

func submitSync(m *Model, text string) {
	m.input.SetValue(text)
	_, cmd := m.Update(keyMsg("enter"))
	drain(m, cmd)
}

func TestSnippetPreviewFlow(t *testing.T) {
	m, f, _ := snippetModel(t, 80, 24)
	submitSync(m, "/snippet main.go:5-7 see this")
	if m.mode != modeSharePreview {
		t.Fatalf("mode %v notice %q", m.mode, m.notice)
	}
	if want := "see this\n`main.go:5-7`\n```go\nline f\nline g\nline h\n```"; m.share.body != want {
		t.Fatalf("body %q want %q", m.share.body, want)
	}
	v := ansi.Strip(checkView(t, m, "snippet preview"))
	if !strings.Contains(v, "main.go:5-7") || !strings.Contains(v, "line g") || !strings.Contains(v, "Esc cancels") {
		t.Fatalf("preview:\n%s", v)
	}
	for _, sz := range [][2]int{{40, 12}, {20, 8}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkView(t, m, "narrow snippet preview")
	}

	// Esc cancels and sends nothing.
	m.Update(keyMsg("esc"))
	if m.mode != modeNormal || len(f.sent) != 0 {
		t.Fatalf("after Esc: mode %v sent %d", m.mode, len(f.sent))
	}
	if !strings.Contains(m.notice, "not shared") {
		t.Errorf("notice %q", m.notice)
	}

	// Enter sends exactly the previewed body.
	submitSync(m, "/snippet main.go:10")
	if m.mode != modeSharePreview {
		t.Fatalf("mode %v notice %q", m.mode, m.notice)
	}
	if !strings.Contains(m.share.body, "`main.go:7-13`") {
		t.Fatalf("context default: %q", m.share.body)
	}
	body := m.share.body
	m.Update(keyMsg("enter"))
	if m.mode != modeNormal || len(f.sent) != 1 || f.sent[0].Body != body || f.sent[0].Chan != 10 {
		t.Fatalf("after Enter: mode %v sent %+v", m.mode, f.sent)
	}
}

func TestSnippetErrors(t *testing.T) {
	m, f, dir := snippetModel(t, 80, 24)
	for _, in := range []string{"/snippet", "/snippet nope.go", "/snippet main.go:20-10", "/snippet main.go:999", "/snippet ."} {
		submitSync(m, in)
		if m.mode != modeNormal || !m.noticeErr || len(f.sent) != 0 {
			t.Errorf("%q: mode %v noticeErr %v notice %q", in, m.mode, m.noticeErr, m.notice)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "b.bin"), []byte("a\x00b"), 0o644); err != nil {
		t.Fatal(err)
	}
	submitSync(m, "/snippet b.bin")
	if !m.noticeErr || !strings.Contains(m.notice, "binary") {
		t.Errorf("binary: %q", m.notice)
	}
	checkView(t, m, "snippet error")

	// No channel open: error, no preview.
	m2, _ := newTestModelFake(t, 80, 24)
	m2.active = 0
	submitSync(m2, "/snippet x")
	if m2.mode != modeNormal || !m2.noticeErr {
		t.Errorf("no channel: %q", m2.notice)
	}
}
