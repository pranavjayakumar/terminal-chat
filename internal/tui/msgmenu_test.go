package tui

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
)

// selectID selects message id in the focused pane.
func selectID(t *testing.T, m *Model, id uint64) {
	t.Helper()
	if m.mode != modeSelect {
		m.enterSelect()
	}
	p := m.focus()
	for i, x := range p.msgs {
		if x.ID == id {
			p.sel = i
			m.ensureVisible(p)
			return
		}
	}
	t.Fatalf("no message %d", id)
}

// selectLocal selects the unsent message with local id l.
func selectLocal(t *testing.T, m *Model, l int64) {
	t.Helper()
	m.enterSelect()
	for i, x := range m.main.msgs {
		if x.ID == 0 && x.Local == l {
			m.main.sel = i
			return
		}
	}
	t.Fatalf("no local message %d", l)
}

func menuLabels(m *Model) []string {
	var out []string
	for _, it := range m.mm.items {
		out = append(out, it.label)
	}
	return out
}

func menuIndex(t *testing.T, m *Model, label string) int {
	t.Helper()
	for i, it := range m.mm.items {
		if it.label == label {
			return i
		}
	}
	t.Fatalf("menu %v lacks %q", menuLabels(m), label)
	return -1
}

// Each message gets only the items that do something for it.
func TestMsgMenuItems(t *testing.T) {
	m := newTestModel(t, 120, 40)
	cases := []struct {
		pick func()
		want []string
	}{
		{func() { selectID(t, m, 103) }, []string{"Reply in thread", "React", "Edit", "Copy text", "Pin", "Delete"}},
		{func() { selectID(t, m, 104) }, []string{"Reply in thread", "React", "Copy text", "Pin"}},
		{func() { selectID(t, m, 106) }, []string{"Copy text"}},
		{func() { selectLocal(t, m, 7) }, []string{"Copy text"}},
		{func() { selectLocal(t, m, 8) }, []string{"Retry send", "Copy text", "Discard"}},
	}
	for i, c := range cases {
		c.pick()
		press(m, "m")
		if m.mode != modeMsgMenu {
			t.Fatalf("case %d: m didn't open the menu (mode %v)", i, m.mode)
		}
		if got := menuLabels(m); !slices.Equal(got, c.want) {
			t.Errorf("case %d: menu %v, want %v", i, got, c.want)
		}
		plain := checkView(t, m, "menu")
		for _, l := range c.want {
			if !strings.Contains(plain, l) {
				t.Errorf("case %d: %q not drawn\n%s", i, l, plain)
			}
		}
		press(m, "esc")
		if m.mode != modeSelect {
			t.Fatalf("case %d: esc left mode %v", i, m.mode)
		}
	}
	// Pinned messages offer Unpin; Delete is drawn in the error color.
	m.main.msgs[findRef(m.main.msgs, "3~i103")].Pinned = true
	selectID(t, m, 103)
	press(m, "enter")
	menuIndex(t, m, "Unpin")
	x, y := find(t, m, "mm:"+strconv.Itoa(menuIndex(t, m, "Delete")))
	m.Update(tea.MouseMsg{X: 0, Y: 0, Action: tea.MouseActionMotion})
	m.View()
	found := false
	for xx := x; xx < x+20; xx++ {
		if cl := m.lastCanvas.at(xx, y); cl.s == "D" && cl.fg == m.st.th.err {
			found = true
		}
	}
	if !found {
		t.Error("Delete isn't in the error color")
	}
}

