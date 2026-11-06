package tui

import (
	"context"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// writeSizes are the sizes every new writing feature must fit.
var writeSizes = [][2]int{{20, 6}, {40, 12}, {80, 24}, {120, 40}, {200, 60}}

func pasteUp(m *Model, s string) {
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true})
	run(m, cmd)
}

// onlySent drops the fake's pending and failed messages, so "my last
// message" is a sent one.
func onlySent(m *Model, f *fakeClient) {
	var keep []client.Message
	for _, x := range f.msgs[10] {
		if x.ID != 0 {
			keep = append(keep, x)
		}
	}
	f.msgs[10] = keep
	m.reloadPane(&m.main)
}

func TestQuoteHelpers(t *testing.T) {
	if got := quoteText("a\n\nb  \n"); got != "> a\n>\n> b\n" {
		t.Fatalf("quoteText: %q", got)
	}
	long := strings.Repeat("x\n", 30)
	if got := quoteText(long); strings.Count(got, "\n") != maxQuoteLines+1 || !strings.HasSuffix(got, "> …\n") {
		t.Fatalf("long quote: %q", got)
	}
	for in, want := range map[string]int{"> a": 1, "> > a": 2, ">": 1, ">>> py": 0, "->": 0, ">_<": 0, "a > b": 0, ">a": 0} {
		if d, _ := quoteDepth(in); d != want {
			t.Errorf("quoteDepth(%q) = %d, want %d", in, d, want)
		}
	}
}

func TestQuoteKey(t *testing.T) {
	m, _ := newTestModelFake(t, 120, 40)
	selectID(t, m, 102)
	press(m, "q")
	if m.mode != modeNormal {
		t.Fatalf("q left mode %v", m.mode)
	}
	v := m.input.Value()
	if !strings.HasPrefix(v, "> yep, pasting:\n> ```go\n") || !strings.HasSuffix(v, "\n") {
		t.Fatalf("quote: %q", v)
	}
	for _, l := range strings.Split(strings.TrimSuffix(v, "\n"), "\n") {
		if !strings.HasPrefix(l, ">") {
			t.Fatalf("unquoted line %q in %q", l, v)
		}
	}
	// A deleted message has nothing to quote.
	press(m, "ctrl+u")
	selectID(t, m, 106)
	press(m, "q")
	if m.input.Value() != "" || !strings.Contains(m.notice, "nothing to quote") {
		t.Fatalf("deleted quote: %q / %q", m.input.Value(), m.notice)
	}
	// The menu runs it too.
	press(m, "esc")
	selectID(t, m, 100)
	press(m, "m")
	m.mm.sel = menuIndex(t, m, "Quote")
	press(m, "enter")
	if m.input.Value() != "> morning! anyone seen the deploy logs from last night?\n" {
		t.Fatalf("menu quote: %q", m.input.Value())
	}
}

func TestQuoteRenders(t *testing.T) {
	for _, sz := range writeSizes {
		m, f := newTestModelFake(t, sz[0], sz[1])
		f.msgs[10] = append(f.msgs[10][:0:0], client.Message{Message: proto.Message{ID: 500, Chan: 10, Author: 2,
			Body: "> quoted **line** with a long tail that wraps around the narrow column\n> > nested\n>\nmy reply", Time: 1}})
		m.reloadPane(&m.main)
		m.main.scroll, m.main.place = 0, false
		v := checkView(t, m, "quote")
		if sz[0] >= 40 && (!strings.Contains(v, "▎ quoted line") || !strings.Contains(v, "▎ ▎ nested") || strings.Contains(v, "> quoted")) {
			t.Fatalf("%v: quote not drawn as one:\n%s", sz, v)
		}
		if sz[0] >= 40 && !strings.Contains(v, "my reply") {
			t.Fatalf("%v: reply missing:\n%s", sz, v)
		}
	}
}

