package tui

import (
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// A skin is the chrome: frame glyphs, shadows, the desktop pattern, the
// menu bar, the intro. It holds no colors; everything it draws takes the
// theme's tokens, so every skin works with every theme. A new skin is one
// more entry in skins.
type skin struct {
	id, label string
	box       boxRunes
	framed    bool   // dialogs get a frame, not just a panel
	shadow    bool   // floating boxes cast a shadow
	desktop   string // the gutter pattern; "" leaves gutters blank
	menuBar   bool   // row 0 holds a menu bar
	intro     bool   // the zoom intro plays while tc connects (intro.go)
	theme     string // suggested on selection to someone still on the default theme
}

// boxRunes are a frame's corners and edges.
type boxRunes struct{ tl, tr, bl, br, h, v string }

var skins = []*skin{
	{id: "modern", label: "Modern", box: boxRunes{"╭", "╮", "╰", "╯", "─", "│"}},
	{id: "retro", label: "Retro", box: boxRunes{"╔", "╗", "╚", "╝", "═", "║"},
		framed: true, shadow: true, desktop: "░", menuBar: true, intro: true, theme: "turbo"},
}

// skinNamed is the skin with id, or the default.
func skinNamed(id string) *skin {
	for _, s := range skins {
		if s.id == id {
			return s
		}
	}
	return skins[0]
}

func (m *Model) skin() *skin { return skinNamed(m.ui.Skin) }

// setSkin switches the chrome, cross-fading the colors and swapping the
// glyphs halfway. The theme stays, unless the user never picked one and
// the skin suggests its own.
func (m *Model) setSkin(id string) {
	s := skinNamed(id)
	if s == m.skin() {
		return
	}
	m.fx.themeFrom(m)
	m.fx.skinSwap = m.fx.themePrev != nil
	m.ui.Skin = s.id
	if s == skins[0] {
		m.ui.Skin = ""
	}
	if t, ok := themes[s.theme]; ok && m.ui.Theme == "" && t != m.st.th {
		m.applyStyles(newStyles(m.st.color, m.st.prof, t))
		m.ui.Theme = s.theme
	}
	m.savePrefs()
	m.layoutInput()
	m.clampScroll(&m.main)
	m.clampScroll(&m.thread)
	m.setInfo("▣ skin · " + s.id)
}

// shadowOf is a cell color in a shadow: toward black on a dark theme,
// toward the theme's ink on a light one, where black would read as a hole.
func (t *theme) shadowOf(c rgb, text bool) rgb {
	k := 0.7
	if text {
		k = 0.55
	}
	if t.light {
		return mix(c, t.fg, k*0.45)
	}
	return mix(c, 0x000000, k)
}

// patternColor is the desktop pattern's color: quiet on any theme.
func (t *theme) patternColor() rgb { return mix(t.bg, t.faint, 0.4) }

// shade darkens the cells a floating box at r casts its shadow on: two
// columns on the right and one row below, Turbo Vision style. It stays
// inside the clip, so never off screen or over a pane kept clear.
func (m *Model) shade(c *canvas, r rect) {
	if !m.skin().shadow || !m.st.color {
		return
	}
	th := m.st.th
	dark := func(cl *cell) {
		cl.bg, cl.fg = th.shadowOf(cl.bg, false), th.shadowOf(cl.fg, true)
	}
	c.each(r.x1, r.y0+1, 2, r.y1-r.y0, dark)
	c.each(r.x0+2, r.y1, r.x1-r.x0-2, 1, dark)
}

// drawDesktop fills the empty cells of the gutters beside the message
// column (and around the thread pane) with the skin's pattern, never
// behind text and never where a click lands.
func (m *Model) drawDesktop(c *canvas, g geo) {
	pat := m.skin().desktop
	if pat == "" || g.thrFull || (m.mode == modeViewer && m.view.wide) {
		return
	}
	th := m.st.th
	col := th.patternColor()
	if !m.st.color {
		col = th.faint
	}
	fill := func(x0, x1 int) {
		for y := g.T0; y < g.statusY; y++ {
			for x := max(x0, 0); x < min(x1, c.w); x++ {
				cl := c.at(x, y)
				if cl.s == " " && cl.act == 0 && cl.bg == th.bg {
					cl.s, cl.fg, cl.attr = pat, col, 0
				}
			}
		}
	}
	fill(g.L, g.CX-1)
	fill(g.CX+g.CW+2, g.RX)
}

// menuItem is a menu bar entry: the accented letter (⌥ plus it opens the
// entry), the rest of the label and the click action.
type menuItem struct{ key, rest, act string }

var menuItems = []menuItem{
	{"≡", "", "menu:all"}, {"C", "hat", "menu:chat"}, {"V", "iew", "menu:view"},
	{"H", "uddle", "menu:huddle"}, {"T", "heme", "menu:theme"}, {"?", " Help", "menu:help"},
}

// menuKey is the menu entry ⌥key opens, if any.
func menuKey(key string) string {
	for _, it := range menuItems {
		if key == "alt+"+strings.ToLower(it.key) {
			return strings.TrimPrefix(it.act, "menu:")
		}
	}
	return ""
}

// menuRight is the menu bar's right side (clock and who is online) and
// its column; "" when the bar is too narrow to keep it.
func (m *Model) menuRight() (string, int) {
	right := m.now().Format("15:04") + "  ◉ " + m.onlineLabel()
	rx := m.w - 2 - strWidth(right)
	if rx < 30 {
		return "", m.w
	}
	return right, rx
}

// drawMenuBar draws row 0 when the skin has a menu bar: the menus on the
// left, the clock and who is online on the right. When room runs out the
// middle entries go first; ≡ and Help stay.
func (m *Model) drawMenuBar(c *canvas, g geo) {
	if !g.bar {
		return
	}
	th := m.st.th
	c.fill(0, 0, m.w, 1, th.element, "")
	right, rx := m.menuRight()
	if right != "" {
		c.put(rx, 0, right, fgc(th.dim).on(th.element), "")
	}
	width := func(it menuItem) int { return strWidth(it.key) + strWidth(it.rest) + 3 }
	items := append([]menuItem(nil), menuItems...)
	total := 1
	for _, it := range items {
		total += width(it)
	}
	for len(items) > 2 && total > rx-1 {
		total -= width(items[len(items)-2])
		items = append(items[:len(items)-2], items[len(items)-1])
	}
	x := 1
	for _, it := range items {
		if x+width(it)-1 > rx-1 {
			break
		}
		bg, key, rest := th.element, fgb(th.accent), fgc(th.fg)
		if m.hover == it.act {
			bg, key, rest = th.accent, fgb(th.accentFg), fgc(th.accentFg)
		}
		on := func(s cstyle) cstyle {
			if m.hover == it.act {
				s = s.with(aMonoRev)
			}
			return s.on(bg)
		}
		e := c.put(x, 0, " ", on(rest), it.act)
		e = c.put(e, 0, it.key, on(key), it.act)
		e = c.put(e, 0, it.rest, on(rest), it.act)
		x = c.put(e, 0, " ", on(rest), it.act) + 1
	}
}

// onlineLabel is "N online", or the connection state while not online.
func (m *Model) onlineLabel() string {
	if m.status.State != client.Online || m.status.Stopped {
		return strings.ToLower(m.statusText())
	}
	return strconv.Itoa(m.onlineCount()) + " online"
}

func (m *Model) onlineCount() int {
	n := 0
	for _, u := range m.userList {
		if m.presence[u.ID].State == proto.PresOnline {
			n++
		}
	}
	return n
}

// menuAction runs a menu bar entry.
func (m *Model) menuAction(which string) tea.Cmd {
	m.leaveSelect()
	switch which {
	case "all":
		m.openPalette("all")
	case "chat":
		m.openPalette("channels")
	case "view":
		m.openPalette("settings")
	case "theme":
		m.openPalette("settings")
		return m.palRun(palItem{run: "app:themes"})
	case "help":
		m.openHelp()
	case "huddle":
		if m.hud.in {
			return m.runQuiet("/leave-huddle")
		}
		return m.runQuiet("/huddle")
	}
	return nil
}

// clockMsg redraws the menu bar's clock when the minute turns.
type clockMsg struct{}

// clockTick wakes once at the next minute while the menu bar shows a
// clock: the only idle redraw the Retro skin adds.
func (m *Model) clockTick() tea.Cmd {
	if m.clockRun || !m.menuBarShown() {
		return nil
	}
	if right, _ := m.menuRight(); right == "" {
		return nil
	}
	m.clockRun = true
	now := m.now()
	d := now.Truncate(time.Minute).Add(time.Minute).Sub(now)
	return tea.Tick(d, func(time.Time) tea.Msg { return clockMsg{} })
}
