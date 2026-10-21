package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

var altE = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}, Alt: true}

// selectMsg enters select mode on message id of the channel pane.
func selectMsg(t *testing.T, m *Model, id uint64) {
	t.Helper()
	press(m, "up")
	for i, x := range m.main.msgs {
		if x.ID == id {
			m.main.sel = i
		}
	}
	m.ensureVisible(&m.main)
	if sel, ok := m.selected(); !ok || sel.ID != id {
		t.Fatalf("message %d not selected", id)
	}
}

// pickedItem is the emoji the picker has selected.
func pickedItem(m *Model) string {
	if m.emo.sel < 0 || m.emo.sel >= len(m.emo.items) {
		return ""
	}
	return m.emo.items[m.emo.sel].e
}

// ---- ":" completion ----

// ":" plus two letters opens the popup with emoji; the frequently used one
// first; Tab and Enter insert it and close the popup.
func TestColonCompletion(t *testing.T) {
	m := newTestModel(t, 120, 40)
	typeKeys(m, "ship it :f")
	if m.compShown() {
		t.Fatal("open after one letter")
	}
	typeKeys(m, "i")
	if !m.compShown() || m.comp.kind != ':' {
		t.Fatalf("not open after :fi (kind %q)", m.comp.kind)
	}
	if it := m.comp.items[0]; it.text != "🔥" || it.desc != "frequent" {
		t.Fatalf("first item %+v", it)
	}
	v := checkView(t, m, ":fi")
	if !strings.Contains(v, " Emoji ") || !strings.Contains(v, ":fire:") || !strings.Contains(v, "⇥/⏎ insert") {
		t.Fatalf("popup:\n%s", v)
	}
	press(m, "down")
	want := m.comp.items[1].text
	press(m, "enter")
	if got := m.input.Value(); got != "ship it "+want+" " || m.compShown() {
		t.Fatalf("Enter: %q, shown %v", got, m.compShown())
	}
	if m.f().called("send") || len(m.f().sent) != 0 {
		t.Fatal("Enter sent the message")
	}
	press(m, "ctrl+u")
	typeKeys(m, "a :+1")
	press(m, "tab")
	if got := m.input.Value(); got != "a 👍 " {
		t.Fatalf("Tab: %q", got)
	}
	// Mid-line: the word is replaced, the text after it kept.
	press(m, "ctrl+u")
	typeKeys(m, "x  y")
	press(m, "left", "left")
	typeKeys(m, ":rocke")
	press(m, "tab")
	if got := m.input.Value(); got != "x 🚀 y" {
		t.Fatalf("mid-line: %q", got)
	}
}

