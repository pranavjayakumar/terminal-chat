package tui

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/iconfont"
)

// The table matches the font's source: same names at the same code points,
// all inside the font's range, and every icon has both fallbacks.
func TestIconTableMatchesFont(t *testing.T) {
	f, err := os.Open("../../assets/iconfont/icons.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src := map[rune]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := sc.Text(); l != "" && l[0] != '.' && l[0] != '#' {
			parts := strings.Fields(l)
			cp, _ := strconv.ParseUint(parts[0], 16, 32)
			src[rune(cp)] = parts[1]
		}
	}
	if len(src) != int(nIcons) {
		t.Fatalf("font has %d icons, table %d", len(src), nIcons)
	}
	for r, ic := range iconTable {
		if src[ic.sym] != ic.name {
			t.Errorf("role %d: U+%04X is %q in the font, %q here", r, ic.sym, src[ic.sym], ic.name)
		}
		if ic.sym < iconfont.First || ic.sym > iconfont.Last {
			t.Errorf("%s outside the font's range", ic.name)
		}
		if ic.uni == "" || ic.dos == "" {
			t.Errorf("%s lacks a fallback", ic.name)
		}
	}
}

// Private Use glyphs are one column wide in our width function, as in
// terminals; a wider count would misalign every row that has one.
func TestPrivateUseIsOneColumn(t *testing.T) {
	for r := iconfont.First; r <= iconfont.Last; r++ {
		if w := strWidth(string(r)); w != 1 {
			t.Fatalf("U+%04X measures %d", r, w)
		}
	}
	c := newCanvas(4, 1, themes[defaultTheme])
	if x := c.put(0, 0, "", cstyle{}, ""); x != 2 {
		t.Fatalf("two icons end at column %d", x)
	}
}

// Every icon role is drawn from the table: no literal glyph for a role
// outside icons.go (comments aside).
func TestNoIconLiterals(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	roles := []string{"⌕", "⧉", "⋯", "↳", "⊘", "◉", "📌", "⚙", "↩", "▤", "‹›"}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "icons.go" || f == "emoji_data.go" {
			continue
		}
		b, _ := os.ReadFile(f)
		for i, l := range strings.Split(string(b), "\n") {
			code, _, _ := strings.Cut(l, "//")
			for _, g := range roles {
				if strings.Contains(code, g) {
					t.Errorf("%s:%d draws %s directly; use m.icon", f, i+1, g)
				}
			}
		}
	}
}

func iconModel(t *testing.T, prefs string, font FontHooks, ssh bool) *Model {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ui.json")
	if prefs != "" {
		os.WriteFile(p, []byte(prefs), 0o600)
	}
	m := New(context.Background(), newFake(), Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: p, Font: font, SSH: ssh})
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 44})
	return m
}

func fakeFont(installed *bool) FontHooks {
	return FontHooks{
		Installed: func() bool { return *installed },
		Install:   func() error { *installed = true; return nil },
	}
}

// Auto: tc Symbols when the font is here and we aren't over SSH or in a
// CJK locale; else Unicode, or DOS in the Retro skin. A choice wins.
func TestIconPackAuto(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	yes, no := true, false
	for _, c := range []struct {
		prefs string
		font  *bool
		ssh   bool
		want  string
	}{
		{"", &yes, false, packSymbols},
		{"", &yes, true, packUnicode},
		{"", &no, false, packUnicode},
		{`{"skin":"retro"}`, &no, false, packDOS},
		{`{"skin":"retro"}`, &yes, false, packSymbols},
		{`{"icons":"unicode"}`, &yes, false, packUnicode},
		{`{"icons":"dos"}`, &no, false, packDOS},
		{`{"icons":"symbols"}`, &no, true, packSymbols},
	} {
		m := iconModel(t, c.prefs, fakeFont(c.font), c.ssh)
		if m.pack != c.want {
			t.Errorf("%s font=%v ssh=%v: %s, want %s", c.prefs, *c.font, c.ssh, m.pack, c.want)
		}
	}
	t.Setenv("LANG", "ja_JP.UTF-8")
	if m := iconModel(t, "", fakeFont(&yes), false); m.pack != packUnicode {
		t.Fatalf("CJK locale: %s", m.pack)
	}
}

