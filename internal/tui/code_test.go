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

// codeRows draws text as bob's message at block width bw+6 and returns the
// block and its rows.
func codeRows(color bool, text string, bw int) (*Model, *canvas, []string) {
	m := blockModel(color)
	b, rows := block(m, client.Message{Message: proto.Message{ID: 1, Author: 2, Body: text}}, bw+6, false)
	return m, b, rows
}

func TestDiffRendering(t *testing.T) {
	for _, body := range []string{"```diff\n" + sampleDiff + "\n```", "```\n" + sampleDiff + "\n```"} {
		m, b, rows := codeRows(true, body, 60)
		th := m.st.th
		if !strings.Contains(rows[0], "diff  +1 −1") {
			t.Fatalf("header: %q", rows)
		}
		want := []struct {
			text   string
			fg, bg rgb
		}{
			{"diff --git a/x.go b/x.go", th.dim, th.panel},
			{"--- a/x.go", th.dim, th.panel},
			{"+++ b/x.go", th.dim, th.panel},
			{"@@ -1,3 +1,3 @@", th.hunk, th.panel},
			{"-old", th.del, th.delBg},
			{"+new", th.add, th.addBg},
			{" ctx", th.fg, th.panel},
		}
		for _, w := range want {
			y := rowWith(rows, w.text)
			c := cellAt(b, rows, y, strings.TrimSpace(w.text)[:1])
			if c == nil || c.fg != w.fg || c.bg != w.bg {
				t.Errorf("%q: %+v, want fg %v bg %v", w.text, c, w.fg, w.bg)
			}
		}
	}
	// Not a diff: plain code. A labeled "go" block holding diff-looking
	// text is not a diff either.
	for _, body := range []string{"```\n-not diff\n+really\n```", "```go\n" + sampleDiff + "\n```"} {
		m, b, rows := codeRows(true, body, 60)
		y := rowWith(rows, "+")
		if c := cellAt(b, rows, y, "+"); c == nil || c.fg != m.st.th.fg || c.bg != m.st.th.panel {
			t.Errorf("%q colored as a diff: %+v", body, c)
		}
	}
	// Hand-written ```diff with no headers: +/- still colored.
	m, b, rows := codeRows(true, "```diff\n-a\n+b\n```", 40)
	if c := cellAt(b, rows, rowWith(rows, "-a"), "-"); c == nil || c.fg != m.st.th.del {
		t.Errorf("bare diff: %q", rows)
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
	m, b, rows := codeRows(false, "```diff\n"+sampleDiff+"\n```", 60)
	if out := b.encode(m.st.enc); strings.Contains(out, "38;") || strings.Contains(out, "48;") {
		t.Errorf("color under NO_COLOR: %q", out)
	}
	if rowWith(rows, "-old") < 0 || rowWith(rows, "+new") < 0 {
		t.Errorf("markers lost: %q", rows)
	}
}

// A huge paste draws a bounded block (v shows it all), quickly.
func TestHugeDiffRenders(t *testing.T) {
	var b strings.Builder
	b.WriteString("```diff\n" + sampleDiff + "\n")
	for range 20000 {
		b.WriteString("+added line with some length to it\n-removed line\n")
	}
	b.WriteString("```")
	_, blk, rows := codeRows(true, b.String(), 50)
	if blk.h > maxBlockRows+4 || rowWith(rows, "more lines · v views all") < 0 {
		t.Fatalf("rows=%d, last %q", blk.h, rows[len(rows)-1])
	}
	for _, l := range rows {
		if strWidth(l) > 56 {
			t.Fatalf("overwide: %d", strWidth(l))
		}
	}
	long := strings.Repeat("word ", 20000)
	if _, blk, rows := codeRows(true, long, 50); blk.h > maxBlockRows+4 || rowWith(rows, "v views all") < 0 {
		t.Fatalf("long text: %d rows", blk.h)
	}
}

func TestLanguageLabel(t *testing.T) {
	head := func(bw int, text string) string {
		_, _, rows := codeRows(true, text, bw)
		s := strings.TrimSpace(strings.TrimPrefix(rows[0], "│"))
		for _, ic := range []string{iconIn(packUnicode, icCode), iconIn(packUnicode, icFile)} {
			s = strings.TrimSpace(strings.TrimPrefix(s, ic)) // the label's icon
		}
		return s
	}
	if got := head(30, "```go\nfunc main() {}\nsecond\n```"); !strings.HasPrefix(got, "go") || !strings.HasSuffix(got, "copy") {
		t.Errorf("label: %q", got)
	}
	if _, _, rows := codeRows(true, "```go\nfunc main() {}\nsecond\n```", 30); len(rows) != 3 || strings.Contains(strings.Join(rows, ""), "```") {
		t.Errorf("fence lines must not render: %q", rows)
	}
	// No info string or an odd one: "code".
	for _, text := range []string{"```\nx\n```", "```<script>\nx\n```"} {
		if got := head(30, text); !strings.HasPrefix(got, "code") {
			t.Errorf("%q: %q", text, got)
		}
	}
	// Extra words after the language are ignored.
	if got := head(30, "```python title=x\nx\n```"); !strings.HasPrefix(got, "python ") {
		t.Errorf("first word: %q", got)
	}
	if got := head(40, "```diff\n+a\n```"); !strings.HasPrefix(got, "diff") {
		t.Errorf("diff label: %q", got)
	}
	// Empty block: nothing to draw.
	if _, _, rows := codeRows(true, "```go\n```", 30); strings.Contains(strings.Join(rows, ""), "copy") {
		t.Errorf("empty block: %q", rows)
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
