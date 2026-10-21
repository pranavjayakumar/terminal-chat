package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/sanitize"
)

// Settings (Ctrl-, where the terminal sends it, /settings, the palette, ⚙).
// Appearance is stored in ui.json; Notifications runs the existing /notify,
// /sounds and /dnd commands; Account and About show what tc already knows.

var setPages = [...]struct{ id, label string }{
	{"appearance", "Appearance"}, {"notifications", "Notifications"}, {"keys", "Keys"},
	{"account", "Account & devices"}, {"about", "About"},
}

type setState struct {
	page int
	row  int // focused control on the page
}

// setCtl is one control: a row of choices, or a toggle (two choices).
type setCtl struct {
	label, sub string
	opts       []string // choice labels; "~x" is shown greyed (later)
	cur        int
	toggle     bool
	pick       func(i int) tea.Cmd
	themes     bool // the theme card grid
	button     string
}

func (m *Model) openSettings(page string) {
	m.leaveSelect()
	m.mode = modeSettings
	m.fx.dialogAt = m.now()
	m.set.row = 0
	for i, p := range setPages {
		if p.id == page {
			m.set.page = i
		}
	}
}

func (m *Model) setMotion(on bool) {
	m.fx.off = !on
	m.ui.NoMotion = !on
	m.savePrefs()
	m.applyCursorMode()
}

// setSplit picks the message layout: everyone on the left, or ours on the
// right.
func (m *Model) setSplit(on bool) {
	if m.ui.Split == on {
		return
	}
	m.ui.Split = on
	m.savePrefs()
	m.main.ver++
	m.thread.ver++
}

func (m *Model) setCompact(on bool) {
	m.ui.Compact = on
	m.savePrefs()
	m.main.ver++
	m.thread.ver++
}

func (m *Model) setSidebar(mode string) {
	if mode == m.ui.sidebarMode() {
		return
	}
	m.fx.sbFrom = m.sbW()
	m.ui.Sidebar = mode
	m.fx.sbTo = sbWidth(m.sbMode())
	m.fx.sbAt = m.now()
	m.savePrefs()
	m.layoutInput()
	m.clampScroll(&m.main)
	m.clampScroll(&m.thread)
}

// cycleSidebar is Ctrl-B: normal → wide → icons → normal, saved. Below
// sbIconsBelow columns the size is automatic, so Ctrl-B instead cycles
// icons → normal → hidden for this session only.
func (m *Model) cycleSidebar() {
	if m.w < minSidebarCols {
		m.setInfo("too narrow for sidebar")
		return
	}
	if m.w < sbIconsBelow {
		cur := m.sbMode()
		if cur == "" {
			cur = sbHidden
		}
		next := map[string]string{sbIcons: sbNormal, sbNormal: sbHidden, sbHidden: sbIcons, sbWide: sbHidden}[cur]
		if next == sbNormal && m.w < sbHideBelow {
			next = sbHidden // a full sidebar would leave the chat too little room
		}
		m.fx.sbFrom = m.sbW()
		m.sbForce = next
		m.fx.sbTo, m.fx.sbAt = sbWidth(m.sbMode()), m.now()
		m.setInfo("sidebar: " + next + " (at this width; wider terminals use your setting)")
		m.layoutInput()
		m.clampScroll(&m.main)
		m.clampScroll(&m.thread)
		return
	}
	next := map[string]string{sbNormal: sbWide, sbWide: sbIcons, sbIcons: sbNormal}[m.ui.sidebarMode()]
	m.setSidebar(next)
}

func (m *Model) runQuiet(s string) tea.Cmd {
	cmd, err := m.runCommand(s)
	if err != nil {
		m.setErr("", err)
	}
	return cmd
}