func TestQuickReactPicker(t *testing.T) {
	for _, sz := range writeSizes {
		m, f := newTestModelFake(t, sz[0], sz[1])
		m.ui.Reacts = []string{"🎉", "🚀"}
		selectID(t, m, 100)
		press(m, "+")
		if m.mode != modeEmoji {
			continue // too small for the picker
		}
		v := checkView(t, m, "picker")
		if sz[0] >= 80 && !strings.Contains(v, "1 🎉") {
			t.Fatalf("%v: recent row missing:\n%s", sz, v)
		}
		if m.recentShown() < 2 {
			press(m, "2")
			if m.mode != modeEmoji || m.emo.query != "2" {
				t.Fatalf("%v: a digit not on screen picked (query %q)", sz, m.emo.query)
			}
			continue
		}
		press(m, "2")
		if m.mode == modeEmoji || !f.called("react 🚀") {
			t.Fatalf("%v: 2 didn't react 🚀 (mode %v, calls %v)", sz, m.mode, f.calls)
		}
		if m.ui.Reacts[0] != "🚀" || len(m.ui.Reacts) != 2 {
			t.Fatalf("recent not updated: %v", m.ui.Reacts)
		}
		checkView(t, m, "after react")
	}
	// A typed query makes digits search again: "+1" still finds 👍.
	m, f := newTestModelFake(t, 120, 40)
	selectID(t, m, 100)
	press(m, "+", "+", "1")
	if m.mode != modeEmoji || m.emo.query != "+1" {
		t.Fatalf("query %q mode %v", m.emo.query, m.mode)
	}
	press(m, "enter")
	if !f.called("react 👍") {
		t.Fatalf("+1 search: %v", f.calls)
	}
	// From the input (insert), digits type into the search.
	press(m, "esc", "alt+e", "1")
	if m.mode != modeEmoji || m.emo.query != "1" {
		t.Fatalf("insert picker: query %q", m.emo.query)
	}
}

func TestPlusPlus(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	m.ui.Reacts = []string{"🔥"}
	typeVia(m, "++")
	press(m, "enter")
	if !f.called("react 🔥") || m.input.Value() != "" || len(f.sent) != 0 {
		t.Fatalf("++: calls %v sent %v input %q", f.calls, f.sent, m.input.Value())
	}
	checkView(t, m, "++")
	// Escaped, or not the whole message: plain text.
	typeVia(m, `\++`)
	press(m, "enter")
	typeVia(m, "c++")
	press(m, "enter")
	if len(f.sent) != 2 || f.sent[0].Body != "++" || f.sent[1].Body != "c++" {
		t.Fatalf("sent %v", f.sent)
	}
	// A fresh install reacts with the first frequently used emoji.
	m2, f2 := newTestModelFake(t, 80, 24)
	typeVia(m2, "++")
	press(m2, "enter")
	if !f2.called("react " + freqSeed[0]) {
		t.Fatalf("fresh ++: %v", f2.calls)
	}
}

func TestPasteWrap(t *testing.T) {
	for _, sz := range writeSizes {
		m, _ := newTestModelFake(t, sz[0], sz[1])
		typeVia(m, "look:")
		pasteUp(m, "panic: boom\n  at a.go:1\n  at b.go:2")
		if !m.wrapArmed() || !strings.Contains(m.notice, "code block") {
			t.Fatalf("%v: no offer (%q)", sz, m.notice)
		}
		checkView(t, m, "offer")
		press(m, "tab")
		want := "look:\n```\npanic: boom\n  at a.go:1\n  at b.go:2\n```\n"
		if got := m.input.Value(); got != want {
			t.Fatalf("%v: wrapped %q, want %q", sz, got, want)
		}
		checkView(t, m, "wrapped")
	}
	m, _ := newTestModelFake(t, 80, 24)
	// Two lines: no offer, and Tab completes as usual.
	pasteUp(m, "a\nb")
	if m.wrapArmed() {
		t.Fatal("2-line paste offered")
	}
	// Typing after the paste takes the offer back.
	press(m, "ctrl+u")
	pasteUp(m, "1\n2\n3")
	typeVia(m, "x")
	if m.wrapArmed() || strings.Contains(m.notice, "code block") {
		t.Fatalf("offer kept after typing: %q", m.notice)
	}
	press(m, "tab")
	if strings.Contains(m.input.Value(), "```") {
		t.Fatal("Tab wrapped after an edit")
	}
	// Already code, a command, or inside an open fence: no offer.
	for _, c := range []struct{ before, p string }{{"", "```\na\nb\n```"}, {"/snippet ", "a\nb\nc"}, {"```\n", "a\nb\nc"}} {
		press(m, "ctrl+u")
		m.input.SetValue(c.before)
		pasteUp(m, c.p)
		if m.wrapArmed() {
			t.Fatalf("offered for %q + %q", c.before, c.p)
		}
	}
	// Tab completion still works after a normal typed word.
	press(m, "ctrl+u")
	typeVia(m, "@bo")
	press(m, "tab")
	if !strings.HasPrefix(m.input.Value(), "@bob") {
		t.Fatalf("completion: %q", m.input.Value())
	}
}