// Every item runs the select-mode key it shows, by Enter, its letter or a click.
func TestMsgMenuRunsEachItem(t *testing.T) {
	type check func(m *Model, f *fakeClient) bool
	cases := []struct {
		label, key string
		ok         check
	}{
		{"Reply in thread", "r", func(m *Model, f *fakeClient) bool { return m.thread.open() && m.thread.root == 103 }},
		{"React", "+", func(m *Model, f *fakeClient) bool { return m.mode == modeEmoji && m.emo.react && m.target.ID == 103 }},
		{"Edit", "e", func(m *Model, f *fakeClient) bool { return m.editing == 103 }},
		{"Copy text", "y", func(m *Model, f *fakeClient) bool {
			return strings.Contains(m.opt.Out.(*syncBuf).String(), "\x1b]52;c;")
		}},
		{"Pin", "p", func(m *Model, f *fakeClient) bool { return f.called("pin") }},
		{"Delete", "d", func(m *Model, f *fakeClient) bool { return m.mode == modeConfirmDelete && m.target.ID == 103 }},
	}
	for _, how := range []string{"enter", "letter", "click"} {
		for _, c := range cases {
			m, f := newTestModelFake(t, 120, 40)
			selectID(t, m, 103)
			press(m, "m")
			i := menuIndex(t, m, c.label)
			if m.mm.items[i].key != c.key {
				t.Fatalf("%s shows key %q", c.label, m.mm.items[i].key)
			}
			switch how {
			case "enter":
				for range i {
					press(m, "down")
				}
				press(m, "enter")
			case "letter":
				press(m, c.key)
			case "click":
				click(t, m, "mm:"+strconv.Itoa(i))
			}
			if !c.ok(m, f) {
				t.Errorf("%s by %s: didn't run (mode %v, calls %v)", c.label, how, m.mode, f.calls)
			}
			if m.mode == modeMsgMenu {
				t.Errorf("%s by %s: menu still open", c.label, how)
			}
			checkView(t, m, c.label+" by "+how)
		}
	}
	// Delete keeps its confirmation.
	m, f := newTestModelFake(t, 120, 40)
	selectID(t, m, 103)
	press(m, "m", "d")
	if f.called("delete") {
		t.Fatal("deleted without confirming")
	}
	press(m, "y")
	if !f.called("delete") {
		t.Fatal("confirmed delete not sent")
	}
}

