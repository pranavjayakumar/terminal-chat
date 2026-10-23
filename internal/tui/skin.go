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
	intro     string // how it opens after connecting: introZoom or introFade (intro.go)
	theme     string // suggested on selection to someone still on the default theme
	inputPad  int    // blank rows under the input's text, inside its box
	// defined pulls a theme's surfaces apart (colors) so frames, bars and
	// shadows read on every theme, near-black ones included.
	defined bool
	cache   map[*theme]*theme
}

// boxRunes are a frame's corners and edges.
type boxRunes struct{ tl, tr, bl, br, h, v string }

var skins = []*skin{
	{id: "modern", label: "Modern", box: boxRunes{"╭", "╮", "╰", "╯", "─", "│"}, intro: introFade},
	{id: "retro", label: "Retro", box: boxRunes{"╔", "╗", "╚", "╝", "═", "║"},
		framed: true, shadow: true, desktop: "░", menuBar: true, intro: introZoom, theme: "turbo", inputPad: 1,
		defined: true},
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

// Minimum contrasts a defined skin keeps between surfaces: a panel or
// window against the background, a bar or selection, a frame against its
// panel. Turbo already meets them, so it stays exactly the mockup's.
const (
	apartPanel = 1.1
	apartBar   = 1.5
	apartFrame = 3.0
	// a background so dark that a shadow can't be darker gets lifted
	// toward its line color until the shadow shows
	apartShadow = 1.25
)

// colors is t as this skin draws it. A defined skin moves panel, element
// and line toward fg just far enough to keep the minimum contrasts; every
// other token stays. The result is made once per theme.
func (s *skin) colors(t *theme) *theme {
	if !s.defined {
		return t
	}
	if c, ok := s.cache[t]; ok {
		return c
	}
	orig, c := t, *t
	if t.groundable() {
		// a neutral near-black background becomes a deep ground in the
		// accent's hue, like turbo's blue: sky navy, rose wine, ember brown
		h := t.accent.hue()
		c.bg, c.panel = shadeOf(h, 0.6, 0.018), shadeOf(h, 0.6, 0.012)
		c.element, c.line = shadeOf(h, 0.5, 0.06), shadeOf(h, 0.5, 0.28)
		base := c
		t = &base // the steps below work from the ground
	}
	for k := 0.0; !t.light && k <= 1; k += 0.05 {
		if c.bg = mix(t.bg, t.line, k); contrast(c.bg, t.shadowOf(c.bg, false)) >= apartShadow {
			break
		}
	}
	c.panel = apart(t.panel, c.bg, t.fg, apartPanel)
	c.element = apart(t.element, c.bg, t.fg, apartBar)
	c.line = apart(t.line, c.panel, t.fg, apartFrame)
	// text keeps reading on every surface
	for _, s := range []rgb{c.bg, c.panel, c.element} {
		c.dim = apart(c.dim, s, t.fg, 4.5)
		c.faint = apart(c.faint, s, t.fg, 2.6)
	}
	for i := range c.nicks {
		c.nicks[i] = apart(c.nicks[i], c.bg, t.fg, 3)
	}
	if s.cache == nil {
		s.cache = map[*theme]*theme{}
	}
	s.cache[orig] = &c
	return &c
}

// shadeOf is the color of hue h and saturation s whose luminance is lum,
// so grounds of every hue sit equally deep (green looks brighter than blue
// at the same HSL lightness).
func shadeOf(h, s, lum float64) rgb {
	for l := 0.02; l < 1; l += 0.005 {
		if c := hsl(h, s, l); c.luminance() >= lum {
			return c
		}
	}
	return hsl(h, s, 1)
}

// groundable: a dark theme on a neutral near-black (gray within 12 of
// 255), so a skin may give it a colored ground.
func (t *theme) groundable() bool {
	return !t.light && !t.plainGround && t.bg.chroma() < 12 && t.bg.luminance() < 0.02
}

// apart moves c toward to until it stands at least min off from.
func apart(c, from, to rgb, min float64) rgb {
	for k := 0.0; k <= 1; k += 0.05 {
		if x := mix(c, to, k); contrast(x, from) >= min {
			return x
		}
	}
	return to
}

// themeStyles builds styles for theme t as the current skin draws it.
func (m *Model) themeStyles(t *theme) *styles {
	return newStyles(m.st.color, m.st.prof, m.skin().colors(t))
}

// barColors are the menu bar's: background, text, hotkey letters, quiet
// text. A theme with dialog colors (qbasic) uses them, like DOS menus; a
// theme's hot color, when set, makes the hotkeys pop where the accent
// can't (macintosh's is black).
func (t *theme) barColors() (bg, fg, key, dim rgb) {
	if d := t.dialog; d != nil {
		return d.panel, d.fg, d.accent, d.dim
	}
	key = t.accent
	if t.hot != 0 {
		key = t.hot
	}
	return t.element, t.fg, key, t.dim
}

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
	if _, ok := themes[s.theme]; ok && m.ui.Theme == "" {
		m.ui.Theme = s.theme
	}
	m.applyStyles(m.themeStyles(themeNamed(m.ui.themeName())))
	m.resolvePack()
	m.savePrefs()
	m.layoutInput()
	m.clampScroll(&m.main)
	m.clampScroll(&m.thread)
	m.setInfo("▣ skin · " + s.id)
}