func (m *Model) setCtls() []setCtl {
	switch setPages[m.set.page].id {
	case "appearance":
		cur := 0
		for i, id := range themeOrder {
			if themes[id] == m.st.th {
				cur = i
			}
		}
		sb := map[string]int{sbIcons: 0, sbNormal: 1, sbWide: 2}[m.ui.sidebarMode()]
		return []setCtl{
			{label: "Theme", sub: "←/→ or click to switch", themes: true, cur: cur, opts: themeOrder,
				pick: func(i int) tea.Cmd { m.setTheme(themeOrder[i]); return nil }},
			{label: "Density", opts: []string{"Cozy", "Compact"}, cur: boolInt(m.ui.Compact),
				pick: func(i int) tea.Cmd { m.setCompact(i == 1); return nil }},
			{label: "Sidebar", sub: "or click its edge, or ⌃B", opts: []string{"Icons", "Normal", "Wide"}, cur: sb,
				pick: func(i int) tea.Cmd { m.setSidebar([]string{sbIcons, sbNormal, sbWide}[i]); return nil }},
			{label: "Messages", sub: "split puts yours on the right", opts: []string{"Left", "Split"}, cur: boolInt(m.ui.Split),
				pick: func(i int) tea.Cmd { m.setSplit(i == 1); return nil }},
			{label: "Show people as", sub: "more styles later", opts: []string{"Name", "~Stripe", "~Tag", "~Shape"},
				pick: func(int) tea.Cmd { return nil }},
			{label: "Animations", sub: "fades, pops, slides · off = instant", toggle: true, cur: boolInt(!m.fx.off),
				pick: func(i int) tea.Cmd { m.setMotion(i == 1); return nil }},
		}
	case "notifications":
		p := m.nt.prefs
		lvl := map[client.NotifyLevel]int{client.LevelAll: 0, client.LevelMentions: 1, client.LevelOff: 2}[p.Default]
		if p.Default == client.LevelDefault {
			lvl = 1
		}
		content := map[string]int{"": 0, "name": 1, "none": 2}[p.Content]
		dnd := p.DNDUntil > m.now().UnixMilli()
		quietSub := "22:00 – 08:00 when turned on"
		if p.Quiet != "" {
			quietSub = sanitize.Line(p.Quiet)
		}
		dndSub := "pause everything for 2 h"
		if dnd {
			dndSub = "until " + m.untilLabel(p.DNDUntil)
		}
		return []setCtl{
			{label: "Notify me about", sub: "default for channels · per channel: /notify there", opts: []string{"Everything", "Mentions & DMs", "Nothing"}, cur: lvl,
				pick: func(i int) tea.Cmd { return m.runQuiet("/notify global " + []string{"all", "mentions", "off"}[i]) }},
			{label: "Popups show", opts: []string{"Sender and text", "Sender only", "Nothing"}, cur: content,
				pick: func(i int) tea.Cmd { return m.runQuiet("/notify content " + []string{"full", "name", "none"}[i]) }},
			{label: "Sounds", sub: "/sounds picks which", toggle: true, cur: boolInt(!p.SoundOff),
				pick: func(i int) tea.Cmd { return m.runQuiet("/sounds " + []string{"off", "on"}[i]) }},
			{label: "Quiet hours", sub: quietSub, toggle: true, cur: boolInt(p.Quiet != ""),
				pick: func(i int) tea.Cmd {
					return m.runQuiet("/notify quiet " + []string{"off", "22:00-08:00"}[i])
				}},
			{label: "Do not disturb", sub: dndSub, toggle: true, cur: boolInt(dnd),
				pick: func(i int) tea.Cmd { return m.runQuiet("/dnd " + []string{"off", "2h"}[i]) }},
			{label: "Send a test", button: "/notify test", pick: func(int) tea.Cmd { return m.runQuiet("/notify test") }},
		}
	case "account":
		return []setCtl{
			{label: "Devices", sub: "revoke a lost one; keys rotate so it reads nothing new", button: "/devices", pick: func(int) tea.Cmd { m.mode = modeNormal; return m.runQuiet("/devices") }},
		}
	}
	return nil
}

