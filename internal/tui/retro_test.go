package tui

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// retroPrefs writes a ui.json with the Retro skin on.
func retroPrefs(t *testing.T, extra string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ui.json")
	if err := os.WriteFile(p, []byte(`{"skin":"retro"`+extra+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func retroModel(t *testing.T, w, h int) (*Model, *fakeClient) {
	t.Helper()
	f := newFake()
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: retroPrefs(t, "")})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m, f
}

// The qbasic dialog colors pass the same checks as a theme's panel.
func TestDialogColors(t *testing.T) {
	for _, name := range themeOrder {
		d := themes[name].dialog
		if d == nil {
			continue
		}
		if contrast(d.fg, d.panel) < 7 || contrast(d.dim, d.panel) < 4.5 || contrast(d.faint, d.panel) < 3 || contrast(d.accentFg, d.accent) < 4.5 {
			t.Errorf("%s: dialog colors too weak", name)
		}
	}
}

// Skin and theme are independent: one theme list for every skin, and a
// skin only suggests its theme to someone who never picked one.
func TestSkinAndThemeIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui.json")
	open := func() *Model {
		m := New(context.Background(), newFake(), Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: path})
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		return m
	}
	m := open()
	m.setSkin("retro")
	if m.skin().id != "retro" || m.st.th.name != "turbo" {
		t.Fatalf("retro on the default theme: %q", m.st.th.name)
	}
	m.setTheme("sky") // any theme works in any skin
	m.setSkin("modern")
	if m.st.th.name != "sky" {
		t.Fatalf("modern overrode the theme: %q", m.st.th.name)
	}
	m.setTheme("graphite") // picked on purpose, even though it is the default
	m.setSkin("retro")
	if m.st.th.name != "graphite" {
		t.Fatalf("retro overrode a picked theme: %q", m.st.th.name)
	}
	runInput(t, m, "/theme qbasic")
	m.setSkin("modern")
	if m.st.th.name != "qbasic" {
		t.Fatal("a retro palette is not usable in the modern skin")
	}
	m.setSkin("retro")
	b, _ := os.ReadFile(path)
	for _, want := range []string{`"skin": "retro"`, `"theme": "qbasic"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("ui.json lacks %s: %s", want, b)
		}
	}
	again := open()
	if again.skin().id != "retro" || again.st.th.name != "qbasic" {
		t.Fatalf("restart: %s %q", again.skin().id, again.st.th.name)
	}
	// The palette lists every theme in every skin, and switches skins.
	for _, sk := range skins {
		again.setSkin(sk.id)
		again.openPalette("settings")
		var shown []string
		for _, it := range again.pal.shown {
			if it.group == "Theme" {
				shown = append(shown, it.label)
			}
		}
		if strings.Join(shown, ",") != strings.Join(themeOrder, ",") {
			t.Fatalf("%s: palette themes %v", sk.id, shown)
		}
		press(again, "esc")
	}
	again.openPalette("settings")
	for i, it := range again.pal.shown {
		if it.run == "app:skin:retro" {
			again.pal.sel = i
			again.palRun(it)
		}
	}
	if again.skin().id != "retro" || again.st.th.name != "qbasic" {
		t.Fatal("palette skin switch")
	}
}

// Every skin with every theme: frames fit at every size, and text stays
// readable on what it is drawn on.
func TestEverySkinEveryTheme(t *testing.T) {
	for _, sk := range skins {
		for _, name := range themeOrder {
			for _, sz := range [][2]int{{160, 48}, {80, 24}, {40, 12}} {
				p := filepath.Join(t.TempDir(), "ui.json")
				os.WriteFile(p, []byte(`{"skin":"`+sk.id+`","theme":"`+name+`"}`), 0o600)
				f := newFake()
				f.typing[10] = []uint64{3}
				m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: p})
				m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
				m.reloadTyping()
				id := sk.id + "/" + name
				readable(t, m, id+" main", rect{0, 0, m.w, m.h})
				typeVia(m, "/")
				readable(t, m, id+" popup", rect{0, 0, m.w, m.h})
				press(m, "esc", "esc")
				run(m, m.openThread(103))
				m.layoutInput()
				readable(t, m, id+" thread", rect{0, 0, m.w, m.h})
				press(m, "esc")
				for _, d := range []string{"ctrl+k", "f1"} {
					press(m, d)
					readable(t, m, id+" "+d, m.dialogBox(76, 30))
					press(m, "esc")
				}
				runInput(t, m, "/settings")
				readable(t, m, id+" settings", m.dialogBox(116, 38))
				press(m, "esc")
			}
		}
	}
}

// readable checks the frame fits and every glyph inside r reads against
// its own background. Shapes (pattern, pill ends, bars, frames) and
// shadowed cells are decoration, not text.
func readable(t *testing.T, m *Model, step string, r rect) {
	t.Helper()
	checkView(t, m, step)
	th := m.st.th
	if m.overlayShown() && m.dst != nil {
		th = m.dst.th
	}
	c := m.lastCanvas
	const shapes = " ░▐▌▎▏│┃┇║═╔╗╚╝╭╮╰╯─━·•⣀"
	for y := max(r.y0, 0); y < min(r.y1, c.h); y++ {
		for x := max(r.x0, 0); x < min(r.x1, c.w); x++ {
			cl := c.at(x, y)
			if cl.w == 0 || strings.Contains(shapes, cl.s) || cl.bg == th.shadowOf(th.bg, false) || cl.bg == th.shadowOf(th.panel, false) {
				continue
			}
			if cr := contrast(cl.fg, cl.bg); cr < 2.2 {
				t.Fatalf("%s: %q at %d,%d reads at %.2f (fg %v on %v)", step, cl.s, x, y, cr, cl.fg, cl.bg)
			}
		}
	}
}

// A skin switch cross-fades the colors and swaps the frames halfway.
func TestSkinSwitchFades(t *testing.T) {
	m, _, clk := motionModel(t, 120, 40, "")
	m.View()
	m.setSkin("retro")
	if v := ansi.Strip(m.View()); !strings.Contains(v, "╭") || strings.Contains(v, "╔") {
		t.Fatal("the chrome swapped before halfway")
	}
	clk.add(dTheme * 3 / 4)
	if v := ansi.Strip(m.View()); !strings.Contains(v, "╔") || strings.Contains(v, "╭") {
		t.Fatal("the chrome didn't swap after halfway")
	}
	clk.add(dTheme)
	checkView(t, m, "after the fade")
}

// Retro chrome: double-line boxes and the menu bar; Modern has neither,
// and only Retro moves the layout down a row.
func TestRetroChrome(t *testing.T) {
	mod := newTestModel(t, 120, 40)
	ret, _ := retroModel(t, 120, 40)
	mv, rv := checkView(t, mod, "modern"), checkView(t, ret, "retro")
	if strings.Contains(mv, "╔") || strings.Contains(mv, "Huddle") || strings.Contains(mv, "░") {
		t.Fatal("retro chrome in the modern skin")
	}
	first := strings.Split(rv, "\n")[0]
	for _, want := range []string{"≡", "Chat", "View", "Huddle", "Theme", "? Help", "12:", "online"} {
		if !strings.Contains(first, want) && !(want == "12:" && strings.Contains(first, ":")) {
			t.Errorf("menu bar lacks %q: %q", want, first)
		}
	}
	if !strings.Contains(rv, "╔") || !strings.Contains(rv, "╝") {
		t.Fatal("no double-line input box")
	}
	if g, h := ret.geo(), mod.geo(); g.top != h.top+1 || g.T0 != 1 || g.statusY != h.statusY {
		t.Fatalf("retro top %d (T0 %d), modern %d", g.top, g.T0, h.top)
	}
	// The desktop pattern sits in the gutters only, never on a target.
	c := ret.lastCanvas
	g := ret.geo()
	n := 0
	for y := 0; y < c.h; y++ {
		for x := 0; x < c.w; x++ {
			cl := c.at(x, y)
			if cl.s != "░" {
				continue
			}
			n++
			if cl.act != 0 {
				t.Fatalf("desktop under a target at %d,%d", x, y)
			}
			if x >= g.CX-1 && x < g.CX+g.CW+2 || x < g.L || y == 0 || y >= g.statusY {
				t.Fatalf("desktop outside the gutters at %d,%d", x, y)
			}
		}
	}
	if n == 0 {
		t.Fatal("no desktop pattern")
	}
	// Tiny terminals drop the menu bar.
	ret.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	if ret.geo().bar {
		t.Fatal("menu bar on a 10-row terminal")
	}
	checkView(t, ret, "tiny retro")
}

// The menu bar's entries open what they name and light up under the
// pointer.
func TestMenuBar(t *testing.T) {
	m, _ := retroModel(t, 120, 40)
	click(t, m, "menu:chat")
	if m.mode != modeSwitcher || palTabs[m.pal.tab].id != "channels" {
		t.Fatalf("Chat: mode %v tab %d", m.mode, m.pal.tab)
	}
	press(m, "esc")
	click(t, m, "menu:view")
	if m.mode != modeSwitcher || palTabs[m.pal.tab].id != "settings" {
		t.Fatal("View")
	}
	press(m, "esc")
	click(t, m, "menu:theme")
	if m.mode != modeSwitcher || m.pal.shown[m.pal.sel].group != "Theme" {
		t.Fatal("Theme")
	}
	press(m, "esc")
	click(t, m, "menu:help")
	if m.mode != modeHelp {
		t.Fatal("Help")
	}
	press(m, "esc")
	click(t, m, "menu:all")
	if m.mode != modeSwitcher || palTabs[m.pal.tab].id != "all" {
		t.Fatal("≡")
	}
	press(m, "esc")
	// hover
	x, y := find(t, m, "menu:view")
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
	m.View()
	if m.lastCanvas.at(x, y).bg != m.st.th.accent {
		t.Fatal("no hover highlight on View")
	}
	m.Update(tea.MouseMsg{X: x, Y: y + 5, Action: tea.MouseActionMotion})
	m.View()
	if m.lastCanvas.at(x, y).bg == m.st.th.accent {
		t.Fatal("hover stayed")
	}
	// Huddle joins, then leaves.
	hm, h := newHuddleModel(t, 120, 40)
	hm.setSkin("retro")
	click(t, hm, "menu:huddle")
	h.update(hm)
	if h.in != 10 {
		t.Fatalf("Huddle didn't join: in %d (%q)", h.in, hm.notice)
	}
	click(t, hm, "menu:huddle")
	h.update(hm)
	if h.in != 0 {
		t.Fatal("Huddle didn't leave")
	}
}

// The clock ticks once a minute while the menu bar shows it, never in
// the Modern skin.
func TestMenuClockTick(t *testing.T) {
	m := newTestModel(t, 120, 40)
	if m.clockTick() != nil {
		t.Fatal("clock tick in the modern skin")
	}
	r, _ := retroModel(t, 120, 40)
	r.clockRun = false
	if r.clockTick() == nil || r.clockTick() != nil {
		t.Fatal("want exactly one pending clock tick")
	}
}

// A shadow darkens two columns right and one row below, and never draws
// outside the screen, whatever the box.
func TestShadowStaysOnScreen(t *testing.T) {
	m, _ := retroModel(t, 20, 8)
	th := m.st.th
	for _, r := range []rect{{14, 3, 20, 8}, {-3, -2, 25, 12}, {0, 0, 4, 2}, {18, 6, 19, 7}} {
		c := newCanvas(20, 8, th)
		m.shade(c, r)
		if len(c.cells) != 20*8 {
			t.Fatal("canvas grew")
		}
		c = newCanvas(20, 8, th)
		c.setClip(rect{0, 0, 10, 8}) // a pane the caller keeps clear
		m.shade(c, r)
		for y := 0; y < 8; y++ {
			for x := 10; x < 20; x++ {
				if c.at(x, y).bg != th.bg {
					t.Fatalf("shadow outside the clip at %d,%d", x, y)
				}
			}
		}
	}
	c := newCanvas(20, 8, th)
	m.shade(c, rect{2, 2, 8, 5})
	for _, p := range [][2]int{{8, 3}, {9, 4}, {4, 5}, {7, 5}} {
		if c.at(p[0], p[1]).bg == th.bg {
			t.Errorf("no shadow at %v", p)
		}
	}
	for _, p := range [][2]int{{8, 2}, {10, 3}, {3, 5}, {5, 4}} {
		if c.at(p[0], p[1]).bg != th.bg {
			t.Errorf("shadow at %v", p)
		}
	}
	mod := newTestModel(t, 20, 8)
	c = newCanvas(20, 8, mod.st.th)
	mod.shade(c, rect{2, 2, 8, 5})
	if c.at(8, 3).bg != mod.st.th.bg {
		t.Fatal("shadow in the modern skin")
	}
}

// Dialogs, popups and the picker get a frame and a shadow in Retro.
func TestRetroFloatingBoxes(t *testing.T) {
	m, _ := retroModel(t, 120, 40)
	th := m.st.th
	shadowed := func(step string) {
		t.Helper()
		c := m.lastCanvas
		n := 0
		for i := range c.cells {
			if c.cells[i].bg == th.shadowOf(th.bg, false) {
				n++
			}
		}
		if n == 0 {
			t.Errorf("%s: no shadow", step)
		}
	}
	for _, step := range []string{"palette", "settings", "keys"} {
		switch step {
		case "palette":
			press(m, "ctrl+k")
		case "settings":
			runInput(t, m, "/settings")
		case "keys":
			press(m, "f1")
		}
		v := checkView(t, m, step)
		if !strings.Contains(v, "╔") || !strings.Contains(v, "╝") {
			t.Errorf("%s: no double-line frame", step)
		}
		press(m, "esc")
		if step == "keys" && m.mode != modeNormal {
			press(m, "esc")
		}
	}
	typeVia(m, "/")
	v := checkView(t, m, "popup")
	if strings.Count(v, "╔") < 2 {
		t.Error("popup not double-lined")
	}
	shadowed("popup")
	press(m, "esc", "esc")
	m.openPicker(false)
	checkView(t, m, "picker")
	shadowed("picker")
}

// The qbasic palette draws its dialogs gray with dark text, the palette's
// rows included.
func TestDialogTheme(t *testing.T) {
	f := newFake()
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: retroPrefs(t, `,"theme":"qbasic"`)})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	press(m, "ctrl+k")
	m.View()
	d := m.st.th.dialog
	if d == nil || m.st.th.name != "qbasic" {
		t.Fatal("qbasic has no dialog colors")
	}
	c := m.lastCanvas
	for y := 0; y < c.h; y++ {
		for x := 0; x < c.w; x++ {
			cl := c.at(x, y)
			if cl.bg == d.panel && !strings.Contains(" ▐▌", cl.s) && contrast(cl.fg, cl.bg) < 3 { // pill ends are shapes
				t.Fatalf("%q at %d,%d is unreadable on the gray dialog (%.2f)", cl.s, x, y, contrast(cl.fg, cl.bg))
			}
		}
	}
	if m.st.th != themes["qbasic"] || m.lastCanvas.th != m.st.th {
		t.Fatal("the dialog styles outlived the dialog")
	}
}

// introModel is a retro model with motion on and a fake clock; ready says
// whether the cache already has channels.
func introModel(t *testing.T, ready bool, extra string) (*Model, *fakeClient, *clock) {
	t.Helper()
	f := newFake()
	if !ready {
		f.chans = nil
		f.status.State = client.Syncing
	}
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), motion: true, UIPrefs: retroPrefs(t, extra)})
	clk := newClock(m)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return m, f, clk
}

// tick advances the clock and delivers a frame, as the tick would.
func tick(m *Model, clk *clock, d time.Duration) {
	clk.add(d)
	m.fx.running = false
	m.Update(frameMsg{gen: m.fx.gen})
}

func desktopOnly(m *Model) bool {
	m.View()
	for _, cl := range m.lastCanvas.cells {
		if cl.s != "░" {
			return false
		}
	}
	return true
}

func TestIntro(t *testing.T) {
	// A cached start: the desktop holds the minimum, then the zoom opens.
	m, _, clk := introModel(t, true, "")
	if !m.introShown() || !desktopOnly(m) {
		t.Fatal("no desktop at start")
	}
	if w := m.introWake(); w != dIntroHold {
		t.Fatalf("wake in %v, want %v", w, dIntroHold)
	}
	tick(m, clk, dIntroHold-time.Millisecond)
	if !desktopOnly(m) {
		t.Fatal("the zoom started before the minimum")
	}
	tick(m, clk, time.Millisecond)
	if !m.introZooming() || !m.fastWanted() {
		t.Fatal("no zoom after the minimum")
	}
	tick(m, clk, fastFrame)
	if desktopOnly(m) {
		t.Fatal("the rectangle didn't open")
	}
	for i := 0; i < 10; i++ {
		v := checkView(t, m, "zoom")
		if i < 3 && !strings.Contains(v, "═") {
			t.Fatal("no growing frame")
		}
		tick(m, clk, dIntroZoom/10)
	}
	tick(m, clk, time.Millisecond)
	if m.introShown() || m.fastWanted() {
		t.Fatal("the intro didn't end")
	}
	if v := checkView(t, m, "after the intro"); strings.Count(v, "░") > 2000 || !strings.Contains(v, "general") {
		t.Fatal("the UI didn't come back")
	}

	// A fresh start waits for the connection, not past it.
	m, f, clk := introModel(t, false, "")
	tick(m, clk, dIntroHold*2)
	if m.introZooming() {
		t.Fatal("zoomed before the connection")
	}
	f.status.State = client.Online
	m.Update(updateMsg(client.Update{Status: true}))
	if !m.introZooming() {
		t.Fatal("no zoom when the connection came up")
	}

	// ...and never longer than dIntroMax.
	m, _, clk = introModel(t, false, "")
	tick(m, clk, dIntroMax)
	if !m.introZooming() {
		t.Fatal("still holding after dIntroMax")
	}

	// A key ends it and still types.
	m, _, _ = introModel(t, true, "")
	press(m, "h")
	if m.introShown() || m.input.Value() != "h" {
		t.Fatalf("key during the intro: shown %v, input %q", m.introShown(), m.input.Value())
	}

	// Instant, Animations off and the Modern skin skip it.
	m, _, _ = introModel(t, true, `,"startup":"instant"`)
	if m.introShown() {
		t.Fatal("Instant played the intro")
	}
	m, _, _ = introModel(t, true, `,"no_motion":true`)
	if m.introShown() {
		t.Fatal("Animations off played the intro")
	}
	mm, _, _ := motionModel(t, 120, 40, "")
	if mm.introShown() {
		t.Fatal("the modern skin played the intro")
	}
}

// Settings › Appearance › Startup saves Instant.
func TestStartupSetting(t *testing.T) {
	path := retroPrefs(t, "")
	m := New(context.Background(), newFake(), Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: path})
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 44})
	m.openSettings("appearance")
	ctls := m.setCtls()
	row := -1
	for i, c := range ctls {
		if c.label == "Startup" {
			row = i
		}
	}
	if row < 0 {
		t.Fatal("no Startup row")
	}
	m.set.row = row // in view
	click(t, m, "set:"+strconv.Itoa(row)+":1")
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"startup": "instant"`) {
		t.Fatalf("ui.json: %s", b)
	}
}