// The matched letters are drawn in the accent; a click picks a row.
func TestColonCompletionLooks(t *testing.T) {
	m := newTestModel(t, 120, 40)
	typeKeys(m, "go :rock")
	m.View()
	c := m.lastCanvas
	r, _, _ := m.popupRect(m.geo())
	if w := r.x1 - r.x0; w > emojiPopupW {
		t.Fatalf("popup %d wide", w)
	}
	found := false
	for y := r.y0; y < r.y1; y++ {
		for x := r.x0; x < r.x1; x++ {
			if cl := c.at(x, y); cl.s == "r" && strings.HasPrefix(c.actionAt(x, y), "comp:") {
				// the r of "rock" in a row that isn't the highlighted one
				if y != r.y0+2 && cl.fg == m.st.th.accent && cl.attr&aBold != 0 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("no accent letters in the rows")
	}
	want := m.comp.items[1].text
	click(t, m, "comp:1")
	if got := m.input.Value(); got != "go "+want+" " {
		t.Fatalf("click: %q", got)
	}
}

// No popup inside code, after a letter (a URL, a time), or after a whole
// :name:.
func TestColonCompletionDoesNotTrigger(t *testing.T) {
	for _, in := range []string{
		"see http://fire", "at 10:fi", "`x :fi", "```\n:fi", "a:fi", ":fire:", "::fi", ":f",
		"`a` b `:fi",
	} {
		m := newTestModel(t, 120, 40)
		m.input.SetValue(in[:len(in)-1])
		m.input.CursorEnd()
		typeKeys(m, in[len(in)-1:])
		if m.compShown() && m.comp.kind == ':' {
			t.Errorf("%q opened the emoji popup", in)
		}
	}
	m := newTestModel(t, 120, 40)
	typeKeys(m, "`a` :fi")
	if !m.compShown() || m.comp.kind != ':' {
		t.Error("closed code span: no popup")
	}
}

// ---- the picker: keys ----

func TestPickerReactKeys(t *testing.T) {
	m, f := newTestModelFake(t, 120, 40)
	selectMsg(t, m, 105)
	press(m, "+")
	if m.mode != modeEmoji || !m.emo.react {
		t.Fatalf("mode %v", m.mode)
	}
	v := checkView(t, m, "open")
	if !strings.Contains(v, "React to carol") || !strings.Contains(v, "Frequently used") || !strings.Contains(v, "⏎ react") {
		t.Fatalf("picker:\n%s", v)
	}
	if got := pickedItem(m); got != freqSeed[0] {
		t.Fatalf("first selection %s", got)
	}
	press(m, "right", "right")
	if got := pickedItem(m); got != freqSeed[2] {
		t.Fatalf("right right: %s", got)
	}
	press(m, "down") // into the smileys, same column
	if it := m.emo.items[m.emo.sel]; it.sec != 0 || m.emo.sel-m.emo.rows[m.pickRowOf(m.emo.sel)].from != 2 {
		t.Fatalf("down: %+v", it)
	}
	press(m, "up", "left", "left", "left")
	if m.emo.sel != 0 {
		t.Fatalf("left at the start: %d", m.emo.sel)
	}
	// Tab walks the categories and wraps; Shift-Tab goes back.
	for i := range emojiCats {
		press(m, "tab")
		if sec := m.pickSection(); sec != i {
			t.Fatalf("tab %d: section %d", i, sec)
		}
		if r := m.pickRowOf(m.emo.sel); r < m.emo.top || r >= m.emo.top+m.pickSize(m.geo()).grid {
			t.Fatalf("tab %d: selection off screen", i)
		}
		checkView(t, m, "tab")
	}
	press(m, "tab")
	if m.pickSection() != pickSecFreq {
		t.Fatal("tab didn't wrap")
	}
	press(m, "shift+tab")
	if m.pickSection() != len(emojiCats)-1 {
		t.Fatal("shift+tab")
	}
	press(m, "pgup", "pgdown", "pgdown")
	checkView(t, m, "pages")
	// Typing filters; Enter reacts with the top match and goes back to
	// select mode.
	typeKeys(m, ":tada")
	if m.emo.query != ":tada" || pickedItem(m) != "🎉" {
		t.Fatalf("query %q picked %s", m.emo.query, pickedItem(m))
	}
	if v := checkView(t, m, "search"); !strings.Contains(v, ":tada") || !strings.Contains(v, "match") {
		t.Fatalf("search:\n%s", v)
	}
	press(m, "enter")
	if !f.called("react 🎉") || m.mode != modeSelect {
		t.Fatalf("calls %v mode %v", f.calls, m.mode)
	}
	// Esc closes without reacting; Backspace and Ctrl-U edit the search.
	press(m, "+")
	typeKeys(m, "zzzz")
	if v := checkView(t, m, "no match"); !strings.Contains(v, "no emoji match") {
		t.Fatalf("no match:\n%s", v)
	}
	press(m, "enter") // nothing to pick
	if m.mode != modeEmoji {
		t.Fatal("Enter with no match closed the picker")
	}
	press(m, "backspace")
	if m.emo.query != "zzz" {
		t.Fatalf("backspace: %q", m.emo.query)
	}
	press(m, "ctrl+u")
	if m.emo.query != "" || pickedItem(m) != m.freqList()[0] || m.freqList()[0] != "🎉" {
		t.Fatalf("ctrl+u: %q", m.emo.query)
	}
	n := len(f.calls)
	press(m, "esc")
	if m.mode != modeSelect || len(f.calls) != n {
		t.Fatalf("esc: mode %v calls %v", m.mode, f.calls[n:])
	}
}

// My own reaction toggles off, like clicking its pill.
func TestPickerTakesBackMyReaction(t *testing.T) {
	m, f := newTestModelFake(t, 120, 40)
	selectMsg(t, m, 103)
	press(m, "+")
	if v := checkView(t, m, "mine"); !strings.Contains(v, "⏎ remove") {
		t.Fatalf("preview of an emoji I reacted with:\n%s", v)
	}
	press(m, "enter")
	if !f.called("unreact 👍") {
		t.Fatalf("calls %v", f.calls)
	}
}

// A message that isn't sent can't take a reaction: a notice, no picker.
func TestPickerNotOnUnsent(t *testing.T) {
	m := newTestModel(t, 120, 40)
	press(m, "up") // the failed message
	press(m, "+")
	if m.mode == modeEmoji || !strings.Contains(m.notice, "not sent") {
		t.Fatalf("mode %v notice %q", m.mode, m.notice)
	}
}

// ⌥E (and the ´ macOS types for Option-E) inserts at the cursor.
func TestPickerInsert(t *testing.T) {
	for _, k := range []tea.KeyMsg{altE, {Type: tea.KeyRunes, Runes: []rune{'´'}}} {
		m := newTestModel(t, 120, 40)
		typeKeys(m, "ab")
		press(m, "left")
		send(m, k)
		if m.mode != modeEmoji || m.emo.react {
			t.Fatalf("%q: mode %v", k.String(), m.mode)
		}
		v := checkView(t, m, "insert")
		if !strings.Contains(v, "Insert emoji") || !strings.Contains(v, "⏎ insert") {
			t.Fatalf("picker:\n%s", v)
		}
		// Above the input's left edge, a blank row between.
		g := m.geo()
		r := m.pickPlace(g, m.pickSize(g))
		if r.x0 != g.inX || r.y1 != g.inY-1 {
			t.Fatalf("at %+v, input at %d,%d", r, g.inX, g.inY)
		}
		typeKeys(m, "rocket")
		press(m, "enter")
		if got := m.input.Value(); got != "a🚀b" || m.mode != modeNormal {
			t.Fatalf("%q: input %q mode %v", k.String(), got, m.mode)
		}
		send(m, k)
		send(m, k) // ⌥E again closes it
		if m.mode != modeNormal || m.input.Value() != "a🚀b" {
			t.Fatalf("toggle: mode %v input %q", m.mode, m.input.Value())
		}
	}
}

// In select mode ⌥E reacts, like +; the palette has a row for the picker.
func TestPickerOtherWaysIn(t *testing.T) {
	m := newTestModel(t, 120, 40)
	selectMsg(t, m, 105)
	send(m, altE)
	if m.mode != modeEmoji || !m.emo.react {
		t.Fatal("⌥E in select mode")
	}
	press(m, "esc", "esc")
	press(m, "ctrl+k")
	typeVia(m, "emoji")
	if it := m.pal.shown[m.pal.sel]; it.run != "app:emoji" {
		t.Fatalf("palette row %+v", it)
	}
	press(m, "enter")
	if m.mode != modeEmoji || m.emo.react {
		t.Fatalf("palette: mode %v", m.mode)
	}
	press(m, "esc")
	sheet := strings.Join(sheetPlain(120), "\n")
	for _, s := range []string{"⌥E", "emoji picker", ":fi", "react (emoji picker)"} {
		if !strings.Contains(sheet, s) {
			t.Errorf("keys sheet lacks %q", s)
		}
	}
}

// ---- the picker: mouse ----

func TestPickerMouse(t *testing.T) {
	m, f := newTestModelFake(t, 120, 40)
	// The toolbar's ☺ opens it for that message.
	x, y := find(t, m, "msg:main:")
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
	click(t, m, "tb:")
	if m.mode != modeEmoji || !m.emo.react {
		t.Fatalf("☺: mode %v", m.mode)
	}
	target := m.target.ID
	// Hover lights a cell and the preview follows it.
	x, y = find(t, m, "emo:3")
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
	m.View()
	if bg := m.lastCanvas.at(x, y).bg; bg != m.st.th.accent {
		t.Fatalf("hovered cell bg %v", bg)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, ":"+emojiName(m.emo.items[3].e)+":") {
		t.Fatalf("preview doesn't follow the pointer:\n%s", v)
	}
	// A tab click switches category; the wheel scrolls.
	click(t, m, "emotab:3")
	if m.pickSection() != 3 {
		t.Fatalf("tab click: section %d", m.pickSection())
	}
	top := m.emo.top
	m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if m.emo.top <= top {
		t.Fatal("wheel didn't scroll")
	}
	if r := m.pickRowOf(m.emo.sel); r < m.emo.top {
		t.Fatal("selection left behind by the wheel")
	}
	// A click on an emoji reacts with it.
	x, y = find(t, m, "emo:")
	a := m.lastCanvas.actionAt(x, y)
	i, _ := strconv.Atoi(strings.TrimPrefix(a, "emo:"))
	e := m.emo.items[i].e
	click(t, m, a)
	if !f.called("react "+e) || m.mode != modeSelect || m.target.ID != target {
		t.Fatalf("click %s: calls %v mode %v", a, f.calls, m.mode)
	}
	// ✕ closes; a click outside closes too.
	press(m, "+")
	click(t, m, "emoclose")
	if m.mode != modeSelect {
		t.Fatal("✕")
	}
	press(m, "+")
	_, cmd := m.Update(tea.MouseMsg{X: 1, Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	run(m, cmd)
	if m.mode == modeEmoji {
		t.Fatal("a click outside left it open")
	}
}

// ---- reactions row ----

// ☺+ ends a reaction row and opens the picker for that message; hovering a
// reaction says who reacted.
func TestReactionRowAddAndWho(t *testing.T) {
	m := newTestModel(t, 120, 40)
	selectMsg(t, m, 103)
	press(m, "esc")
	v := checkView(t, m, "row")
	if !strings.Contains(v, "☺+") {
		t.Fatalf("no ☺+ pill:\n%s", v)
	}
	x, y := find(t, m, "react:main:")
	a := m.lastCanvas.actionAt(x, y)
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
	v = checkView(t, m, "hover")
	want := "carol reacted with :tada:"
	if strings.HasSuffix(a, ":👍") {
		want = "you and bob reacted with :+1:"
	}
	if !strings.Contains(v, want) {
		t.Fatalf("no %q on hover of %s:\n%s", want, a, v)
	}
	click(t, m, "addreact:main:")
	if m.mode != modeEmoji || !m.emo.react || m.target.ID != 103 {
		t.Fatalf("☺+: mode %v target %d", m.mode, m.target.ID)
	}
}

func TestReactorsText(t *testing.T) {
	m := newTestModel(t, 80, 24)
	cases := []struct {
		us   []uint64
		e    string
		want string
	}{
		{[]uint64{2}, "🎉", "bob reacted with :tada:"},
		{[]uint64{1, 2}, "👍", "you and bob reacted with :+1:"},
		{[]uint64{2, 3, 4}, "x\x1b[31m", "bob, carol and averyveryverylongusername reacted with x[31m"},
		{[]uint64{1, 2, 3, 4, 5, 6, 7}, "🚀", "you, bob, carol, averyveryverylongusername, user5 and 2 others reacted with :rocket:"},
		{nil, "🚀", ""},
	}
	for _, c := range cases {
		if got := m.reactorsText(c.us, c.e); got != c.want {
			t.Errorf("%v %q: %q, want %q", c.us, c.e, got, c.want)
		}
	}
}

// ---- frequently used ----

func TestFrequentlyUsedPersistsAndCaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui.json")
	newM := func() (*Model, *fakeClient) {
		f := newFake()
		m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: path})
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		return m, f
	}
	m, _ := newM()
	if !slices.Equal(m.freqList(), freqSeed) {
		t.Fatalf("fresh install: %v", m.freqList())
	}
	// Picking moves an emoji up; the file keeps it.
	send(m, altE)
	typeKeys(m, "unicorn")
	press(m, "enter")
	m.input.SetValue("go :rocket: :rocket:")
	press(m, "enter")
	m2, _ := newM()
	got := m2.freqList()
	if got[0] != "🚀" || !slices.Contains(got, "🦄") || len(got) != len(freqSeed)+1 {
		t.Fatalf("after reload: %v", got)
	}
	b, _ := os.ReadFile(path)
	var raw struct {
		Emoji []emojiUse `json:"emoji"`
	}
	if json.Unmarshal(b, &raw) != nil || raw.Emoji[0] != (emojiUse{"🚀", 2}) {
		t.Fatalf("ui.json: %s", b)
	}
	// The newest of equal counts goes first; the list stops at maxFreq.
	n := 0
	for _, d := range emojiTable {
		if n == maxFreq+5 {
			break
		}
		if !slices.Contains(freqSeed, d.e) {
			m2.noteEmoji(d.e)
			n++
		}
	}
	got = m2.freqList()
	if len(got) != maxFreq || got[0] != "🚀" {
		t.Fatalf("cap: %d %v", len(got), got[:3])
	}
	// The picker shows two grid rows of them at most.
	send(m2, altE)
	freq := 0
	for _, it := range m2.emo.items {
		if it.sec == pickSecFreq {
			freq++
		}
	}
	if freq != 2*pickCols {
		t.Fatalf("frequently used row: %d", freq)
	}
	// A hostile ui.json is cleaned on load.
	os.WriteFile(path, []byte(`{"emoji":[{"e":"\u001b[31m","n":3},{"e":"👨‍👩‍👧","n":2},{"e":"🔥","n":1},{"e":"x","n":-1}]}`), 0o644)
	m3, _ := newM()
	if got := m3.freqList(); !slices.Equal(got, []string{"🔥"}) {
		t.Fatalf("hostile ui.json: %q", got)
	}
}

// ---- frames ----

// The picker and the ":" popup fit at every size, in both layouts, while
// they fade and rise in and out.
func TestEmojiFramesFit(t *testing.T) {
	for _, split := range []bool{false, true} {
		for _, sz := range [][2]int{{80, 24}, {120, 40}, {160, 48}, {40, 24}, {40, 12}, {20, 6}, {200, 60}} {
			m, _, clk := motionModel(t, sz[0], sz[1], "")
			m.ui.Split = split
			step := func(name string) {
				t.Helper()
				for range 5 {
					checkView(t, m, name)
					clk.add(40 * time.Millisecond)
				}
			}
			press(m, "up", "up", "up", "+")
			step("react opens")
			typeKeys(m, "ca")
			press(m, "down", "right", "tab", "tab")
			step("react moves")
			press(m, "esc")
			step("react closes")
			press(m, "esc")
			typeVia(m, "hi ")
			send(m, altE)
			step("insert opens")
			press(m, "shift+tab", "up", "pgdown")
			press(m, "enter")
			step("insert picks")
			typeKeys(m, " :fi")
			step("colon popup")
			press(m, "down", "tab")
			step("colon inserted")
			// Thread open: the picker sits by a reply.
			press(m, "ctrl+u", "up")
			for i, x := range m.main.msgs {
				if x.ID == 103 {
					m.main.sel = i
				}
			}
			press(m, "r")
			press(m, "up", "+")
			step("thread react")
			press(m, "esc", "esc", "esc")
		}
	}
}

// The panel is opaque: the frame's cells take the pane's background (no
// message tint behind a corner), the inside is the panel, and the lit
// cell is the accent, in every theme.
func TestPickerOpaqueEveryTheme(t *testing.T) {
	for name, th := range themes {
		m := newTestModel(t, 120, 40)
		m.applyStyles(newStyles(true, m.st.prof, th))
		selectMsg(t, m, 105)
		press(m, "+")
		m.View()
		g := m.geo()
		b := m.pickSize(g)
		r := m.pickPlace(g, b)
		c := m.lastCanvas
		for y := r.y0; y < r.y1; y++ {
			for x := r.x0; x < r.x1; x++ {
				cl := c.at(x, y)
				edge := y == r.y0 || y == r.y1-1 || x == r.x0 || x == r.x1-1
				switch {
				case edge && cl.bg != th.bg:
					t.Fatalf("%s: frame cell %d,%d bg %v", name, x, y, cl.bg)
				case !edge && cl.bg != th.panel && cl.bg != th.element && cl.bg != th.accent:
					t.Fatalf("%s: inside cell %d,%d bg %v shows through", name, x, y, cl.bg)
				}
			}
		}
		lit := 0
		for y := r.y0; y < r.y1; y++ {
			for x := r.x0; x < r.x1; x++ {
				if c.at(x, y).bg == th.accent {
					lit++
				}
			}
		}
		if lit != 4 {
			t.Fatalf("%s: %d accent cells, want one 4-cell emoji", name, lit)
		}
		if contrast(th.accent, th.panel) < 1.5 {
			t.Errorf("%s: the lit cell barely shows (%.2f)", name, contrast(th.accent, th.panel))
		}
	}
}

// Narrow terminals get fewer columns, never a wider panel than the screen.
func TestPickerShrinks(t *testing.T) {
	for _, w := range []int{120, 52, 46, 40, 30, 20, 12, 8} {
		m := newTestModel(t, w, 30)
		send(m, altE)
		checkView(t, m, "narrow")
		g := m.geo()
		b := m.pickSize(g)
		r := m.pickPlace(g, b)
		if r.x1 > w || r.x0 < 0 || (b.cols < pickCols && w >= 46+8) {
			t.Fatalf("%d: %+v cols %d", w, r, b.cols)
		}
		if b.framed && r.x1-r.x0 != 4*b.cols+6 {
			t.Fatalf("%d: uneven margins: %d wide for %d columns", w, r.x1-r.x0, b.cols)
		}
	}
}

// Animations off: it is there at once and gone at once.
func TestPickerInstantWithoutMotion(t *testing.T) {
	m := newTestModel(t, 120, 40)
	send(m, altE)
	if !strings.Contains(checkView(t, m, "open"), "Insert emoji") || m.fastWanted() {
		t.Fatal("not instant")
	}
	press(m, "esc")
	if strings.Contains(checkView(t, m, "closed"), "Insert emoji") {
		t.Fatal("still drawn after Esc")
	}
}

// With motion on it fades in and out, then no frames run.
func TestPickerFades(t *testing.T) {
	m, _, clk := motionModel(t, 120, 40, "")
	send(m, altE)
	if !m.fastWanted() {
		t.Fatal("no frames while it opens")
	}
	clk.add(time.Second)
	press(m, "esc")
	if !strings.Contains(checkView(t, m, "closing"), "Insert emoji") || !m.fastWanted() {
		t.Fatal("no fade out")
	}
	clk.add(time.Second)
	if strings.Contains(checkView(t, m, "closed"), "Insert emoji") || m.fastWanted() {
		t.Fatal("still there")
	}
}

// A channel switch closes it at once.
func TestPickerClosesOnChannelSwitch(t *testing.T) {
	m := newTestModel(t, 120, 40)
	send(m, altE)
	run(m, m.openChannel(11))
	if m.mode != modeNormal || m.emo.open {
		t.Fatalf("mode %v open %v", m.mode, m.emo.open)
	}
}

// Hostile search text never reaches the screen raw.
func TestPickerHostileQuery(t *testing.T) {
	m := newTestModel(t, 120, 40)
	send(m, altE)
	send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("\x1b[31m‮evil\u202e" + strings.Repeat("ab", 40)), Paste: true})
	checkView(t, m, "hostile")
	if len([]rune(m.emo.query)) > pickMaxQ || strings.ContainsAny(m.emo.query, "\x1b‮") {
		t.Fatalf("query %q", m.emo.query)
	}
	// A pasted emoji that isn't in the table is still offered.
	press(m, "ctrl+u")
	send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("🫠"), Paste: true})
	if pickedItem(m) != "🫠" {
		t.Fatalf("literal: %q", pickedItem(m))
	}
}
