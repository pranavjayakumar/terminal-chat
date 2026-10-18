package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/proto"
)

// typeKeys types s one key per rune, as a person does (press is in
// drafts_test.go).
func typeKeys(m *Model, s string) {
	for _, r := range s {
		send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func send(m *Model, k tea.KeyMsg) {
	_, cmd := m.Update(k)
	run(m, cmd)
}

func paste(m *Model, s string) {
	send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true})
}

func compTexts(m *Model) []string {
	var out []string
	for _, it := range m.comp.items {
		out = append(out, it.text)
	}
	return out
}

func TestSlashMenuOpensAndFilters(t *testing.T) {
	for _, sz := range [][2]int{{40, 12}, {200, 60}, {80, 24}, {20, 6}} {
		m := newTestModel(t, sz[0], sz[1])
		typeKeys(m, "/")
		if !m.compShown() || m.comp.kind != '/' {
			t.Fatalf("%v: / didn't open the menu", sz)
		}
		plain := checkView(t, m, "/")
		if sz[1] >= 12 && !strings.Contains(plain, "/join #x") {
			t.Fatalf("%v: menu not drawn:\n%s", sz, plain)
		}
		// Scroll to the last item (↑ wraps) and back.
		press(m, "up")
		checkView(t, m, "up")
		press(m, "down")
		typeKeys(m, "re")
		texts := compTexts(m)
		if len(texts) == 0 || texts[0] != "/remove" {
			t.Fatalf("%v: /re: %v", sz, texts)
		}
		// Prefix matches before substring ones (/accept-key, /share-pane...).
		seenSub := false
		for _, s := range texts {
			pre := strings.HasPrefix(s, "/re")
			if !pre {
				seenSub = true
			} else if seenSub {
				t.Fatalf("%v: prefix match %s after a substring match: %v", sz, s, texts)
			}
		}
		checkView(t, m, "/re")
		typeKeys(m, "zzz")
		if m.compShown() {
			t.Fatalf("%v: menu shown with no matches", sz)
		}
		checkView(t, m, "/rezzz")
	}
}

func TestSlashMenuEnterCompletesThenSends(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "/he")
	press(m, "enter")
	if v := m.input.Value(); v != "/help " {
		t.Fatalf("Enter on /he: input %q, want %q", v, "/help ")
	}
	if m.mode != modeNormal || m.compShown() {
		t.Fatalf("Enter ran the command or left the menu open (mode %v)", m.mode)
	}
	press(m, "enter") // popup closed: runs as today
	if m.mode != modeHelp {
		t.Fatalf("second Enter didn't run /help (mode %v)", m.mode)
	}
	press(m, "esc")

	// Exactly a command: Enter still completes (adds the space) first.
	typeKeys(m, "/help")
	if !m.compShown() || m.comp.items[m.comp.sel].text != "/help" {
		t.Fatalf("/help not highlighted: %v sel %d", compTexts(m), m.comp.sel)
	}
	press(m, "enter")
	if v := m.input.Value(); v != "/help " || m.mode != modeNormal {
		t.Fatalf("exact /help + Enter: %q mode %v", v, m.mode)
	}
	press(m, "enter")
	if m.mode != modeHelp {
		t.Fatal("second Enter didn't run /help")
	}
	press(m, "esc")

	// An exact name sorts first even when others share its prefix.
	typeKeys(m, "/invite")
	if got := compTexts(m); got[0] != "/invite" {
		t.Fatalf("/invite: %v", got)
	}
	press(m, "ctrl+u")

	// A space after the command closes the menu; Enter then runs it.
	typeKeys(m, "/topic hello")
	if m.compShown() {
		t.Fatal("menu open after a space")
	}
	press(m, "enter")
	if !f.called("topic hello") || m.input.Value() != "" {
		t.Fatalf("/topic hello not run: %q", m.input.Value())
	}
	if len(f.sent) != 0 {
		t.Fatalf("a command was sent as a message: %+v", f.sent)
	}
}

func TestSlashMenuTabAndArrows(t *testing.T) {
	m := newTestModel(t, 80, 24)
	typeKeys(m, "/")
	first, second := m.comp.items[0].text, m.comp.items[1].text
	press(m, "down")
	if m.comp.sel != 1 {
		t.Fatalf("down: sel %d", m.comp.sel)
	}
	press(m, "ctrl+n", "ctrl+p", "up", "down")
	if m.comp.sel != 1 {
		t.Fatalf("ctrl+n/ctrl+p/up/down: sel %d", m.comp.sel)
	}
	press(m, "tab")
	if v := m.input.Value(); v != second+" " {
		t.Fatalf("Tab: %q, want %q", v, second+" ")
	}
	if m.active != 10 {
		t.Fatal("ctrl+n/ctrl+p switched channels while the menu was open")
	}
	press(m, "ctrl+u")
	typeKeys(m, "/")
	press(m, "up") // wraps to the last
	if m.comp.sel != len(m.comp.items)-1 {
		t.Fatalf("up from the top: sel %d", m.comp.sel)
	}
	press(m, "down")
	press(m, "enter")
	if v := m.input.Value(); v != first+" " {
		t.Fatalf("Enter: %q", v)
	}
}