func (m *Model) settingsKey(key string) tea.Cmd {
	ctls := m.setCtls()
	s := &m.set
	if s.row >= len(ctls) {
		s.row = max(len(ctls)-1, 0)
	}
	switch key {
	case kEsc, "q":
		m.mode = modeNormal
		return nil
	case kTab:
		s.page, s.row = (s.page+1)%len(setPages), 0
		return nil
	case kShiftTab:
		s.page, s.row = (s.page+len(setPages)-1)%len(setPages), 0
		return nil
	case kUp, "k":
		if s.row > 0 {
			s.row--
		}
		return nil
	case kDown, "j":
		if s.row < len(ctls)-1 {
			s.row++
		}
		return nil
	}
	if setPages[s.page].id == "keys" && (key == kEnter || key == " ") {
		m.openHelp()
		return nil
	}
	if len(ctls) == 0 {
		return nil
	}
	c := ctls[s.row]
	n := len(c.opts)
	if c.toggle {
		n = 2
	}
	move := func(d int) tea.Cmd {
		i := c.cur + d
		for i >= 0 && i < n && c.opts != nil && strings.HasPrefix(c.opts[i], "~") {
			i += d
		}
		if i < 0 || i >= n {
			return nil
		}
		return c.pick(i)
	}
	switch key {
	case "left", "h":
		return move(-1)
	case "right", "l":
		return move(1)
	case kEnter, " ":
		switch {
		case c.button != "":
			return c.pick(0)
		case c.toggle:
			return c.pick(1 - c.cur)
		case c.themes:
			return nil
		default:
			return move(1)
		}
	}
	return nil
}

// settingsClick handles a click on a settings control: "set:<row>:<opt>".
func (m *Model) settingsClick(a string) tea.Cmd {
	parts := strings.Split(a, ":")
	switch {
	case parts[0] == "setpage" && len(parts) == 2:
		i, _ := strconv.Atoi(parts[1])
		if setPages[i].id == "keys" {
			m.openHelp()
			return nil
		}
		m.set.page, m.set.row = i, 0
		return nil
	case parts[0] == "set" && len(parts) == 3:
		row, _ := strconv.Atoi(parts[1])
		opt, _ := strconv.Atoi(parts[2])
		ctls := m.setCtls()
		if row >= len(ctls) {
			return nil
		}
		m.set.row = row
		c := ctls[row]
		if c.opts != nil && opt < len(c.opts) && strings.HasPrefix(c.opts[opt], "~") {
			m.setInfo("more styles are coming later")
			return nil
		}
		if c.toggle {
			opt = 1 - c.cur
		}
		return c.pick(opt)
	}
	return nil
}

