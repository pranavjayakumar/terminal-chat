package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
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
	if m.compShown() {
		t.Fatal("a cursor move opened the popup")
	}
	press(m, "tab") // one match: completes at once
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

// Cursor moves never open the popup: ↑/↓ keep moving the cursor and Enter
// sends the draft as it is.
func TestCursorMovesDontOpenPopup(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "hi @bob")
	press(m, "esc", "alt+enter")
	typeKeys(m, "second line here")
	press(m, "up")
	if m.compShown() {
		t.Fatal("↑ onto an @word opened the popup")
	}
	press(m, "down")
	if m.input.Line() != 1 {
		t.Fatalf("↓ didn't move the cursor back (line %d)", m.input.Line())
	}
	press(m, "up", "enter")
	if len(f.sent) != 1 || f.sent[0].Body != "hi @bob\nsecond line here" {
		t.Fatalf("sent %+v", f.sent)
	}
	typeKeys(m, "hey @bob can")
	for range 5 {
		send(m, tea.KeyMsg{Type: tea.KeyLeft})
	}
	press(m, "enter")
	if len(f.sent) != 2 || f.sent[1].Body != "hey @bob can" {
		t.Fatalf("← over a mention then Enter: %+v", f.sent)
	}
}

// "#12" in a message isn't completed to #release-12 by a substring match.
func TestDigitsArentCompleted(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	f.chans = append(f.chans, client.ChannelView{Channel: proto.Channel{ID: 14, Name: "release-12", Kind: proto.ChanPublic}, Display: "release-12", Member: true})
	m.reloadChans()
	typeKeys(m, "fixed in #12")
	if m.compShown() {
		t.Fatalf("popup for #12: %v", compTexts(m))
	}
	press(m, "enter")
	if len(f.sent) != 1 || f.sent[0].Body != "fixed in #12" {
		t.Fatalf("sent %+v", f.sent)
	}
	typeKeys(m, "#rel")
	if got := compTexts(m); len(got) != 1 || got[0] != "#release-12" {
		t.Fatalf("#rel: %v", got)
	}
	press(m, "ctrl+u")
	typeKeys(m, "#e") // one letter: prefix matches only
	for _, s := range compTexts(m) {
		if !strings.HasPrefix(s, "#e") {
			t.Fatalf("one-letter substring match %s", s)
		}
	}
	press(m, "ctrl+u")
	typeKeys(m, "#ase") // two letters: substring matches too
	if got := compTexts(m); len(got) != 1 || got[0] != "#release-12" {
		t.Fatalf("#ase: %v", got)
	}
}

// A word that already equals the highlighted item has nothing to complete:
// Enter sends on the first press (the owner's rule). Tab still completes.
func TestExactMentionSendsOnFirstEnter(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "thanks @bob")
	if !m.compShown() || m.comp.items[m.comp.sel].text != "@bob" {
		t.Fatalf("@bob not highlighted: %v sel %d", compTexts(m), m.comp.sel)
	}
	press(m, "enter")
	if len(f.sent) != 1 || f.sent[0].Body != "thanks @bob" {
		t.Fatalf("first Enter: sent %+v input %q", f.sent, m.input.Value())
	}
	if m.input.Value() != "" || m.compShown() {
		t.Fatalf("after send: input %q popup %v", m.input.Value(), m.compShown())
	}
	checkView(t, m, "sent")
}

func TestExactChannelSendsOnFirstEnter(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "see #general")
	if !m.compShown() || m.comp.items[m.comp.sel].text != "#general" {
		t.Fatalf("#general not highlighted: %v sel %d", compTexts(m), m.comp.sel)
	}
	press(m, "enter")
	if len(f.sent) != 1 || f.sent[0].Body != "see #general" || m.input.Value() != "" {
		t.Fatalf("first Enter: sent %+v input %q", f.sent, m.input.Value())
	}
}

func TestPrefixMentionEnterCompletes(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "thanks @bo")
	if !m.compShown() || m.comp.items[m.comp.sel].text != "@bob" {
		t.Fatalf("@bob not highlighted: %v sel %d", compTexts(m), m.comp.sel)
	}
	press(m, "enter")
	if len(f.sent) != 0 || m.input.Value() != "thanks @bob " {
		t.Fatalf("Enter on @bo: sent %+v input %q", f.sent, m.input.Value())
	}
	press(m, "enter")
	if len(f.sent) != 1 || f.sent[0].Body != "thanks @bob" {
		t.Fatalf("second Enter: %+v", f.sent)
	}
}

func TestExactMentionTabCompletes(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "thanks @bob")
	press(m, "tab")
	if len(f.sent) != 0 || m.input.Value() != "thanks @bob " || m.compShown() {
		t.Fatalf("Tab on exact @bob: sent %+v input %q", f.sent, m.input.Value())
	}
}

func TestExactCommandEnterCompletes(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "/pins")
	if !m.compShown() || m.comp.items[m.comp.sel].text != "/pins" {
		t.Fatalf("/pins not highlighted: %v sel %d", compTexts(m), m.comp.sel)
	}
	press(m, "enter")
	if v := m.input.Value(); v != "/pins " || m.mode != modeNormal || len(f.sent) != 0 {
		t.Fatalf("exact /pins + Enter: %q mode %v sent %+v", v, m.mode, f.sent)
	}
}