// Settings › Icons picks a pack (saved, previewed live), and installs the
// font when it's missing.
func TestIconSettings(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	installed := false
	m := iconModel(t, `{"icons_hint":true}`, fakeFont(&installed), false)
	m.openSettings("appearance")
	row := -1
	for i, c := range m.setCtls() {
		if c.label == "Icons" {
			row = i
		}
	}
	m.set.row = row
	click(t, m, "set:"+strconv.Itoa(row)+":3") // DOS
	v := checkView(t, m, "dos")
	if m.pack != packDOS || !strings.Contains(v, iconIn(packDOS, icSearch)+" "+iconIn(packDOS, icPin)) {
		t.Fatalf("DOS preview missing (pack %s):\n%s", m.pack, v)
	}
	if b, _ := os.ReadFile(m.ui.path); !strings.Contains(string(b), `"icons": "dos"`) {
		t.Fatalf("not saved: %s", b)
	}
	click(t, m, "set:"+strconv.Itoa(row)+":0") // Auto
	if m.pack != packUnicode || !strings.Contains(checkView(t, m, "auto"), "auto → Unicode") {
		t.Fatal("auto preview")
	}
	if !strings.Contains(checkView(t, m, "button"), "Install tc icons") {
		t.Fatal("no install button without the font")
	}
	m.set.row = row + 1
	press(m, "enter")
	if !installed || !strings.Contains(m.notice, "restart your terminal") {
		t.Fatalf("install: %v %q", installed, m.notice)
	}
	// the running terminal may not draw it yet: the icons stay until restart
	if m.pack != packUnicode {
		t.Fatalf("switched to %s mid-session", m.pack)
	}
	if strings.Contains(checkView(t, m, "installed"), "Install tc icons") {
		t.Fatal("button stayed after installing")
	}
	// the focus moved off the vanished button: a second Enter doesn't
	// change the row that took its place
	touches := m.ui.Touches
	press(m, "enter")
	if m.ui.Touches != touches || m.setCtls()[m.set.row].label != "Icons" {
		t.Fatalf("second Enter changed %q (row %s)", m.ui.Touches, m.setCtls()[m.set.row].label)
	}
	// a failed install says why
	f := FontHooks{Installed: func() bool { return false }, Install: func() error { return errors.New("disk full") }}
	m2 := iconModel(t, `{"icons_hint":true}`, f, false)
	m2.openSettings("appearance")
	run(m2, m2.installIconFont())
	if !m2.noticeErr || !strings.Contains(m2.notice, "disk full") {
		t.Fatalf("failed install: %q", m2.notice)
	}
}

// The "New: tc icons" toast shows once, to someone without the font.
func TestIconsHintOnce(t *testing.T) {
	no, yes := false, true
	p := filepath.Join(t.TempDir(), "ui.json")
	open := func(font *bool, ssh bool) *Model {
		return New(context.Background(), newFake(), Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: p, Font: fakeFont(font), SSH: ssh})
	}
	if m := open(&no, true); strings.Contains(m.notice, "tc icons") {
		t.Fatal("hint over SSH")
	}
	if m := open(&yes, false); strings.Contains(m.notice, "tc icons") {
		t.Fatal("hint with the font installed")
	}
	if m := open(&no, false); m.notice != "New: tc icons · Settings › Icons" {
		t.Fatalf("no hint: %q", m.notice)
	}
	if m := open(&no, false); strings.Contains(m.notice, "tc icons") {
		t.Fatal("hint shown twice")
	}
}

// Touches: a wavy accent underline under @mentions and a dotted info one
// under #channels, encoded as SGR 4:3/4:4 with 58; plain when off.
func TestUnderlineTouches(t *testing.T) {
	cells := func(m *Model) (mention, channel *cell) {
		m.View()
		c := m.lastCanvas
		for y := 0; y < c.h; y++ {
			for x := 0; x+6 < c.w; x++ {
				if c.at(x, y).s == "@" && c.at(x+1, y).s == "a" && mention == nil {
					mention = c.at(x+1, y)
				}
				if c.at(x, y).s == "#" && c.at(x+1, y).s == "b" && c.at(x+2, y).s == "a" && channel == nil {
					channel = c.at(x+1, y)
				}
			}
		}
		return
	}
	for _, on := range []bool{true, false} {
		m, f := newTestModelFake(t, 120, 40)
		m.opt.Touches = on
		f.msgs[10] = append(f.msgs[10], f.msgs[10][4]) // "hey @alice …"
		f.msgs[10][len(f.msgs[10])-1].ID = 999
		f.msgs[10][len(f.msgs[10])-1].Body = "ping @alice, see #backend"
		m.reloadPane(&m.main)
		mc, cc := cells(m)
		if mc == nil || cc == nil {
			t.Fatal("mention or channel not drawn")
		}
		if on != (mc.attr&aCurly != 0 && mc.ul == m.st.th.accent) || on != (cc.attr&aDotted != 0 && cc.ul == m.st.th.info) {
			t.Fatalf("touches=%v: mention %v channel %v", on, mc.attr, cc.attr)
		}
		if cc.fg != m.st.th.info {
			t.Fatal("#channel not in the info color")
		}
		out := m.View()
		if on != (strings.Contains(out, ";4:3;58:") && strings.Contains(out, ";4:4;58:")) {
			t.Fatalf("touches=%v: SGR 4:3/4:4 with 58 in output: %v", on, strings.Contains(out, "4:3"))
		}
	}
	// the setting overrides detection, and NO_COLOR never gets them
	m := newTestModel(t, 80, 24)
	m.setTouches("on")
	if !m.touches() {
		t.Fatal("on")
	}
	m.opt.Touches = true
	m.setTouches("off")
	if m.touches() {
		t.Fatal("off")
	}
	for env, want := range map[string]bool{"TERM_PROGRAM=WezTerm": true, "TERM=xterm-kitty": true, "TERM_PROGRAM=Apple_Terminal": false, "TERM_PROGRAM=WezTerm TMUX=/tmp/x": false, "TERM=xterm-ghostty HERDR_PANE_ID=1": false} {
		vars := map[string]string{}
		for _, kv := range strings.Fields(env) {
			k, v, _ := strings.Cut(kv, "=")
			vars[k] = v
		}
		if got := underlineStyles(func(k string) string { return vars[k] }); got != want {
			t.Errorf("%s: %v", env, got)
		}
	}
}

