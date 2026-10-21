package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

func plainLines(spans [][]span) []string {
	var out []string
	for _, l := range spans {
		var b strings.Builder
		for _, s := range l {
			b.WriteString(s.text)
		}
		out = append(out, b.String())
	}
	return out
}

func TestWrapWords(t *testing.T) {
	got := plainLines(wrapSpans([]span{{text: "hello world foo"}}, 11))
	want := []string{"hello world", "foo"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q want %q", got, want)
	}
	got = plainLines(wrapSpans([]span{{text: "aaaaaaaaaa"}}, 4))
	if strings.Join(got, "|") != "aaaa|aaaa|aa" {
		t.Fatalf("long word: %q", got)
	}
	got = plainLines(wrapSpans([]span{{text: "  indented  text"}}, 40))
	if got[0] != "  indented  text" {
		t.Fatalf("leading indent lost: %q", got)
	}
	got = plainLines(wrapSpans([]span{{text: ""}}, 10))
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("empty: %q", got)
	}
}

func TestWrapWideRunes(t *testing.T) {
	cases := []struct {
		s string
		w int
	}{
		{"你好世界你好世界", 5},
		{"你好世界你好世界", 1},
		{"👍👍👍 ok 🎉🎉", 3},
		{"👨‍👩‍👧👨‍👩‍👧👨‍👩‍👧", 4},
		{"mixed 中文 and emoji 😀 text", 7},
		{"é́́ combining", 3},
	}
	for _, c := range cases {
		lines := plainLines(wrapSpans([]span{{text: c.s}}, c.w))
		for _, l := range lines {
			if w := strWidth(l); w > c.w && !(c.w == 1 && w == 2) {
				t.Errorf("%q at %d: line %q is %d wide", c.s, c.w, l, w)
			}
		}
		joined := strings.ReplaceAll(strings.Join(lines, ""), " ", "")
		if joined != strings.ReplaceAll(c.s, " ", "") {
			t.Errorf("%q at %d: content changed: %q", c.s, c.w, lines)
		}
	}
	// A ZWJ family stays one cluster.
	for _, l := range plainLines(wrapSpans([]span{{text: "👨‍👩‍👧👨‍👩‍👧"}}, 2)) {
		if l != "👨‍👩‍👧" {
			t.Errorf("cluster split: %q", l)
		}
	}
}

func testRenderer(color bool) *renderer {
	names := map[uint64]string{1: "alice", 2: "bob", 3: "carol\x1b[2J", 4: strings.Repeat("x", 40)}
	return &renderer{st: testStyles(color), me: proto.User{ID: 1, Name: "alice"}, name: func(id uint64) string { return names[id] }}
}

func msg(id, author uint64, body string) client.Message {
	return client.Message{Message: proto.Message{ID: id, Chan: 10, Author: author, Body: body,
		Time: time.Date(2026, 10, 9, 10, 42, 0, 0, time.Local).UnixMilli()}}
}

func TestMessageNeverExceedsWidth(t *testing.T) {
	m := blockModel(true)
	bodies := []string{
		"short",
		strings.Repeat("word ", 50),
		strings.Repeat("x", 300),
		"你好世界你好世界你好世界你好世界你好世界你好世界",
		"👍👍👍👍👍👍👍👍👍👍👍👍👍👍👍👍👍👍👍👍👍👍",
		"```\n" + strings.Repeat("code ", 40) + "\n中文中文中文中文中文中文中文中文中文中文中文中文\n```",
		"**bold " + strings.Repeat("b", 80) + "** _it_ `" + strings.Repeat("c", 90) + "` https://example.com/" + strings.Repeat("p", 90),
		"@alice @alice @alice " + strings.Repeat("@alice", 20),
	}
	for _, body := range bodies {
		for _, author := range []uint64{1, 3, 4} {
			x := msg(1, author, body)
			x.Reactions = map[string][]uint64{"👍": {1, 2}, "🎉": {3}, "x\x1b[1m": {2}}
			x.Replies = 5
			x.Edited = 1
			for w := 1; w <= 130; w++ {
				for _, header := range []bool{true, false} {
					b, rows := block(m, x, w, header)
					if b.w != w || b.h == 0 {
						t.Fatalf("w=%d: block %dx%d", w, b.w, b.h)
					}
					for _, l := range rows {
						if lw := strWidth(l); lw > w {
							t.Fatalf("w=%d header=%v body=%.20q: row %q is %d wide", w, header, body, l, lw)
						}
					}
				}
			}
		}
	}
}