// shadowOf is a cell color in a shadow: toward black on a dark theme,
// toward the theme's ink on a light one, where black would read as a hole.
func (t *theme) shadowOf(c rgb, text bool) rgb {
	k := 0.75
	if text {
		k = 0.6
	}
	if t.light {
		return mix(c, t.fg, k*0.45)
	}
	return mix(c, 0x000000, k)
}

// patternContrast is how far the desktop pattern stands off what it is
// drawn on: about the Borland mockup's, visible yet quieter than faint text.
// A ░ covers a quarter of its cell, so less reads as a flat lighter fill.
const patternContrast = 2.0

// patternOn is the desktop pattern's color on bg: the theme's line color
// 45% of the way from bg (the mockup's), pushed further, toward fg if line
// sits too close to bg, until it shows.
func (t *theme) patternOn(bg rgb) rgb {
	to := t.line
	if contrast(to, bg) < patternContrast {
		to = t.fg
	}
	for k := 0.45; k < 1; k += 0.05 {
		if c := mix(bg, to, k); contrast(c, bg) >= patternContrast {
			return c
		}
	}
	return to
}

// shade darkens the cells a floating box at r casts its shadow on: two
// columns on the right and one row below, Turbo Vision style. It stays
// inside the clip, so never off screen or over a pane kept clear.
func (m *Model) shade(c *canvas, r rect) {
	if !m.skin().shadow || !m.st.color {
		return
	}
	th := m.st.th
	pat := m.skin().desktop
	dark := func(cl *cell) {
		if cl.s == pat && pat != "" {
			cl.s = " " // a shadow is solid, like the mockup's
		}
		cl.bg, cl.fg = th.shadowOf(cl.bg, false), th.shadowOf(cl.fg, true)
	}
	c.each(r.x1, r.y0+1, 2, r.y1-r.y0, dark)
	c.each(r.x0+2, r.y1, r.x1-r.x0-2, 1, dark)
}

// drawDesktop fills the empty cells of the gutters beside the message
// column (and around the thread pane) with the skin's pattern, never
// behind text and never where a click lands. Its color follows each
// cell's own background, so a tinted gutter still shows it.
func (m *Model) drawDesktop(c *canvas, g geo) {
	pat := m.skin().desktop
	if pat == "" || g.thrFull || (m.mode == modeViewer && m.view.wide) {
		return
	}
	th := m.st.th
	cols := map[rgb]rgb{} // a frame has a few backgrounds; each is worked out once
	col := func(bg rgb) rgb {
		if !m.st.color {
			return th.faint
		}
		if c, ok := cols[bg]; ok {
			return c
		}
		cols[bg] = th.patternOn(bg)
		return cols[bg]
	}
	fill := func(x0, x1 int) {
		for y := g.T0; y < g.statusY; y++ {
			for x := max(x0, 0); x < min(x1, c.w); x++ {
				cl := c.at(x, y)
				if cl.s == " " && cl.w == 1 && cl.act == 0 {
					cl.s, cl.fg, cl.attr = pat, col(cl.bg), 0
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

// drawMenuBar draws row 0 when the skin has a menu bar, laid out as the
// mockup: " ≡ " from column 1, the menus one space apart, the clock and
// who is online ending 5 columns from the right. When room runs out the
// middle menus go first; ≡ and Help stay.
func (m *Model) drawMenuBar(c *canvas, g geo) {
	if !g.bar {
		return
	}
	th := m.st.th
	barBg, barFg, barKey, barDim := th.barColors()
	c.fill(0, 0, m.w, 1, barBg, "")
	right, rx := m.menuRight()
	if right != "" {
		c.put(rx, 0, right, fgc(barDim).on(barBg), "")
	}
	width := func(i int, it menuItem) int {
		if i == 0 {
			return 3 // " ≡ "
		}
		return strWidth(it.key) + strWidth(it.rest)
	}
	items := append([]menuItem(nil), menuItems...)
	total := 1
	for i, it := range items {
		total += width(i, it) + 1
	}
	for len(items) > 2 && total > rx-1 {
		total -= width(1, items[len(items)-2]) + 1
		items = append(items[:len(items)-2], items[len(items)-1])
	}
	x := 1
	for i, it := range items {
		if x+width(i, it) > rx-1 {
			break
		}
		bg, key, rest := barBg, fgb(barKey), fgc(barFg)
		if m.hover == it.act {
			bg, key, rest = th.accent, fgb(th.accentFg).with(aMonoRev), fgc(th.accentFg).with(aMonoRev)
		}
		if i == 0 {
			x = c.put(x, 0, " "+m.icon(icTC)+" ", key.on(bg), it.act) + 1
			continue
		}
		e := c.put(x, 0, it.key, key.on(bg), it.act)
		x = c.put(e, 0, it.rest, rest.on(bg), it.act) + 1
	}
}

// menuRight is the menu bar's right side (clock and who is online) and
// its column; "" when the bar is too narrow to keep it.
func (m *Model) menuRight() (string, int) {
	right := m.now().Format("15:04") + "  " + m.icon(icPerson) + " " + m.onlineLabel()
	rx := m.w - 5 - strWidth(right)
	if rx < 30 {
		return "", m.w
	}
	return right, rx
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
