package tui

import (
	"context"
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

// motionModel is a test model with animations on and a fake clock.
func motionModel(t *testing.T, w, h int, prefs string) (*Model, *fakeClient, *clock) {
	t.Helper()
	f := newFake()
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), motion: true, UIPrefs: prefs})
	clk := newClock(m)
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m, f, clk
}

// find returns the first cell whose action starts with prefix.
func find(t *testing.T, m *Model, prefix string) (int, int) {
	t.Helper()
	m.View()
	c := m.lastCanvas
	for y := 0; y < c.h; y++ {
		for x := 0; x < c.w; x++ {
			if strings.HasPrefix(c.actionAt(x, y), prefix) {
				return x, y
			}
		}
	}
	t.Fatalf("no %q on screen:\n%s", prefix, ansi.Strip(m.View()))
	return 0, 0
}

func click(t *testing.T, m *Model, prefix string) {
	t.Helper()
	x, y := find(t, m, prefix)
	_, cmd := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	run(m, cmd)
}

// The frame fits at every size the spec names, mid-animation included.
func TestFramesFitWithMotion(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {120, 40}, {160, 48}, {40, 24}, {40, 12}, {20, 6}, {200, 60}} {
		m, f, clk := motionModel(t, sz[0], sz[1], "")
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
		m.setInfo("copied")
		step("toast")
		press(m, "up", "r")
		step("thread pane")
		runInput(t, m, "/settings")
		step("settings")
		press(m, "esc")
		runInput(t, m, "/help")
		step("keys")
	}
}

// No frame tick runs on an idle screen; one runs while something moves.
func TestFrameTickOnlyWhileAnimating(t *testing.T) {
	m, f, clk := motionModel(t, 120, 40, "")
	m.fx.running = false
	if m.frameTick() != nil {
		t.Fatal("tick on an idle screen")
	}
	f.msgs[10] = append(f.msgs[10], client.Message{Message: proto.Message{ID: 900, Chan: 10, Author: 2, Body: "new", Time: clk.t.UnixMilli()}})
	m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{10: true}}))
	if !m.fastWanted() {
		t.Fatal("arrival didn't start an animation")
	}
	m.fx.running = false
	if m.frameTick() == nil {
		t.Fatal("no tick while animating")
	}
	clk.add(time.Second)
	m.fx.running = false
	if m.fastWanted() || m.frameTick() != nil {
		t.Fatal("tick kept running after the animation ended")
	}
	// Typing dots loop slowly; the tick stops when nobody types.
	f.typing[10] = []uint64{2}
	m.Update(updateMsg(client.Update{Typing: map[uint64]bool{10: true}}))
	m.fx.running = false
	if !m.slowWanted() || m.frameTick() == nil {
		t.Fatal("no slow tick for typing dots")
	}
	f.typing[10] = nil
	m.reloadTyping()
	m.fx.running = false
	if m.frameTick() != nil {
		t.Fatal("tick after typing stopped")
	}
	// Animations off: nothing ever ticks.
	m.setMotion(false)
	f.typing[10] = []uint64{2}
	m.reloadTyping()
	m.fx.running = false
	if m.frameTick() != nil {
		t.Fatal("tick with animations off")
	}
}

func TestPaletteTabsAndRows(t *testing.T) {
	m := newTestModel(t, 120, 40)
	press(m, "ctrl+k")
	if m.mode != modeSwitcher || palTabs[m.pal.tab].id != "all" {
		t.Fatalf("mode %v tab %d", m.mode, m.pal.tab)
	}
	v := checkView(t, m, "palette")
	for _, want := range []string{"Command palette", "All", "Channels", "People", "Commands", "Settings", "# ", "esc"} {
		if !strings.Contains(v, want) {
			t.Errorf("palette lacks %q:\n%s", want, v)
		}
	}
	tabs := map[string][]string{
		"channels": {"backend", "general", "secret"},
		"people":   {"bob", "carol"},
		"commands": {"Keyboard shortcuts", "/join"},
		"settings": {"Appearance", "graphite", "macintosh", "✓ current"},
	}
	for i := 1; i < len(palTabs); i++ {
		press(m, "tab")
		id := palTabs[m.pal.tab].id
		v := checkView(t, m, id)
		for _, want := range tabs[id] {
			found := strings.Contains(v, want)
			for _, it := range m.pal.shown {
				found = found || strings.Contains(it.label+" "+it.meta, want)
			}
			if !found {
				t.Errorf("%s tab lacks %q", id, want)
			}
		}
	}
	press(m, "shift+tab", "shift+tab", "shift+tab", "shift+tab")
	if palTabs[m.pal.tab].id != "all" {
		t.Fatalf("shift+tab: %d", m.pal.tab)
	}
	// Fuzzy search keeps the switcher's behavior: Enter opens the top match.
	typeVia(m, "sec")
	press(m, "enter")
	if m.mode != modeNormal || m.active != 13 {
		t.Fatalf("mode %v active %d", m.mode, m.active)
	}
	// A command row that needs arguments is typed into the input.
	press(m, "ctrl+k")
	typeVia(m, "/remind")
	press(m, "enter")
	if m.mode != modeNormal || m.input.Value() != "/remind " {
		t.Fatalf("mode %v input %q", m.mode, m.input.Value())
	}
}

func TestPaletteSwitchesThemeAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui.json")
	f := newFake()
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: path})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.st.th.name != "graphite" {
		t.Fatalf("default theme %q", m.st.th.name)
	}
	press(m, "ctrl+k")
	typeVia(m, "phosphor")
	press(m, "enter")
	if m.st.th.name != "phosphor" || m.mode != modeSwitcher {
		t.Fatalf("theme %q mode %v (a theme row previews and stays open)", m.st.th.name, m.mode)
	}
	press(m, "esc")
	checkView(t, m, "phosphor")
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), `"theme": "phosphor"`) {
		t.Fatalf("ui.json: %s %v", b, err)
	}
	m2 := New(context.Background(), newFake(), Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: path})
	if m2.st.th.name != "phosphor" {
		t.Fatalf("theme not loaded: %q", m2.st.th.name)
	}
	runInput(t, m2, "/theme nope")
	if !m2.noticeErr || !strings.Contains(m2.notice, "graphite") {
		t.Fatalf("bad /theme: %q", m2.notice)
	}
	runInput(t, m2, "/theme graphite")
	if m2.st.th.name != "graphite" {
		t.Fatal("/theme graphite")
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "theme") {
		t.Fatalf("the default theme is stored: %s", b)
	}
	// A broken file falls back to defaults instead of failing.
	os.WriteFile(path, []byte("{nope"), 0o600)
	if p := loadUIPrefs(path); p.themeName() != "graphite" || !p.loadFailed {
		t.Fatalf("broken file: %+v", p)
	}
}