func TestCodeFence(t *testing.T) {
	m := blockModel(false)
	x := msg(1, 2, "see:\n```go\npanic: nil map write "+strings.Repeat("z", 100)+"\n  at handler.go:42\n```\nafter")
	b, rows := block(m, x, 66, true)
	plain := strings.Join(rows, "\n")
	if strings.Contains(plain, "```") {
		t.Fatalf("fence markers rendered:\n%s", plain)
	}
	hdr := rowWith(rows, " go")
	if hdr < 0 || !strings.Contains(rows[hdr], "copy") {
		t.Fatalf("no code header with the language and copy:\n%s", plain)
	}
	if c := cellAt(b, rows, hdr+1, "panic"); c == nil || c.bg != m.st.th.panel {
		t.Fatalf("code line not on the panel:\n%s", plain)
	}
	if !strings.Contains(rows[hdr+1], "…") {
		t.Fatalf("long code line not truncated with …:\n%s", plain)
	}
	if !strings.Contains(plain, "   at handler.go:42") {
		t.Fatalf("code indentation not preserved:\n%s", plain)
	}
	if !strings.Contains(plain, "after") {
		t.Fatalf("text after fence lost")
	}
}

func TestEscapesStripped(t *testing.T) {
	m := blockModel(true)
	x := msg(1, 3, "a\x1b[31mred\x1b]0;pwned\a b‮evil⁦x \x9b2J\x00")
	x.Reactions = map[string][]uint64{"\x1b]52;c;AAAA\a": {2}}
	b, rows := block(m, x, 80, true)
	out := b.encode(m.st.enc)
	for _, bad := range []string{"\x1b[31m", "\x1b]", "\a", "‮", "⁦", "\x9b", "\x00", "\x1b[2J"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q: %q", bad, out)
		}
	}
	if !strings.Contains(strings.Join(rows, "\n"), "[31mred") {
		t.Errorf("text around stripped escape lost: %q", rows)
	}
}

func TestInlineMarkdown(t *testing.T) {
	spans := parseInline("**bold** and _it_ `co*de*` see https://example.com/a. snake_case_name @alice!", "alice")
	find := func(text string) flag {
		for _, s := range spans {
			if s.text == text {
				return s.f
			}
		}
		t.Fatalf("no span %q in %+v", text, spans)
		return 0
	}
	if find("bold")&fBold == 0 || find("it")&fItalic == 0 || find("co*de*")&fCode == 0 {
		t.Fatalf("styles wrong: %+v", spans)
	}
	if find("https://example.com/a")&fURL == 0 {
		t.Fatalf("url: %+v", spans)
	}
	if find("@alice")&fMention == 0 {
		t.Fatalf("mention: %+v", spans)
	}
	for _, s := range spans {
		if s.f&fItalic != 0 && strings.Contains(s.text, "case") {
			t.Fatalf("snake_case became italic: %+v", spans)
		}
	}
	// Unclosed markers are literal.
	if got := parseInline("a ** b ` c", ""); len(got) != 1 || got[0].text != "a ** b ` c" {
		t.Fatalf("unclosed: %+v", got)
	}
}

func TestMentions(t *testing.T) {
	cases := map[string]bool{
		"hi @alice":        true,
		"@Alice: look":     true,
		"(@alice)":         true,
		"mail a@alice.com": false,
		"@alicebob":        false,
		"@alice-x":         false,
		"alice":            false,
	}
	for body, want := range cases {
		if got := mentionsMe(body, "alice"); got != want {
			t.Errorf("mentionsMe(%q) = %v", body, got)
		}
		hit := false
		for _, s := range parseInline(body, "alice") {
			hit = hit || s.f&fMention != 0
		}
		if hit != want {
			t.Errorf("parseInline(%q) mention = %v", body, hit)
		}
	}
	// Highlighted in rendered output with the accent.
	bm := blockModel(true)
	b, rows := block(bm, msg(1, 2, "hey @alice"), 80, true)
	if c := cellAt(b, rows, rowWith(rows, "@alice"), "@alice"); c == nil || c.fg != bm.st.th.accent || c.attr&aBold == 0 {
		t.Fatalf("mention not accented: %+v", c)
	}
}

func TestNoColor(t *testing.T) {
	x := msg(1, 2, "hi @alice **b** https://x.io")
	x.Reactions = map[string][]uint64{"👍": {1}}
	enc := func(color bool) (string, []string) {
		m := blockModel(color)
		b, rows := block(m, x, 80, true)
		return b.encode(m.st.enc), rows
	}
	nc, ncRows := enc(false)
	if strings.Contains(nc, "38;") || strings.Contains(nc, "48;") {
		t.Fatalf("NO_COLOR output has colors: %q", nc)
	}
	if !strings.Contains(nc, ";1m") && !strings.Contains(nc, ";1;") {
		t.Fatalf("NO_COLOR output lost bold: %q", nc)
	}
	c, cRows := enc(true)
	if !strings.Contains(c, "38;5;") {
		t.Fatalf("color output has no colors: %q", c)
	}
	if strings.Join(ncRows, "\n") != strings.Join(cRows, "\n") {
		t.Fatalf("text differs:\n%q\n%q", ncRows, cRows)
	}
}