// ⋯ opens the menu by the button; Esc, a click outside or ⋯ again closes it.
func TestMsgMenuMouse(t *testing.T) {
	m, _ := newTestModelFake(t, 120, 40)
	x, y := find(t, m, "msg:main:")
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
	bx, by := find(t, m, "tb:")
	for a := m.lastCanvas.actionAt(bx, by); !strings.HasSuffix(a, ":more"); a = m.lastCanvas.actionAt(bx, by) {
		bx++
	}
	press := func() {
		_, cmd := m.Update(tea.MouseMsg{X: bx, Y: by, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		run(m, cmd)
	}
	press()
	if m.mode != modeMsgMenu {
		t.Fatalf("⋯: mode %v", m.mode)
	}
	m.View()
	r, n, _ := m.msgMenuRect(m.geo())
	if n == 0 || (r.y0 != by+1 && r.y1 != by) {
		t.Fatalf("menu %+v not by the button at row %d", r, by)
	}
	// Hover lights a row and moves the selection with it.
	hx, hy := find(t, m, "mm:2")
	m.Update(tea.MouseMsg{X: hx, Y: hy, Action: tea.MouseActionMotion})
	m.View()
	if m.mm.sel != 2 || m.lastCanvas.at(hx, hy).bg != m.st.th.accent {
		t.Fatalf("hover: sel %d", m.mm.sel)
	}
	press() // ⋯ again closes it
	if m.mode != modeSelect {
		t.Fatalf("⋯ again: mode %v", m.mode)
	}
	press()
	if m.mode != modeMsgMenu {
		t.Fatal("⋯ didn't reopen")
	}
	click(t, m, "chan:11") // a click outside closes it and still acts
	if m.mode == modeMsgMenu || m.active != 11 {
		t.Fatalf("outside click: mode %v active %d", m.mode, m.active)
	}
	checkView(t, m, "closed")
}

// The thread pane has the menu too, for its own messages.
func TestMsgMenuThreadPane(t *testing.T) {
	for _, split := range []bool{false, true} {
		m, f := newTestModelFake(t, 140, 40)
		m.ui.Split = split
		run(m, m.openThread(103))
		m.layoutInput()
		selectID(t, m, 200)
		press(m, "m")
		if m.mode != modeMsgMenu || slices.Contains(menuLabels(m), "Edit") {
			t.Fatalf("split %v: menu %v mode %v", split, menuLabels(m), m.mode)
		}
		plain := checkView(t, m, "thread menu")
		if !strings.Contains(plain, "Copy text") {
			t.Fatalf("split %v: menu not drawn\n%s", split, plain)
		}
		press(m, "p")
		if !f.called("pin") {
			t.Fatalf("split %v: p in the thread menu didn't pin", split)
		}
		// ⋯ on a thread message.
		x, y := find(t, m, "msg:thr:")
		m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
		click(t, m, "tb:thr:")
		bx, by := find(t, m, "tb:thr:")
		for a := m.lastCanvas.actionAt(bx, by); !strings.HasSuffix(a, ":more"); a = m.lastCanvas.actionAt(bx, by) {
			bx++
		}
		_, cmd := m.Update(tea.MouseMsg{X: bx, Y: by, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		run(m, cmd)
		if m.mode != modeMsgMenu {
			t.Fatalf("split %v: ⋯ in the thread: mode %v", split, m.mode)
		}
		checkView(t, m, "thread ⋯")
	}
}

// The menu stays inside the screen at every size, in both skins, opened on
// every message (the newest one makes it flip above the button).
func TestMsgMenuFits(t *testing.T) {
	sizes := [][2]int{{20, 6}, {40, 12}, {40, 24}, {69, 20}, {80, 24}, {100, 30}, {120, 40}, {160, 48}, {200, 60}}
	for _, skin := range []string{"modern", "retro"} {
		for _, split := range []bool{false, true} {
			for _, sz := range sizes {
				m := newTestModel(t, sz[0], sz[1])
				m.setSkin(skin)
				m.ui.Split = split
				m.layoutInput()
				m.enterSelect()
				for i := range m.main.msgs {
					m.main.sel = i
					m.ensureVisible(&m.main)
					m.mode = modeSelect
					press(m, "m")
					if m.mode != modeMsgMenu {
						if sz[0] >= 30 {
							t.Fatalf("%s %v: no menu on %d", skin, sz, i)
						}
						continue // too small: a notice names the keys instead
					}
					checkView(t, m, "menu")
					g := m.geo()
					r, n, _ := m.msgMenuRect(g)
					if n <= 0 {
						t.Fatalf("%s %v split %v msg %d: menu open but not drawn", skin, sz, split, i)
					}
					if n > 0 && (r.x0 < g.L || r.x1 > m.w || r.y0 < g.top || r.y1 > g.statusY || r.y0 < 0) {
						t.Fatalf("%s %v split %v msg %d: menu %+v outside the frame", skin, sz, split, i, r)
					}
					if n > 0 && r.y1 > m.mm.ay && r.y0 <= m.mm.ay && r.y1-r.y0 < g.statusY-g.top {
						t.Fatalf("%s %v msg %d: menu %+v covers its button row %d", skin, sz, i, r, m.mm.ay)
					}
					press(m, "esc")
				}
			}
		}
	}
}

// The menu fades in: frames keep coming until its text shows.
func TestMsgMenuFadeSchedulesFrames(t *testing.T) {
	m, _, clk := motionModel(t, 120, 40, "")
	selectID(t, m, 104)
	press(m, "m")
	if !m.fastWanted() {
		t.Fatal("no frame scheduled for the fade-in")
	}
	clk.add(time.Second)
	if m.fastWanted() {
		t.Fatal("frames still wanted after the fade")
	}
	if plain := checkView(t, m, "faded in"); !strings.Contains(plain, "Copy text") {
		t.Fatalf("menu text not shown\n%s", plain)
	}
}

// One highlight: keys move it away from a resting pointer.
func TestMsgMenuOneHighlight(t *testing.T) {
	m := newTestModel(t, 120, 40)
	selectID(t, m, 103)
	press(m, "m")
	hx, hy := find(t, m, "mm:1")
	m.Update(tea.MouseMsg{X: hx, Y: hy, Action: tea.MouseActionMotion})
	press(m, "down", "down")
	m.View()
	if m.mm.sel != 3 {
		t.Fatalf("sel %d", m.mm.sel)
	}
	if m.lastCanvas.at(hx, hy).bg == m.st.th.accent {
		t.Fatal("the row under the pointer stays lit after the keys moved on")
	}
	x, y := find(t, m, "mm:3")
	if m.lastCanvas.at(x, y).bg != m.st.th.accent {
		t.Fatal("the selected row isn't lit")
	}
}

// With a thread open, ⋯ on a main-pane message never opens a menu for
// another message.
func TestMsgMenuMainWhileThreadOpen(t *testing.T) {
	m := newTestModel(t, 160, 40)
	m.ui.Split = true
	run(m, m.openThread(103))
	m.layoutInput()
	selectID(t, m, 200) // a thread message is selected
	x, y := find(t, m, "msg:main:")
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
	find(t, m, "tb:main:") // the toolbar shows, without ⋯
	c := m.lastCanvas
	for yy := 0; yy < c.h; yy++ {
		for xx := 0; xx < c.w; xx++ {
			if a := c.actionAt(xx, yy); strings.HasPrefix(a, "tb:main:") && strings.HasSuffix(a, ":more") {
				t.Fatalf("⋯ on a main-pane message while the thread has the selection (%d,%d)", xx, yy)
			}
		}
	}
	_, cmd := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	run(m, cmd)
	if m.mode == modeMsgMenu {
		t.Fatalf("menu opened on %s for a main-pane click", m.mm.ref)
	}
}

// Keys the menu doesn't list close it and do what they do in select mode.
func TestMsgMenuPassesOtherKeys(t *testing.T) {
	m := newTestModel(t, 120, 40)
	selectID(t, m, 104)
	press(m, "m", "v")
	if m.mode != modeViewer {
		t.Fatalf("v: mode %v", m.mode)
	}
	press(m, "esc")
	selectID(t, m, 104)
	press(m, "m", "e") // not mine: select mode's own answer
	if m.mode != modeSelect || !strings.Contains(m.notice, "only edit your own") {
		t.Fatalf("e on another's message: mode %v notice %q", m.mode, m.notice)
	}
	press(m, "m", "ctrl+k")
	if m.mode != modeSwitcher {
		t.Fatalf("ctrl+k: mode %v", m.mode)
	}
}

// The rows follow the message: deleted meanwhile, they shrink; gone, the
// menu closes; a pending message once sent gets the full menu.
func TestMsgMenuFollowsTheMessage(t *testing.T) {
	m, f := newTestModelFake(t, 120, 40)
	selectID(t, m, 104)
	press(m, "m")
	for i := range f.msgs[10] {
		if f.msgs[10][i].ID == 104 {
			f.msgs[10][i].Deleted = true
		}
	}
	m.reloadPane(&m.main)
	if got := menuLabels(m); !slices.Equal(got, []string{"Copy text"}) || m.mode != modeMsgMenu {
		t.Fatalf("deleted meanwhile: %v mode %v", got, m.mode)
	}
	f.msgs[10] = slices.DeleteFunc(f.msgs[10], func(x client.Message) bool { return x.ID == 104 })
	m.reloadPane(&m.main)
	if m.mode == modeMsgMenu {
		t.Fatal("menu stays open on a message that is gone")
	}
	selectLocal(t, m, 7)
	press(m, "m")
	for i := range f.msgs[10] {
		if f.msgs[10][i].Local == 7 {
			f.msgs[10][i].ID, f.msgs[10][i].Pending = 900, false
		}
	}
	m.reloadPane(&m.main)
	if !slices.Contains(menuLabels(m), "Edit") {
		t.Fatalf("sent meanwhile: %v", menuLabels(m))
	}
	press(m, "y")
	if !strings.Contains(m.opt.Out.(*syncBuf).String(), "\x1b]52;c;") {
		t.Fatal("copy did nothing after the send")
	}
}