func TestSettingsDialog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui.json")
	f := newFake()
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: path})
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 44})
	runInput(t, m, "/settings")
	if m.mode != modeSettings || setPages[m.set.page].id != "appearance" {
		t.Fatalf("mode %v page %d", m.mode, m.set.page)
	}
	v := checkView(t, m, "appearance")
	for _, want := range []string{"Settings", "Appearance", "Notifications", "Theme", "Density", "Cozy", "Compact", "Sidebar", "Show people as", "Stripe", "Animations", "✓ graphite", "macintosh"} {
		if !strings.Contains(v, want) {
			t.Errorf("appearance lacks %q", want)
		}
	}
	press(m, "right") // theme → sky
	if m.st.th.name != "sky" {
		t.Fatalf("right on themes: %q", m.st.th.name)
	}
	press(m, "down", "right") // density → compact
	if !m.ui.Compact {
		t.Fatal("density")
	}
	press(m, "down", "right") // sidebar normal → wide
	if m.ui.sidebarMode() != sbWide {
		t.Fatalf("sidebar %q", m.ui.sidebarMode())
	}
	press(m, "down", "right") // "show people as": later styles are greyed and skipped
	press(m, "down", "enter") // animations: tests start with them off
	if m.fx.off || m.ui.NoMotion {
		t.Fatal("animations toggle")
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{`"theme": "sky"`, `"compact": true`, `"sidebar": "wide"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("ui.json lacks %s: %s", want, b)
		}
	}
	press(m, "tab")
	v = checkView(t, m, "notifications")
	for _, want := range []string{"Notify me about", "Mentions & DMs", "Popups show", "Sounds", "Quiet hours", "Do not disturb"} {
		if !strings.Contains(v, want) {
			t.Errorf("notifications lacks %q", want)
		}
	}
	press(m, "tab")
	press(m, "enter") // Keys opens the sheet
	if m.mode != modeHelp {
		t.Fatalf("keys page: mode %v", m.mode)
	}
	press(m, "esc")
	m.openSettings("about")
	if v := checkView(t, m, "about"); !strings.Contains(v, "terminal chat") {
		t.Errorf("about page:\n%s", v)
	}
	// Clicking a control works like the keys.
	m.openSettings("appearance")
	click(t, m, "set:1:1")
	if !m.ui.Compact {
		t.Fatal("click compact")
	}
	click(t, m, "set:1:0")
	if m.ui.Compact {
		t.Fatal("click cozy")
	}
	click(t, m, "set:3:1")
	if !strings.Contains(m.notice, "later") {
		t.Fatalf("greyed style: %q", m.notice)
	}
	for _, sz := range [][2]int{{80, 24}, {40, 24}, {20, 8}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkView(t, m, "settings small")
	}
	press(m, "esc")
	if m.mode != modeNormal {
		t.Fatal("esc")
	}
}

func TestSidebarModesAndTooltip(t *testing.T) {
	m := newTestModel(t, 140, 40)
	for _, c := range []struct {
		mode  string
		w     int
		label bool
	}{{sbNormal, sbWNormal, true}, {sbWide, sbWWide, true}, {sbIcons, sbWIcons, false}} {
		m.setSidebar(c.mode)
		if g := m.geo(); g.SB != c.w {
			t.Fatalf("%s: width %d", c.mode, g.SB)
		}
		v := checkView(t, m, c.mode)
		if got := strings.Contains(v, "# backend"); got != c.label {
			t.Errorf("%s: labels shown = %v", c.mode, got)
		}
	}
	// Hovering an icon shows the channel's name.
	x, y := find(t, m, "chan:11")
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
	if v := checkView(t, m, "tooltip"); !strings.Contains(v, "# backend  3") {
		t.Fatalf("no tooltip:\n%s", v)
	}
	// The same motion again reuses the frame.
	before := m.View()
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
	if !m.reuseFrame || m.View() != before {
		t.Fatal("unchanged hover redrew")
	}
	// Clicking the edge cycles the size; dragging picks one.
	ex, ey := find(t, m, "sbresize")
	m.Update(tea.MouseMsg{X: ex, Y: ey, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: ex, Y: ey, Action: tea.MouseActionRelease})
	if m.ui.sidebarMode() != sbNormal {
		t.Fatalf("edge click: %q", m.ui.sidebarMode())
	}
	ex, ey = find(t, m, "sbresize")
	m.Update(tea.MouseMsg{X: ex, Y: ey, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: 31, Y: ey, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: 31, Y: ey, Action: tea.MouseActionRelease})
	if m.ui.sidebarMode() != sbWide {
		t.Fatalf("edge drag: %q", m.ui.sidebarMode())
	}
	// Narrow terminals: icons below 90, hidden below 60.
	m.Update(tea.WindowSizeMsg{Width: 89, Height: 30})
	if m.sbMode() != sbIcons {
		t.Fatalf("89 cols: %q", m.sbMode())
	}
	m.Update(tea.WindowSizeMsg{Width: 59, Height: 30})
	if m.sidebarShown() {
		t.Fatal("59 cols: sidebar shown")
	}
}

func TestToasts(t *testing.T) {
	m := newTestModel(t, 120, 40)
	m.setInfo("pinned")
	g := m.geo()
	rows := strings.Split(checkView(t, m, "toast"), "\n")
	if !strings.Contains(rows[g.noteY], "pinned") {
		t.Fatalf("toast not above the input: %q", rows[g.noteY])
	}
	click(t, m, "toast:close")
	if m.notice != "" {
		t.Fatal("click didn't dismiss the toast")
	}
	// Errors keep their row, in the error style, and are no toast.
	m.setErr("send", context.DeadlineExceeded)
	if m.toastShown() || !strings.Contains(strings.Split(checkView(t, m, "err"), "\n")[g.noteY], "! send") {
		t.Fatal("error shown as a toast")
	}
	// A notice raised inside a dialog shows over it.
	m.clearNotice()
	runInput(t, m, "/help")
	m.setInfo("copied")
	if v := checkView(t, m, "dialog notice"); !strings.Contains(v, "copied") {
		t.Fatalf("notice hidden by the dialog:\n%s", v)
	}
}

func TestMouseClicks(t *testing.T) {
	m, f := newTestModelFake(t, 140, 44)
	click(t, m, "chan:13")
	if m.active != 13 {
		t.Fatalf("sidebar click: %d", m.active)
	}
	click(t, m, "chan:10")
	m.main.scroll = 0
	// A message: select, then its reaction pill, its replies, the toolbar.
	click(t, m, "msg:main:")
	if m.mode != modeSelect {
		t.Fatal("message click didn't select")
	}
	press(m, "esc")
	for m.main.scroll = 0; ; m.main.scroll += 5 {
		m.View()
		if _, ok := m.lastCanvas.actIx["react:main:3~i103:👍"]; ok || m.main.scroll > 200 {
			break
		}
	}
	click(t, m, "react:main:3~i103:👍")
	if !strings.Contains(strings.Join(f.calls, " "), "unreact 👍") {
		t.Fatalf("pill click: calls %v", f.calls)
	}
	click(t, m, "thread:main:3~")
	if !m.thread.open() || m.thread.root != 103 {
		t.Fatal("replies click didn't open the thread")
	}
	if v := checkView(t, m, "thread"); !strings.Contains(v, "Thread") || !strings.Contains(v, "reply two") {
		t.Fatalf("thread pane:\n%s", v)
	}
	click(t, m, "thread:close")
	if m.thread.open() {
		t.Fatal("✕ didn't close the thread")
	}
	click(t, m, "palette:settings")
	if m.mode != modeSwitcher || palTabs[m.pal.tab].id != "settings" {
		t.Fatal("theme in the status bar")
	}
	click(t, m, "close")
	if m.mode != modeNormal {
		t.Fatal("click outside the dialog")
	}
	click(t, m, "keys")
	if m.mode != modeHelp {
		t.Fatal("F1 keys in the status bar")
	}
}

// The loading screen shows only without a cache, and ends with the first
// channels; a start with a cache never shows it.
func TestLoadingScreen(t *testing.T) {
	m, f, all := freshModel(t)
	if !m.booting() || !strings.Contains(ansi.Strip(m.View()), "terminal chat") {
		t.Fatalf("no loading screen on a fresh start:\n%s", ansi.Strip(m.View()))
	}
	f.chans = all
	m.Update(updateMsg(client.Update{Channels: true}))
	if m.booting() || strings.Contains(ansi.Strip(m.View()), "terminal chat") {
		t.Fatal("loading screen stayed")
	}
	cached := newTestModel(t, 100, 30)
	if cached.booting() {
		t.Fatal("loading screen with a cache")
	}
}

// Under NO_COLOR the frame has no colors, only attributes.
func TestNoColorFrame(t *testing.T) {
	f := newFake()
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(false)})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	for _, k := range []string{"", "ctrl+k", "esc", "up", "esc", "f1"} {
		if k != "" {
			press(m, k)
		}
		v := checkView(t, m, "no color "+k)
		if out := m.View(); strings.Contains(out, "38;") || strings.Contains(out, "48;") {
			t.Fatalf("color under NO_COLOR after %q", k)
		}
		_ = v
	}
}

// Hostile graphemes can't widen a row or leave stale cells, and a click
// never acts on a message other than the one drawn under it.
func TestCanvasHostileText(t *testing.T) {
	th := themes[defaultTheme]
	c := newCanvas(20, 1, th)
	c.put(0, 0, "SECRETSTUFF!", fgc(th.fg), "")
	c.put(0, 0, strings.Repeat("؀", 5)+"b", fgc(th.fg), "")
	if got := ansi.Strip(c.encode(newEncoder(0, false))); strWidth(got) != 20 || strings.Contains(got, "ECRET") {
		t.Fatalf("stale cells or wrong width: %q (%d)", got, strWidth(got))
	}
	c = newCanvas(20, 1, th)
	c.put(0, 0, strings.Repeat("ൎ", 8)+"aEND", fgc(th.fg), "")
	if got := ansi.Strip(c.encode(newEncoder(0, false))); strWidth(got) != 20 {
		t.Fatalf("row is %d cells: %q", strWidth(got), got)
	}
	c = newCanvas(20, 1, th)
	c.put(0, 0, "a\xff\x9bb", fgc(th.fg), "")
	if out := c.encode(newEncoder(0, false)); strings.Contains(out, "\xff") || strings.Contains(out, "\x9b") {
		t.Fatalf("raw bytes reached the terminal: %q", out)
	}
	done := make(chan struct{})
	go func() {
		c.putANSI(0, 0, "abc\x1b", 20, fgc(th.fg), "")
		c.putANSI(0, 0, "x\x1b\xc2\x9b2Jy", 20, fgc(th.fg), "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("putANSI hangs on a trailing ESC")
	}
	if out := c.encode(newEncoder(0, false)); strings.Contains(out, "\x9b") {
		t.Fatalf("8-bit CSI leaked: %q", out)
	}
	if w := strWidth(strings.Repeat("؀؀؀W", 40)); w != 160 {
		t.Fatalf("strWidth disagrees with the canvas: %d", w)
	}
	// A click on a message that moved since the frame finds it by id.
	msgs := []client.Message{{Message: proto.Message{ID: 7}}, {Message: proto.Message{ID: 9}}}
	if i := findRef(msgs, "0~i9"); i != 1 {
		t.Fatalf("moved message: %d", i)
	}
	if i := findRef(msgs, "0~i8"); i != -1 {
		t.Fatalf("gone message: %d", i)
	}
}