func TestSubstFix(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	onlySent(m, f)
	typeVia(m, "s/nice/great/")
	press(m, "enter")
	if len(f.edits) != 0 || len(f.sent) != 0 || !strings.Contains(m.notice, "again fixes") {
		t.Fatalf("first Enter: edits %v sent %v notice %q", f.edits, f.sent, m.notice)
	}
	checkView(t, m, "subst hint")
	press(m, "enter")
	if len(f.edits) != 1 || !strings.Contains(f.edits[0], "👍 great") || m.input.Value() != "" {
		t.Fatalf("second Enter: edits %v input %q", f.edits, m.input.Value())
	}
	// Changing the text after the hint asks again.
	typeVia(m, "s/nice/grea")
	press(m, "enter")
	typeVia(m, "t")
	press(m, "enter")
	if len(f.edits) != 1 {
		t.Fatalf("an edited s/// fixed at once: %v", f.edits)
	}
	press(m, "enter")
	if len(f.edits) != 2 {
		t.Fatalf("confirmed s/// didn't fix: %v", f.edits)
	}
	// Not in my last message, escaped, or pasted: sent as text.
	press(m, "ctrl+u")
	typeVia(m, "s/zzz/y")
	press(m, "enter")
	typeVia(m, `\s/nice/great`)
	press(m, "enter")
	pasteUp(m, "s/nice/great/")
	press(m, "enter")
	var bodies []string
	for _, x := range f.sent {
		bodies = append(bodies, x.Body)
	}
	if strings.Join(bodies, "|") != "s/zzz/y|s/nice/great|s/nice/great/" || len(f.edits) != 2 {
		t.Fatalf("sent %q edits %v", bodies, f.edits)
	}
	for _, s := range []string{"s/a/b/c", "s//b", "s/a", "s/a/b\nx"} {
		if _, _, ok := parseSubst(s); ok {
			t.Errorf("parseSubst(%q) matched", s)
		}
	}
}

func TestSafeURL(t *testing.T) {
	for _, u := range []string{"https://example.com", "http://a.b/x?y=1#z", "HTTPS://EXAMPLE.COM/a;b"} {
		if !safeURL(u) {
			t.Errorf("rejected %q", u)
		}
	}
	for _, u := range []string{"javascript:alert(1)", "file:///etc/passwd", "https://", "https://a b", "https://a\x1b]8;;x", "https://user:pw@evil.com",
		"https://é.com", "ftp://x.y", "https://a.com/" + strings.Repeat("x", 3000), "-https://x.y"} {
		if safeURL(u) {
			t.Errorf("accepted %q", u)
		}
	}
	if got := msgLinks("see https://a.com/x, and `https://code.com` and\n```\nhttps://block.com\n```\nhttps://a.com/x again"); len(got) != 1 || got[0] != "https://a.com/x" {
		t.Fatalf("msgLinks: %v", got)
	}
}