func TestSlashMenuEscClosesWithoutClearing(t *testing.T) {
	m := newTestModel(t, 80, 24)
	typeKeys(m, "/no")
	press(m, "esc")
	if m.compShown() || m.input.Value() != "/no" {
		t.Fatalf("Esc: shown %v, input %q", m.compShown(), m.input.Value())
	}
	if m.notice != "" {
		t.Fatalf("Esc armed the clear-draft prompt: %q", m.notice)
	}
	press(m, "esc")
	if m.notice == "" {
		t.Fatal("second Esc didn't fall through to the usual chain")
	}
	// Cursor moves don't reopen it; an edit does.
	send(m, tea.KeyMsg{Type: tea.KeyLeft})
	if m.compShown() {
		t.Fatal("reopened on a cursor move")
	}
	press(m, "esc", "esc") // the double Esc still clears the draft
	if m.input.Value() != "" {
		t.Fatalf("double Esc didn't clear: %q", m.input.Value())
	}
	typeKeys(m, "/no")
	press(m, "esc")
	typeKeys(m, "t")
	if !m.compShown() {
		t.Fatal("an edit after Esc didn't reopen the menu")
	}
	// Enter after Esc sends the text as it is (an unknown command: kept).
	press(m, "esc", "enter")
	if m.input.Value() != "/not" || !m.noticeErr {
		t.Fatalf("Enter after Esc: %q err %v", m.input.Value(), m.noticeErr)
	}
	// Tab reopens it.
	press(m, "tab")
	if m.input.Value() != "/notify " {
		t.Fatalf("Tab after Esc: %q", m.input.Value())
	}
}

func TestNormalMessageSendsOnFirstEnter(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "hi there #1 and a@b.c")
	if m.compShown() {
		t.Fatalf("popup open for plain text: %v", compTexts(m))
	}
	press(m, "enter")
	if len(f.sent) != 1 || f.sent[0].Body != "hi there #1 and a@b.c" {
		t.Fatalf("sent %+v", f.sent)
	}
	typeKeys(m, "//etc")
	if m.compShown() {
		t.Fatal("// opened the menu")
	}
	press(m, "enter")
	if len(f.sent) != 2 || f.sent[1].Body != "/etc" {
		t.Fatalf("sent %+v", f.sent)
	}
}

func TestPasteNeverOpensPopup(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	paste(m, "/notify all")
	if m.compShown() {
		t.Fatal("paste opened the menu")
	}
	if m.input.Value() != "/notify all" {
		t.Fatalf("paste mangled: %q", m.input.Value())
	}
	press(m, "enter")
	if m.input.Value() != "" || m.noticeErr {
		t.Fatalf("pasted /notify all didn't run: %q %q", m.input.Value(), m.notice)
	}
	paste(m, "/he")
	press(m, "enter") // runs "/he" as typed, as before: unknown, kept
	if m.input.Value() != "/he" || !m.noticeErr {
		t.Fatalf("pasted /he: %q", m.input.Value())
	}
	press(m, "ctrl+u")
	paste(m, "ping @bo")
	if m.compShown() {
		t.Fatal("paste opened the @ popup")
	}
	press(m, "enter")
	if len(f.sent) != 1 || f.sent[0].Body != "ping @bo" {
		t.Fatalf("pasted message not sent on the first Enter: %+v", f.sent)
	}
	// A burst of fast typing (several runes, no paste flag) is text too.
	send(m, keyMsg("ok @b"))
	press(m, "enter")
	if len(f.sent) != 2 || f.sent[1].Body != "ok @b" {
		t.Fatalf("burst: %+v", f.sent)
	}
}