func (m *Model) drawSettings(c *canvas) {
	th := m.st.th
	r := m.dialog(c, 116, 38, "Settings")
	old0 := c.setClip(r)
	defer c.setClip(old0)
	w, h := r.x1-r.x0, r.y1-r.y0
	navW := 24
	if w < 70 {
		navW = 0
	}
	top := r.y0 + 3
	if h < 14 {
		top = r.y0 + 2
	}
	if navW > 0 {
		c.fill(r.x0+1, top, navW, r.y1-top-1, th.panel, "")
		for i, p := range setPages {
			on := i == m.set.page
			bg := th.panel
			if on {
				bg = th.element
			}
			a := "setpage:" + strconv.Itoa(i)
			c.fill(r.x0+2, top+i, navW-2, 1, bg, a)
			c.tag(r.x0+2, top+i, navW-2, 1, a, !on, true)
			if on {
				c.put(r.x0+1, top+i, "▎", fgc(th.accent), a)
			}
			st := fgc(th.dim)
			if on {
				st = fgb(th.fg).with(aMonoRev)
			}
			c.put(r.x0+3, top+i, p.label, st, a)
		}
	} else {
		// narrow: the page name with ⇥ to move
		c.put(r.x0+2, top, setPages[m.set.page].label+"  ⇥", fgb(th.accent), "")
		top += 2
	}
	cx := r.x0 + navW + 5
	if navW == 0 {
		cx = r.x0 + 2
	}
	cw := r.x1 - cx - 2
	old := c.setClip(rect{cx - 2, top, cx + cw, r.y1 - 1}) // from the › focus mark
	defer c.setClip(old)
	y := top
	label := func(s, sub string, focused bool) {
		st := fgb(th.fg)
		if focused {
			st = fgb(th.accent)
			c.put(cx-2, y, "›", fgc(th.accent), "")
		}
		c.put(cx, y, s, st, "")
		if sx := cx + strWidth(s) + 2; sub != "" && cx+cw-sx > 4 {
			c.put(sx, y, truncate(sub, cx+cw-sx), fgc(th.faint), "")
		}
		y++
	}
	switch setPages[m.set.page].id {
	case "keys":
		label("Keyboard shortcuts", "", true)
		y++
		c.put(cx, y, "Enter or F1 opens the keys sheet.", fgc(th.dim), "")
		c.pill(cx, y+2, " Open keys sheet ", fgc(th.info).on(th.element), "app:keys")
		return
	case "account":
		label("You", "", false)
		y++
		e := c.put(cx, y, sanitize.Line(m.me.Name), fgb(th.nick(m.me.ID)), "")
		if m.me.Admin {
			c.put(e+2, y, "admin", fgc(th.faint), "")
		}
		if d := sanitize.Line(m.me.Display); d != "" {
			c.put(e+10, y, d, fgc(th.dim), "")
		}
		y += 2
		m.drawCtls(c, cx, y, cw, r.y1-1)
		y += 3
		devs := m.c.Devices(m.me.ID)
		for i, d := range devs {
			if i >= 6 {
				c.put(cx, y, "… /devices lists all", fgc(th.faint), "")
				y++
				break
			}
			if d.Former || d.Revoked {
				continue
			}
			c.put(cx, y, "▪ "+d.Short(), fgc(th.fg), "")
			c.put(cx+16, y, d.Created.Local().Format("2 Jan 2006"), fgc(th.faint), "")
			if d.This {
				c.put(cx+30, y, "this device", fgc(th.accent), "")
			}
			y++
		}
		if err := m.c.DeviceError(); err != nil {
			c.put(cx, y, truncate("⚠ "+sanitize.Line(err.Error()), cw), fgc(th.warn), "")
			y++
		}
		return
	case "about":
		label("tc", "terminal chat", false)
		y++
		v := m.opt.Version
		if v == "" {
			v = "dev"
		}
		e := c.put(cx, y, sanitize.Line(v), fgc(th.dim), "")
		if m.newRelease != "" {
			c.put(e+3, y, "update available: "+sanitize.Line(m.newRelease)+" · tc update", fgc(th.info), "")
		} else if m.opt.UpdateCheck != nil {
			c.put(e+3, y, "up to date (checked at start)", fgc(th.faint), "")
		}
		y += 2
		c.put(cx, y, "end-to-end encrypted DMs and private channels", fgc(th.faint), "")
		y += 2
		if p := m.ui.path; p != "" {
			c.put(cx, y, truncate("settings: "+p, cw), fgc(th.faint), "")
		}
		return
	}
	m.drawCtls(c, cx, y, cw, r.y1-1)
}

// themeRows is how many rows of cards the grid has at width w, and whether
// they are the one-line compact kind.
func themeGrid(w, room int) (cols, rows int, compact bool) {
	cols = 5
	for cols > 1 && cols*16 > w {
		cols--
	}
	rows = (len(themeOrder) + cols - 1) / cols
	return cols, rows, room < rows*6+14
}

// ctlHeight is how many rows drawCtls gives a control.
func ctlHeight(ctl setCtl, cw, room int) int {
	switch {
	case ctl.themes:
		_, rows, compact := themeGrid(cw, room)
		if compact {
			return rows*2 + 2
		}
		return rows*6 + 2
	case ctl.toggle:
		return 2
	}
	return 3
}