func TestHyperlinkDetect(t *testing.T) {
	env := func(kv ...string) func(string) string {
		return func(k string) string {
			for i := 0; i+1 < len(kv); i += 2 {
				if kv[i] == k {
					return kv[i+1]
				}
			}
			return ""
		}
	}
	cases := []struct {
		e    func(string) string
		want bool
	}{
		{env("TERM_PROGRAM", "iTerm.app"), true},
		{env("TERM_PROGRAM", "Apple_Terminal"), false},
		{env("TERM_PROGRAM", "ghostty", "TMUX", "x"), false},
		{env("TERM_PROGRAM", "WezTerm", "HERDR_PANE_ID", "1"), false},
		{env("VTE_VERSION", "7000"), true},
		{env("TERM", "xterm-256color"), false},
		{env("TC_LINKS", "on", "TMUX", "x"), true},
		{env("TC_LINKS", "off", "TERM_PROGRAM", "kitty"), false},
	}
	for i, c := range cases {
		if got := hyperlinks(c.e); got != c.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
}

func TestOSC8Links(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("abcdefghij", 12)
	for _, on := range []bool{false, true} {
		for _, sz := range writeSizes {
			m, f := newTestModelFake(t, sz[0], sz[1])
			m.opt.Hyperlinks = on
			f.msgs[10] = append(f.msgs[10], client.Message{Message: proto.Message{ID: 600, Chan: 10, Author: 2, Body: "read " + long + " now", Time: 1 << 50}})
			onlySent(m, f)
			m.main.scroll, m.main.place = 0, false
			v := m.View()
			checkView(t, m, "links")
			open := strings.Count(v, ";http")
			if !on && open != 0 {
				t.Fatalf("%v: links without support", sz)
			}
			if on && sz[0] >= 40 {
				if open < 2 || strings.Count(v, "\x1b]8;id="+linkID(long)+";"+long+"\x1b\\") != open {
					t.Fatalf("%v: %d links, each piece must name the whole URL\n%s", sz, open, ansi.Strip(v))
				}
				// every line closes what it opens
				for i, l := range strings.Split(v, "\n") {
					if strings.Count(l, "\x1b]8;id=") != strings.Count(l, "\x1b]8;;\x1b\\") {
						t.Fatalf("%v: line %d leaves a link open", sz, i)
					}
				}
			}
		}
	}
}

type openRec struct {
	mu   sync.Mutex
	urls []string
}

func (o *openRec) open(_ context.Context, u string) error {
	o.mu.Lock()
	o.urls = append(o.urls, u)
	o.mu.Unlock()
	return nil
}

func (o *openRec) got() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.urls...)
}

func TestOpenLink(t *testing.T) {
	m, f := newTestModelFake(t, 120, 40)
	rec := &openRec{}
	m.opt.OpenURL = rec.open
	selectID(t, m, 101)
	m.updatePlaceholder()
	if !strings.Contains(m.input.Placeholder, "l link") {
		t.Fatalf("footer lacks l: %q", m.input.Placeholder)
	}
	press(m, "l")
	if got := rec.got(); len(got) != 1 || got[0] != "https://example.com/path" {
		t.Fatalf("opened %v", got)
	}
	selectID(t, m, 100)
	press(m, "l")
	if !strings.Contains(m.notice, "no link") {
		t.Fatalf("notice %q", m.notice)
	}
	// Several links: a list.
	press(m, "esc")
	f.msgs[10] = append(f.msgs[10], client.Message{Message: proto.Message{ID: 700, Chan: 10, Author: 3, Body: "https://one.example and http://two.example/x", Time: 1 << 50}})
	m.reloadPane(&m.main)
	selectID(t, m, 700)
	press(m, "l")
	if m.mode != modeList || len(m.list.urls) != 2 {
		t.Fatalf("mode %v urls %v", m.mode, m.list.urls)
	}
	press(m, "esc")
	if m.mode != modeSelect || m.main.msgs[m.main.sel].ID != 700 {
		t.Fatalf("Esc from the list: mode %v", m.mode)
	}
	press(m, "l", "down", "enter")
	if got := rec.got(); got[len(got)-1] != "http://two.example/x" || m.mode != modeSelect {
		t.Fatalf("opened %v mode %v", got, m.mode)
	}
	press(m, "esc")
	// Over SSH it copies instead.
	m.opt.SSH = true
	selectID(t, m, 101)
	press(m, "l")
	if len(rec.got()) != 2 || m.notice != "copied" {
		t.Fatalf("ssh: %v %q", rec.got(), m.notice)
	}
}

func TestLinksCommand(t *testing.T) {
	for _, sz := range writeSizes {
		m, f := newTestModelFake(t, sz[0], sz[1])
		rec := &openRec{}
		m.opt.OpenURL = rec.open
		f.msgs[10] = append(f.msgs[10], client.Message{Message: proto.Message{ID: 800, Chan: 10, Author: 3, Body: "new https://new.example/ \x1b[31mred", Time: 1 << 50}})
		m.input.SetValue("/links")
		run(m, m.submit())
		if m.mode != modeList || len(m.list.urls) != 2 || m.list.urls[0] != "https://new.example/" {
			t.Fatalf("%v: mode %v urls %v", sz, m.mode, m.list.urls)
		}
		v := checkView(t, m, "/links")
		if sz[0] >= 80 && !strings.Contains(v, "https://new.example/") {
			t.Fatalf("%v: list:\n%s", sz, v)
		}
		press(m, "y")
		if m.mode != modeList || m.notice != "copied" {
			t.Fatalf("y: mode %v notice %q", m.mode, m.notice)
		}
		press(m, "enter")
		if got := rec.got(); len(got) != 1 || got[0] != "https://new.example/" {
			t.Fatalf("enter opened %v", got)
		}
	}
	m, f := newTestModelFake(t, 80, 24)
	f.msgs[10] = f.msgs[10][:1]
	m.input.SetValue("/links")
	run(m, m.submit())
	if m.mode != modeNormal || !strings.Contains(m.notice, "no links") {
		t.Fatalf("no links: mode %v %q", m.mode, m.notice)
	}
}