func TestAtCompletion(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "hi @")
	// #general's members (bob, carol) before others; not me.
	if got := strings.Join(compTexts(m), " "); got != "@bob @carol @averyveryverylongusername" {
		t.Fatalf("@ candidates: %s", got)
	}
	checkView(t, m, "@")
	typeKeys(m, "b")
	press(m, "enter")
	if v := m.input.Value(); v != "hi @bob " {
		t.Fatalf("Enter: %q", v)
	}
	if len(f.sent) != 0 {
		t.Fatal("Enter on the popup sent")
	}
	press(m, "enter")
	if len(f.sent) != 1 || f.sent[0].Body != "hi @bob" {
		t.Fatalf("second Enter: %+v", f.sent)
	}

	// Display names match too; ↓ then Tab picks the next one.
	typeKeys(m, "@")
	press(m, "down", "tab")
	if v := m.input.Value(); v != "@carol " {
		t.Fatalf("down+Tab: %q", v)
	}
	press(m, "ctrl+u")
	typeKeys(m, "@Carol C")
	press(m, "ctrl+u")

	// Mid-text: the whole word is replaced and the existing space reused.
	typeKeys(m, "@ca x")
	send(m, tea.KeyMsg{Type: tea.KeyLeft})
	send(m, tea.KeyMsg{Type: tea.KeyLeft})
	if !m.compShown() {
		t.Fatal("popup not open with the cursor after @ca")
	}
	press(m, "tab")
	if v := m.input.Value(); v != "@carol x" {
		t.Fatalf("mid-text: %q", v)
	}
	press(m, "ctrl+u")

	// Groups come after people matching as well.
	m.groups = []proto.Group{{Name: "oncall", Members: []uint64{2, 3}}}
	typeKeys(m, "@o")
	// A prefix match (the group) beats substring ones (b-o-b).
	if got := compTexts(m); len(got) == 0 || got[0] != "@oncall" {
		t.Fatalf("@o: %v", got)
	}
	press(m, "enter")
	if v := m.input.Value(); v != "@oncall " {
		t.Fatalf("group: %q", v)
	}
	checkView(t, m, "group")
}

func TestHashCompletion(t *testing.T) {
	m := newTestModel(t, 80, 24)
	typeKeys(m, "see #")
	if got := strings.Join(compTexts(m), " "); got != "#backend #general #secret #random" {
		t.Fatalf("# candidates: %s", got)
	}
	typeKeys(m, "ge")
	press(m, "tab")
	if v := m.input.Value(); v != "see #general " {
		t.Fatalf("Tab: %q", v)
	}
	// Multi-line input: the word is on the cursor's line.
	press(m, "alt+enter")
	typeKeys(m, "and #ra")
	press(m, "enter")
	if v := m.input.Value(); v != "see #general \nand #random " {
		t.Fatalf("second line: %q", v)
	}
}

func TestPopupDoesntInterfere(t *testing.T) {
	m := newTestModel(t, 80, 24)
	// ↑ on an empty input still selects messages.
	press(m, "up")
	if m.mode != modeSelect {
		t.Fatal("↑ on empty input didn't enter select mode")
	}
	press(m, "esc")
	// Ctrl-K over an open menu opens the switcher; the menu isn't drawn there.
	typeKeys(m, "/")
	press(m, "ctrl+k")
	if m.mode != modeSwitcher || m.compShown() {
		t.Fatalf("ctrl+k: mode %v shown %v", m.mode, m.compShown())
	}
	press(m, "esc")
	if !m.compShown() {
		t.Fatal("menu gone after closing the switcher with the same input")
	}
	press(m, "ctrl+u")
	// While editing a message, / is text, not a command.
	m.editing = 103
	typeKeys(m, "/x")
	if m.compShown() {
		t.Fatal("menu open while editing")
	}
	m.editing = 0
}

func TestAdminCommandsHidden(t *testing.T) {
	m := newTestModel(t, 80, 24)
	typeKeys(m, "/dis")
	for _, s := range compTexts(m) {
		if s == "/disable" {
			t.Fatal("/disable offered to a non-admin")
		}
	}
	press(m, "ctrl+u")
	m.me.Admin = true
	typeKeys(m, "/dis")
	if got := compTexts(m); len(got) == 0 || got[0] != "/disable" {
		t.Fatalf("admin /dis: %v", got)
	}
}

func TestPopupInThread(t *testing.T) {
	m := newTestModel(t, 120, 30)
	run(m, m.openThread(103))
	typeKeys(m, "@bo")
	press(m, "enter")
	if v := m.input.Value(); v != "@bob " {
		t.Fatalf("thread: %q", v)
	}
	checkView(t, m, "thread")
}

// Esc backs out one thing at a time: the menu, then the open mic, then the
// ring.
func TestEscMenuBeforeMicAndRing(t *testing.T) {
	m, h := newHuddleModel(t, 40, 12)
	newClock(m)
	runInput(t, m, "/huddle")
	runInput(t, m, "/talk")
	startHuddle(m, h, 20, 2)
	typeKeys(m, "/hu")
	if !m.compShown() {
		t.Fatal("menu not open")
	}
	checkView(t, m, "menu over ring")
	press(m, "esc")
	if m.compShown() || !h.Talking() || m.nt.ring.ch == 0 {
		t.Fatalf("first Esc: menu %v talking %v ring %d", m.compShown(), h.Talking(), m.nt.ring.ch)
	}
	press(m, "esc")
	if h.Talking() || m.nt.ring.ch == 0 {
		t.Fatalf("second Esc: talking %v ring %d", h.Talking(), m.nt.ring.ch)
	}
	press(m, "esc")
	if m.nt.ring.ch != 0 {
		t.Fatal("third Esc didn't dismiss the ring")
	}
	if m.input.Value() != "/hu" {
		t.Fatalf("input changed: %q", m.input.Value())
	}
}