func threadModel(t *testing.T, w, h int) (*Model, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ui.json")
	m := New(context.Background(), newFake(), Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: path})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	run(m, m.openThread(103))
	m.layoutInput()
	return m, path
}

func altKey(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: true} }

func TestThreadResize(t *testing.T) {
	m, path := threadModel(t, 160, 48)
	if g := m.geo(); !g.split || g.TW != threadW {
		t.Fatalf("split %v width %d", g.split, g.TW)
	}
	// keys
	m.Update(altKey('['))
	if g := m.geo(); g.TW != threadW+threadStep || g.RX != 160-g.TW {
		t.Fatalf("⌥[: %d", g.TW)
	}
	m.Update(altKey(']'))
	m.Update(altKey(']'))
	if g := m.geo(); g.TW != threadW-threadStep {
		t.Fatalf("⌥]: %d", g.TW)
	}
	// macOS Option typing characters
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'“'}})
	if g := m.geo(); g.TW != threadW {
		t.Fatalf("Option-[: %d", g.TW)
	}
	// limits
	for i := 0; i < 30; i++ {
		m.Update(altKey(']'))
	}
	if g := m.geo(); g.TW != minThreadW {
		t.Fatalf("min: %d", g.TW)
	}
	for i := 0; i < 40; i++ {
		m.Update(altKey('['))
	}
	g := m.geo()
	if g.RX-g.L < besideThread || g.CW < besideThread-8 {
		t.Fatalf("the column got %d beside a %d pane", g.RX-g.L, g.TW)
	}
	checkView(t, m, "widest")
	// mouse: hover lights the grip, a drag sizes it, the release saves
	x, y := find(t, m, "thrresize")
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
	m.View()
	lit := false
	for yy := 0; yy < m.h; yy++ {
		lit = lit || m.lastCanvas.at(x, yy).fg == m.st.th.accent
	}
	if !lit {
		t.Fatal("grip not lit on hover")
	}
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: 160 - 50, Y: y, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
	if g := m.geo(); g.TW != 50 {
		t.Fatalf("drag: %d", g.TW)
	}
	checkView(t, m, "dragging")
	m.Update(tea.MouseMsg{X: 160 - 50, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"thread_w": 50`) {
		t.Fatalf("not saved: %s", b)
	}
	// a restart keeps it; a narrow terminal still goes full width
	again := New(context.Background(), newFake(), Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: path})
	again.Update(tea.WindowSizeMsg{Width: 160, Height: 48})
	run(again, again.openThread(103))
	if g := again.geo(); g.TW != 50 {
		t.Fatalf("restart: %d", g.TW)
	}
	again.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	if g := again.geo(); g.split || !g.thrFull {
		t.Fatal("narrow terminal: not full width")
	}
	again.Update(altKey('['))
	b2, _ := os.ReadFile(path)
	if string(b2) != string(b) {
		t.Fatal("a full-width thread resized")
	}
	checkView(t, again, "narrow")
	// a closed thread doesn't take the keys
	m.closeThread()
	m.Update(altKey('['))
	if m.ui.ThreadW != 50 {
		t.Fatal("resized with no thread open")
	}
}

// blankRuns are the lengths of every run of blank rows between laid-out
// messages, and the rows inside each group.
func blankRuns(rows []prow) []int {
	var out []int
	n := 0
	for i, r := range rows {
		if r.b == nil {
			n++
			continue
		}
		if n > 0 && i > n {
			out = append(out, n)
		}
		n = 0
	}
	return out
}

// Cozy: exactly one blank row between groups and around dividers, none
// inside a group, code and diffs included. Compact: none.
func TestSpacingRhythm(t *testing.T) {
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.Local).UnixMilli()
	msg := func(id, author uint64, min int, body string) client.Message {
		return client.Message{Message: proto.Message{ID: id, Chan: 10, Author: author, Time: base + int64(min)*60000, Body: body}}
	}
	conv := []client.Message{
		msg(1, 2, 0, "first"),
		msg(2, 2, 1, "pasting:\n```go\nx := 1\n```"),
		msg(3, 2, 2, "and the diff\n```diff\n-a\n+b\n```"),
		msg(4, 2, 3, "plain follow-up"),
		msg(5, 3, 4, "another author"),
		msg(6, 3, 12, "same author, 8 min later"),
		msg(7, 1, 13, "mine"),
		msg(8, 1, 14, "mine again"),
		msg(9, 2, 24*60+5, "next day"),
		msg(10, 2, 24*60+6, "```\ncode only\n```"),
		msg(11, 3, 24*60+7, "after code, new author"),
	}
	for _, split := range []bool{false, true} {
		for _, compact := range []bool{false, true} {
			m, f := newTestModelFake(t, 120, 40)
			m.ui.Compact, m.ui.Split = compact, split
			f.msgs[10] = conv
			m.reloadPane(&m.main)
			rows, owner := m.paneLines(&m.main, m.dims().msgW)
			runs := blankRuns(rows)
			if compact {
				if len(runs) != 0 {
					t.Errorf("split=%v compact: blank rows %v", split, runs)
				}
				continue
			}
			for _, n := range runs {
				if n != 1 {
					t.Errorf("split=%v cozy: a gap of %d rows", split, n)
				}
			}
			// groups start after exactly one blank; members follow flush
			starts := map[int]bool{1: true, 5: true, 6: true, 7: true, 9: true, 11: true}
			seen := map[int]bool{}
			for i, o := range owner {
				if o < 0 || seen[o] {
					continue
				}
				seen[o] = true
				id := int(conv[o].ID)
				prevBlank := i > 0 && rows[i-1].b == nil
				switch {
				case id == 1 || id == 9:
					// after a day divider, itself after a blank
					if !prevBlank || owner[i-2] != -1 || rows[i-2].b == nil {
						t.Errorf("split=%v: message %d not one blank under its divider", split, id)
					}
				case starts[id] && !prevBlank:
					t.Errorf("split=%v: group at %d has no blank above", split, id)
				case !starts[id] && prevBlank:
					t.Errorf("split=%v: a gap inside a group before %d", split, id)
				}
			}
		}
	}
}

// The typing line sits right on the input box, and its row stays
// reserved when nobody types, so the log never jumps.
func TestTypingRowReserved(t *testing.T) {
	for _, retro := range []bool{false, true} {
		m, f := newTestModelFake(t, 120, 40)
		if retro {
			m.setSkin("retro")
		}
		quiet := m.geo()
		if quiet.noteY != quiet.inY-1 || quiet.msgBot != quiet.noteY {
			t.Fatalf("retro=%v: note row %d, input %d, log ends %d", retro, quiet.noteY, quiet.inY, quiet.msgBot)
		}
		f.typing[10] = []uint64{2}
		m.reloadTyping()
		v := strings.Split(checkView(t, m, "typing"), "\n")
		if g := m.geo(); g != quiet {
			t.Fatalf("retro=%v: the layout moved when someone typed", retro)
		}
		if !strings.Contains(v[quiet.noteY], "bob is typing") {
			t.Fatalf("retro=%v: typing line at %d: %q", retro, quiet.noteY, v[quiet.noteY])
		}
	}
}

// Every frame fits in the Retro skin too, mid-animation and mid-intro.
func TestRetroFramesFit(t *testing.T) {
	for _, split := range []bool{false, true} {
		for _, sz := range [][2]int{{80, 24}, {120, 40}, {160, 48}, {40, 24}, {40, 12}, {20, 6}, {200, 60}} {
			f := newFake()
			m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), motion: true, UIPrefs: retroPrefs(t, "")})
			clk := newClock(m)
			m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
			m.ui.Split = split
			for i := 0; i < 12; i++ {
				checkView(t, m, "intro")
				tick(m, clk, 60*time.Millisecond)
			}
			retroSteps(t, m, f, clk)
		}
	}
}

func retroSteps(t *testing.T, m *Model, f *fakeClient, clk *clock) {
	t.Helper()
	step := func(name string) {
		t.Helper()
		for i := 0; i < 6; i++ {
			checkView(t, m, name)
			clk.add(40 * time.Millisecond)
		}
	}
	typeVia(m, "hello there")
	press(m, "enter")
	step("send pop")
	typeVia(m, "/")
	step("popup")
	press(m, "esc", "esc")
	f.msgs[10] = append(f.msgs[10], client.Message{Message: proto.Message{ID: 900, Chan: 10, Author: 2, Body: "arrived", Time: clk.t.UnixMilli()}})
	m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{10: true}}))
	step("arrival")
	press(m, "ctrl+b")
	step("sidebar resize")
	press(m, "ctrl+k")
	step("palette")
	press(m, "esc")
	m.setTheme("paper")
	step("theme fade")
	m.setSkin("modern")
	step("skin fade")
	m.setSkin("retro")
	step("skin fade back")
	m.setInfo("copied")
	step("toast")
	run(m, m.openThread(103))
	m.layoutInput()
	step("thread pane")
	m.Update(altKey('['))
	step("thread wider")
	m.openPicker(false)
	step("picker")
	press(m, "esc")
	runInput(t, m, "/settings")
	step("settings")
	press(m, "esc")
	runInput(t, m, "/help")
	step("keys")
}

// A frame needed sooner replaces a long pending wake: the intro's 2.5 s
// hold never delays the zoom once the connection is up.
func TestSoonerFrameReplacesWake(t *testing.T) {
	m, f, clk := introModel(t, false, "")
	if !m.fx.running || m.fx.wakeAt.Sub(clk.t) != dIntroMax {
		t.Fatalf("hold wake at %v", m.fx.wakeAt.Sub(clk.t))
	}
	clk.add(time.Second)
	f.status.State = client.Online
	m.Update(updateMsg(client.Update{Status: true}))
	if !m.introZooming() || m.fx.wakeAt.Sub(clk.t) > fastFrame {
		t.Fatalf("zoom frame waits %v", m.fx.wakeAt.Sub(clk.t))
	}
	gen := m.fx.gen
	m.Update(frameMsg{gen: gen - 1}) // the replaced wake fires: ignored
	if !m.fx.running {
		t.Fatal("a stale frame cancelled the pending one")
	}
}

// Content never lands on a framed dialog's border, however small.
func TestDialogFrameKeptClear(t *testing.T) {
	for _, sz := range [][2]int{{40, 12}, {40, 24}, {30, 16}} {
		m, f := retroModel(t, sz[0], sz[1])
		f.typing[10] = nil
		m.reloadTyping()
		for _, open := range []func(){func() { press(m, "ctrl+k") }, func() { runInput(t, m, "/settings"); press(m, "down") }, func() { runInput(t, m, "/search x") }} {
			open()
			if !m.overlayShown() {
				t.Fatalf("%v: no dialog", sz)
			}
			m.View()
			c, r := m.lastCanvas, m.dialogBox(76, 30)
			if m.mode == modeSettings {
				r = m.dialogBox(116, 38)
			} else if m.mode != modeSwitcher {
				r = m.dialogBox(110, m.h-2)
			}
			for x := r.x0; x < r.x1; x++ {
				for _, y := range []int{r.y0, r.y1 - 1} {
					if s := c.at(x, y).s; !strings.Contains("╔╗╚╝═", s) {
						t.Fatalf("%v mode %v: %q on the frame at %d,%d", sz, m.mode, s, x, y)
					}
				}
			}
			for y := r.y0; y < r.y1; y++ {
				for _, x := range []int{r.x0, r.x1 - 1} {
					if s := c.at(x, y).s; !strings.Contains("╔╗╚╝║", s) {
						t.Fatalf("%v mode %v: %q on the frame at %d,%d", sz, m.mode, s, x, y)
					}
				}
			}
			press(m, "esc")
		}
	}
}

// A release lost to another pane ends the drag at the next plain motion,
// and the width is kept; losing focus ends it too.
func TestDragEndsWithoutRelease(t *testing.T) {
	m, path := threadModel(t, 160, 48)
	x, y := find(t, m, "thrresize")
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: 160 - 60, Y: y, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: 20, Y: y, Action: tea.MouseActionMotion}) // no button: it was released elsewhere
	if m.drag != dragNone || m.geo().TW != 60 {
		t.Fatalf("drag %v width %d", m.drag, m.geo().TW)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"thread_w": 60`) {
		t.Fatalf("not saved: %s", b)
	}
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m.Update(tea.BlurMsg{})
	if m.drag != dragNone {
		t.Fatal("blur left the drag on")
	}
	sb := m.ui.sidebarMode()
	ex, ey := find(t, m, "sbresize")
	m.Update(tea.MouseMsg{X: ex, Y: ey, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: ex, Y: ey + 1, Action: tea.MouseActionMotion})
	if m.drag != dragNone || m.ui.sidebarMode() != sb {
		t.Fatal("a lost release on the sidebar edge acted as a click")
	}
}