// The popup belongs to the place it was opened in: switching channels and
// back doesn't bring it back, and a single-rune paste never opens it.
func TestPopupDoesntFollowChannels(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "/he")
	if !m.compShown() {
		t.Fatal("menu not open")
	}
	press(m, "alt+down")
	if m.compShown() {
		t.Fatal("menu shown in the next channel")
	}
	press(m, "alt+up")
	if m.input.Value() != "/he" {
		t.Fatalf("draft not restored: %q", m.input.Value())
	}
	if m.compShown() {
		t.Fatal("stale menu came back with the draft")
	}
	press(m, "ctrl+u")
	paste(m, "/")
	if m.compShown() {
		t.Fatal("single-rune paste opened the menu")
	}
	press(m, "ctrl+u")
	// Items follow a reload while shown.
	typeKeys(m, "@da")
	if m.compShown() {
		t.Fatal("no user da yet")
	}
	typeKeys(m, "v")
	f.users = append(f.users, proto.User{ID: 9, Name: "dave"})
	m.reloadUsers()
	typeKeys(m, "e")
	if got := compTexts(m); len(got) != 1 || got[0] != "@dave" {
		t.Fatalf("after reload: %v", got)
	}
}

func TestPopupFitsSplitThread(t *testing.T) {
	for _, sz := range [][2]int{{40, 12}, {100, 14}, {200, 60}, {20, 6}} {
		m := newTestModel(t, sz[0], sz[1])
		run(m, m.openThread(103))
		typeKeys(m, "/")
		checkView(t, m, "thread /")
		press(m, "up")
		checkView(t, m, "thread / up")
		press(m, "ctrl+u")
		typeKeys(m, "@")
		checkView(t, m, "thread @")
	}
}

// An exact name is highlighted whatever its membership or kind, so the first
// Enter sends what was typed instead of rewriting it to a longer name.
func TestExactNameRanksFirst(t *testing.T) {
	t.Run("user not here", func(t *testing.T) {
		m, f := newTestModelFake(t, 80, 24)
		m.userList = append(m.userList, proto.User{ID: 5, Name: "bo"}) // not in #general; bob is
		typeKeys(m, "hi @bo")
		if got := compTexts(m); len(got) < 2 || got[0] != "@bo" || got[1] != "@bob" {
			t.Fatalf("@bo: %v", got)
		}
		press(m, "enter")
		if len(f.sent) != 1 || f.sent[0].Body != "hi @bo" {
			t.Fatalf("Enter: sent %+v input %q", f.sent, m.input.Value())
		}
	})
	t.Run("group before longer user", func(t *testing.T) {
		m, f := newTestModelFake(t, 80, 24)
		m.userList = append(m.userList, proto.User{ID: 5, Name: "oncall-x"})
		m.groups = []proto.Group{{Name: "oncall", Members: []uint64{2, 3}}}
		typeKeys(m, "ping @oncall")
		if got := compTexts(m); len(got) < 2 || got[0] != "@oncall" {
			t.Fatalf("@oncall: %v", got)
		}
		press(m, "enter")
		if len(f.sent) != 1 || f.sent[0].Body != "ping @oncall" {
			t.Fatalf("Enter: sent %+v input %q", f.sent, m.input.Value())
		}
	})
	t.Run("unjoined channel", func(t *testing.T) {
		m, f := newTestModelFake(t, 80, 24)
		m.chans = append(m.chans,
			client.ChannelView{Channel: proto.Channel{ID: 30, Name: "dev", Kind: proto.ChanPublic}, Display: "dev"},
			client.ChannelView{Channel: proto.Channel{ID: 31, Name: "dev-ops", Kind: proto.ChanPublic, Members: []uint64{1}}, Display: "dev-ops", Member: true})
		typeKeys(m, "see #dev")
		if got := compTexts(m); len(got) < 2 || got[0] != "#dev" || got[1] != "#dev-ops" {
			t.Fatalf("#dev: %v", got)
		}
		press(m, "enter")
		if len(f.sent) != 1 || f.sent[0].Body != "see #dev" {
			t.Fatalf("Enter: sent %+v input %q", f.sent, m.input.Value())
		}
	})
	// The server refuses a group named like a user (PROTOCOL.md group.set),
	// but should both arrive the order is still fixed: the user first.
	t.Run("user and group same name", func(t *testing.T) {
		m, f := newTestModelFake(t, 80, 24)
		m.groups = []proto.Group{{Name: "bob", Members: []uint64{2, 3}}}
		typeKeys(m, "hi @bob")
		it := m.comp.items
		if len(it) != 2 || it[0].text != "@bob" || it[0].desc != "" || !strings.HasPrefix(it[1].desc, "group") {
			t.Fatalf("@bob: %+v", it)
		}
		press(m, "enter")
		if len(f.sent) != 1 || f.sent[0].Body != "hi @bob" {
			t.Fatalf("Enter: sent %+v input %q", f.sent, m.input.Value())
		}
	})
}