// IconRows lists every icon in every pack, for tc fonts test.
func TestIconRows(t *testing.T) {
	rows := IconRows()
	if len(rows) != int(nIcons) || rows[0].Name != "tc" || rows[0].Symbol != "" || rows[0].Unicode == "" || rows[0].DOS == "" {
		t.Fatalf("%+v", rows[0])
	}
}

// The font is looked for once, at start: drawing never touches the disk.
func TestFontCheckedOnce(t *testing.T) {
	n := 0
	f := FontHooks{Installed: func() bool { n++; return false }, Install: func() error { return nil }}
	m := iconModel(t, `{"icons_hint":true}`, f, false)
	start := n
	m.openSettings("appearance")
	for i := 0; i < 5; i++ {
		m.View()
		press(m, "down")
	}
	if n != start || start != 1 {
		t.Fatalf("font checked %d times at start, %d more while drawing", start, n-start)
	}
}

// Over SSH there's no install button (the font would land on the server);
// the Icons row says where to install it instead.
func TestNoInstallOverSSH(t *testing.T) {
	no := false
	m := iconModel(t, `{"icons_hint":true}`, fakeFont(&no), true)
	m.openSettings("appearance")
	v := checkView(t, m, "ssh")
	if strings.Contains(v, "Install tc icons") || !strings.Contains(v, "own computer") {
		t.Fatalf("over SSH:\n%s", v)
	}
}

// An installed font is refreshed at start, in the background.
func TestFontRefreshedAtStart(t *testing.T) {
	yes, refreshed := true, false
	f := fakeFont(&yes)
	f.Refresh = func() { refreshed = true }
	m := iconModel(t, `{"icons_hint":true}`, f, false)
	run(m, m.Init())
	if !refreshed {
		t.Fatal("no refresh at start")
	}
	no, again := false, false
	f2 := fakeFont(&no)
	f2.Refresh = func() { again = true }
	m2 := iconModel(t, `{"icons_hint":true}`, f2, false)
	run(m2, m2.Init())
	if again {
		t.Fatal("refreshed a font that isn't installed")
	}
}

// #name follows the server's rule, and only a channel that exists is a
// link: issue numbers, hex colors and URL fragments stay plain text.
func TestChannelLinks(t *testing.T) {
	links := func(s string) []string {
		var out []string
		for _, sp := range parseInline(s, "alice") {
			if sp.f&fChannel != 0 {
				out = append(out, sp.text)
			}
		}
		return out
	}
	got := links("PR #1234 fixes it; color #fff; see #general and example.com/#readme #Foo_Bar #a-b- #x9")
	if strings.Join(got, ",") != "#fff,#general,#a-b,#x9" {
		t.Fatalf("parsed %v", got)
	}
	m, f := newTestModelFake(t, 120, 40)
	f.msgs[10] = append(f.msgs[10], f.msgs[10][4])
	f.msgs[10][len(f.msgs[10])-1].ID = 998
	f.msgs[10][len(f.msgs[10])-1].Body = "see #backend not #fff"
	m.reloadPane(&m.main)
	m.View()
	c := m.lastCanvas
	var backend, fff *cell
	bx, by := -1, -1
	for y := 0; y < c.h; y++ {
		for x := 0; x+4 < c.w; x++ {
			if c.at(x, y).s == "#" && c.at(x+1, y).s == "b" && c.at(x+2, y).s == "a" && c.at(x+3, y).s == "c" {
				backend, bx, by = c.at(x+1, y), x+1, y
			}
			if c.at(x, y).s == "#" && c.at(x+1, y).s == "f" && c.at(x+2, y).s == "f" {
				fff = c.at(x+1, y)
			}
		}
	}
	if backend == nil || fff == nil || backend.fg != m.st.th.info || fff.fg == m.st.th.info {
		t.Fatal("only #backend should be a link")
	}
	m.Update(tea.MouseMsg{X: bx, Y: by, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.active != 11 {
		t.Fatalf("clicking #backend opened %d", m.active)
	}
	// a new channel's name becomes a link without restarting
	f.chans = append(f.chans, f.chans[0])
	f.chans[len(f.chans)-1].ID, f.chans[len(f.chans)-1].Name = 77, "fff"
	m.Update(updateMsg{Channels: true})
	if m.chanNamed("fff") != 77 {
		t.Fatal("new channel not indexed")
	}
}