// The resize keys never eat text: curly quotes type once the input has
// text, and a full-width thread lets everything through.
func TestThreadKeysKeepText(t *testing.T) {
	m, _ := threadModel(t, 160, 48)
	typeVia(m, "say ")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'“'}})
	if m.input.Value() != "say “" || m.geo().TW != threadW {
		t.Fatalf("input %q width %d", m.input.Value(), m.geo().TW)
	}
	n, _ := threadModel(t, 90, 30)
	n.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'“'}})
	if n.input.Value() != "“" {
		t.Fatalf("full-width thread ate the quote: %q", n.input.Value())
	}
	// at a limit the key says so instead of saving nothing new
	for i := 0; i < 10; i++ {
		m.Update(altKey(']'))
	}
	if !strings.Contains(m.notice, "narrowest") {
		t.Fatalf("notice %q", m.notice)
	}
}

// The menu bar keeps ≡ and Help when narrow, its letters work with ⌥, and
// a hidden clock never wakes the screen.
func TestMenuBarNarrowAndKeys(t *testing.T) {
	m, _ := retroModel(t, 44, 20)
	first := strings.Split(checkView(t, m, "narrow"), "\n")[0]
	if !strings.Contains(first, "≡") || !strings.Contains(first, "Help") || strings.Contains(first, "Theme") {
		t.Fatalf("narrow menu bar: %q", first)
	}
	m.clockRun = false
	if m.clockTick() != nil {
		t.Fatal("clock tick while the clock is hidden")
	}
	w, _ := retroModel(t, 120, 40)
	w.Update(altKey('c'))
	if w.mode != modeSwitcher || palTabs[w.pal.tab].id != "channels" {
		t.Fatal("⌥C")
	}
	press(w, "esc")
	w.Update(altKey('t'))
	if w.mode != modeSwitcher || w.pal.shown[w.pal.sel].group != "Theme" {
		t.Fatal("⌥T")
	}
	mod := newTestModel(t, 120, 40)
	mod.Update(altKey('c'))
	if mod.mode != modeNormal {
		t.Fatal("menu keys without a menu bar")
	}
	r, _ := retroModel(t, 120, 14)
	if r.geo().bar {
		t.Fatal("menu bar on a 14-row terminal")
	}
}