func TestSidebarMute(t *testing.T) {
	for _, sz := range writeSizes {
		m, f := newTestModelFake(t, sz[0], sz[1])
		g := m.geo()
		if g.SB < 14 {
			checkView(t, m, "no sidebar")
			continue
		}
		x, y := find(t, m, "chan:11")
		m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
		v := checkView(t, m, "hover")
		if !strings.Contains(sidebarRow(t, v, "backend"), "mute") {
			t.Fatalf("%v: no mute button:\n%s", sz, v)
		}
		click(t, m, "sbmute:11")
		if !m.muted(11) || f.nchans[11].Level != client.LevelOff {
			t.Fatalf("%v: not muted", sz)
		}
		v = checkView(t, m, "muted")
		if !strings.Contains(ansi.Strip(v), "unmute") {
			t.Fatalf("%v: no unmute button:\n%s", sz, v)
		}
		click(t, m, "sbmute:11")
		if m.muted(11) {
			t.Fatalf("%v: still muted", sz)
		}
		// the pointer leaves: the badge comes back
		m.Update(tea.MouseMsg{X: m.w - 1, Y: 0, Action: tea.MouseActionMotion})
		if v := checkView(t, m, "left"); strings.Contains(sidebarRow(t, v, "backend"), "mute") {
			t.Fatalf("%v: button stayed", sz)
		}
	}
}

func TestPlusPlusEmptyPrefs(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	m.ui.Emoji, m.ui.Reacts = []emojiUse{}, nil // every stored entry dropped on load
	typeVia(m, "++")
	press(m, "enter")
	if !f.called("react " + freqSeed[0]) {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestSubstEdgeCases(t *testing.T) {
	// My newest message is still sending: never sent as text.
	m, f := newTestModelFake(t, 80, 24)
	f.msgs[10] = f.msgs[10][:len(f.msgs[10])-1] // drop the failed one; "pending one" is newest
	m.reloadPane(&m.main)
	typeVia(m, "s/pending/sent")
	press(m, "enter")
	if len(f.sent) != 0 || len(f.edits) != 0 || !strings.Contains(m.notice, "still sending") || m.input.Value() == "" {
		t.Fatalf("pending: sent %v edits %v notice %q", f.sent, f.edits, m.notice)
	}
	// Typed s/a/ with the replacement pasted: still a fix.
	m, f = newTestModelFake(t, 80, 24)
	onlySent(m, f)
	typeVia(m, "s/nice/")
	pasteUp(m, "great")
	press(m, "enter")
	if !strings.Contains(m.notice, "again fixes") {
		t.Fatalf("typed+pasted: notice %q sent %v", m.notice, f.sent)
	}
	// A new message of mine between the two Enters asks again.
	f.msgs[10] = append(f.msgs[10], client.Message{Message: proto.Message{ID: 900, Chan: 10, Author: 1, Body: "so nice", Time: 1 << 50}})
	m.reloadPane(&m.main)
	press(m, "enter")
	if len(f.edits) != 0 || !strings.Contains(m.notice, "again fixes") {
		t.Fatalf("target changed but edited: %v", f.edits)
	}
	press(m, "enter")
	if len(f.edits) != 1 || f.edits[0] != "so great" {
		t.Fatalf("edits %v", f.edits)
	}
	// An offer's hint goes when the input changes another way.
	pasteUp(m, "x\ny\nz")
	m.input.SetValue("other")
	press(m, "left")
	if strings.Contains(m.notice, "code block") {
		t.Fatalf("stale hint: %q", m.notice)
	}
}