// drawCtls draws the page's controls from row y down to bottom, scrolled so
// the focused one is in view.
func (m *Model) drawCtls(c *canvas, cx, y, cw, bottom int) {
	th := m.st.th
	ctls := m.setCtls()
	room := bottom - y
	start, focusTop, focusEnd := 0, 0, 0
	for row, ctl := range ctls {
		h := ctlHeight(ctl, cw, room)
		if row == m.set.row {
			focusTop, focusEnd = start, start+h
		}
		start += h
	}
	off := 0
	if focusEnd > room {
		off = focusEnd - room
	}
	off = min(off, focusTop)
	more := start-off > room && room > 2
	if more {
		defer func() {
			c.fill(cx-2, bottom-1, cw+2, 1, th.panel, "")
			c.put(cx+cw-8, bottom-1, " ↓ more ", fgc(th.faint), "")
		}()
	}
	y -= off
	for row, ctl := range ctls {
		focused := row == m.set.row
		st := fgb(th.fg)
		if focused {
			st = fgb(th.accent)
			c.put(cx-2, y, "›", fgc(th.accent), "")
		}
		if !ctl.toggle {
			c.put(cx, y, ctl.label, st, "")
			if sx := cx + strWidth(ctl.label) + 2; ctl.sub != "" && cx+cw-sx > 4 {
				c.put(sx, y, truncate(ctl.sub, cx+cw-sx), fgc(th.faint), "")
			}
			y++
		}
		switch {
		case ctl.themes:
			y = m.drawThemeCards(c, cx, y+1, cw, room, row) + 1
		case ctl.toggle:
			on := ctl.cur == 1
			a := "set:" + strconv.Itoa(row) + ":0"
			knob, bg, fg := "●   ", th.element, th.faint
			if on {
				knob, bg, fg = "   ●", th.accent, th.accentFg
			}
			kst := fgb(fg).on(bg)
			if on {
				kst = kst.with(aMonoRev)
			}
			c.pill(cx, y, knob, kst, a)
			e := c.put(cx+8, y, ctl.label, st, a)
			if ctl.sub != "" && cx+cw-e-2 > 4 {
				c.put(e+2, y, truncate(ctl.sub, cx+cw-e-2), fgc(th.faint), "")
			}
			y += 2
		case ctl.button != "":
			c.pill(cx, y, " "+ctl.button+" ", fgc(th.info).on(th.element), "set:"+strconv.Itoa(row)+":0")
			y += 2
		default:
			x := cx
			for i, o := range ctl.opts {
				later := strings.HasPrefix(o, "~")
				o = strings.TrimPrefix(o, "~")
				on := i == ctl.cur
				bg, fg := th.element, th.dim
				switch {
				case on:
					bg, fg = th.accent, th.accentFg
				case later:
					fg = th.faint
				}
				st := fgc(fg).on(bg)
				if on {
					st = st.with(aBold | aMonoRev)
				}
				if x+strWidth(o)+4 > cx+cw {
					break
				}
				x = c.pill(x, y, " "+o+" ", st, "set:"+strconv.Itoa(row)+":"+strconv.Itoa(i)) + 1
			}
			y += 2
		}
	}
}

// drawThemeCards draws the 5×2 grid of theme previews; it returns the row
// after it.
func (m *Model) drawThemeCards(c *canvas, x, y, w, room, row int) int {
	th := m.st.th
	cols, rows, compact := themeGrid(w, room)
	for k, id := range themeOrder {
		t := themes[id]
		a := "set:" + strconv.Itoa(row) + ":" + strconv.Itoa(k)
		cur := t == th
		mark := "  "
		if cur {
			mark = "✓ "
		}
		if compact {
			// one line per card: the theme's own colors and name
			bx, by := x+(k%cols)*16, y+(k/cols)*2
			c.fill(bx, by, 14, 1, t.bg, a)
			c.put(bx, by, "▌", fgc(t.accent), a)
			st := fgc(t.fg)
			if cur {
				st = fgb(t.accent)
			}
			c.put(bx+1, by, mark+id, st.on(t.bg), a)
			continue
		}
		bx, by := x+(k%cols)*16, y+(k/cols)*6
		c.fill(bx, by, 14, 4, t.bg, a)
		c.fill(bx, by, 3, 4, t.panel, a)
		c.put(bx+4, by+1, "│", fgc(t.nicks[1]), a)
		c.put(bx+6, by+1, "────", fgc(t.fg), a)
		c.put(bx+7, by+2, "────", fgc(t.dim), a)
		c.put(bx+12, by+2, "│", fgc(t.accent), a)
		st := fgc(th.dim)
		if cur {
			st = fgb(th.accent)
		}
		c.put(bx, by+4, mark+id, st, a)
	}
	if compact {
		return y + rows*2 - 1
	}
	return y + rows*6 - 1
}