func TestMessageLayout(t *testing.T) {
	m := blockModel(false)
	x := msg(1, 2, "morning! anyone seen the deploy logs from last night?")
	_, rows := block(m, x, 60, true)
	if rows[0] != "│ bob  10:42" {
		t.Fatalf("header row: %q", rows[0])
	}
	for _, l := range rows[1:] {
		if !strings.HasPrefix(l, "│ ") || strings.HasPrefix(l, "│  ") {
			t.Fatalf("body not under the name, after the bar: %q", l)
		}
	}
	_, g := block(m, x, 60, false)
	if strings.Contains(g[0], "bob") || strings.Contains(g[0], "10:42") {
		t.Fatalf("grouped message shows header: %q", g[0])
	}
	// Default (left): ours like everyone's, name in our color, time and
	// status under the text, the bar in our color.
	lb, left := block(m, msg(2, 1, "on it"), 60, true)
	if strings.Join(left, "|") != "│ alice|│ on it|│ 10:42 ✓" || lb.at(0, 0).fg != mix(m.st.th.bg, m.st.th.nick(1), 0.9) {
		t.Fatalf("own message, left: %q", left)
	}
	// Split: right-aligned against an accent bar, time and status under it.
	m.ui.Split = true
	b, mine := block(m, msg(2, 1, "on it"), 60, true)
	if !strings.HasSuffix(mine[0], "on it │") || b.at(59, 0).fg != m.st.th.accent {
		t.Fatalf("own message: %q", mine)
	}
	if !strings.HasSuffix(mine[len(mine)-1], "10:42 ✓") {
		t.Fatalf("own meta: %q", mine)
	}
}

func TestGrouping(t *testing.T) {
	a := msg(1, 2, "a")
	b := msg(2, 2, "b")
	b.Time = a.Time + 60_000
	if !grouped(a, b) {
		t.Fatal("same author within 5 min should group")
	}
	c := b
	c.Time = a.Time + 6*60_000
	if grouped(a, c) {
		t.Fatal("6 min apart should not group")
	}
	d := b
	d.Author = 3
	if grouped(a, d) {
		t.Fatal("different author grouped")
	}
	late := msg(1, 2, "late")
	late.Time = time.Date(2026, 10, 9, 23, 59, 0, 0, time.Local).UnixMilli()
	early := msg(2, 2, "early")
	early.Time = late.Time + 2*60_000
	if grouped(late, early) {
		t.Fatal("grouped across midnight")
	}
}

func TestFooterAndStates(t *testing.T) {
	m := blockModel(true)
	x := msg(1, 2, "hello")
	x.Reactions = map[string][]uint64{"👍": {1, 2}, "🎉": {3}}
	x.Replies = 3
	x.Edited = 5
	b, rows := block(m, x, 80, true)
	plain := strings.Join(rows, "\n")
	for _, want := range []string{"🎉 1", "👍 2", "↳ 3 replies", "(edited)"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in %q", want, plain)
		}
	}
	y := rowWith(rows, "👍 2")
	if c := cellAt(b, rows, y, "2"); c == nil || c.fg != m.st.th.accent || c.bg != m.st.th.accentBg {
		t.Errorf("own reaction not highlighted: %+v", c)
	}
	if c := cellAt(b, rows, y, "1"); c == nil || c.fg == m.st.th.accent {
		t.Errorf("someone else's reaction highlighted: %+v", c)
	}
	del := msg(2, 2, "secret")
	del.Deleted = true
	if _, r := block(m, del, 80, true); !strings.Contains(strings.Join(r, ""), "message deleted") || strings.Contains(strings.Join(r, ""), "secret") {
		t.Errorf("deleted: %q", r)
	}
	f := msg(0, 1, "oops")
	f.Failed = "forbidden"
	if _, r := block(m, f, 80, true); !strings.Contains(strings.Join(r, "\n"), "! failed — r to retry, d to discard") {
		t.Errorf("failed: %q", r)
	}
	pend := msg(0, 1, "wait")
	pend.Pending = true
	pb, pr := block(m, pend, 80, true)
	if c := cellAt(pb, pr, rowWith(pr, "wait"), "wait"); c == nil || c.fg != m.st.th.dim || !strings.Contains(strings.Join(pr, ""), "◌") {
		t.Errorf("pending not dim or no ◌: %q %+v", pr, c)
	}
}

func TestDaySeparatorsInPane(t *testing.T) {
	m := newTestModel(t, 80, 24)
	lines, owner := m.paneLines(&m.main, 60)
	seps := 0
	for i, o := range owner {
		if o == -1 {
			seps++
			if lines[i].b != nil && !strings.Contains(lines[i].text(), "── ") {
				t.Fatalf("separator line: %q", lines[i].text())
			}
			if lines[i].b == nil {
				seps-- // a blank gap row, not a separator
			}
		}
	}
	if seps < 2 {
		t.Fatalf("want day separators for two days, got %d", seps)
	}
}
