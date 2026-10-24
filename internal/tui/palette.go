package tui

import (
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/sanitize"
)

// The command palette (Option-K): the quick switcher's fuzzy jump list plus
// commands and settings, in tabs. Every command row runs an existing slash
// command or opens an existing view.

var palTabs = [...]struct{ id, label string }{
	{"all", "All"}, {"channels", "Channels"}, {"people", "People"}, {"commands", "Commands"}, {"settings", "Settings"},
}

// palItem is one row. A row with sw set jumps like the switcher did.
type palItem struct {
	group  string
	label  string
	desc   string
	meta   string
	icon   string
	iconFg rgb
	iconBg rgb // a swatch: drawn on its own background, selected or not
	// Colors that follow the theme are resolved when drawn, so a dialog
	// recolored by its theme (qbasic) gets its own: info/faint icons, a
	// person's name color, their presence dot.
	iconTone   byte // 'i' info, 'f' faint
	nick, pres uint64
	sw         *switchItem
	run        string // "/cmd args" to run, "/cmd " to type, or an app action
	tabs       string // which tabs list it: letters a c p m s
	score      int
	order      int
}

type palState struct {
	tab   int
	items []palItem
	shown []palItem
	sel   int
}

func (m *Model) palCommands() []palItem {
	out := []palItem{
		{group: "Commands", label: "Settings", meta: "/settings", icon: m.icon(icSettings), run: "app:settings", tabs: "ams"},
		{group: "Commands", label: "Keyboard shortcuts", meta: "F1", icon: "⌨", run: "app:keys", tabs: "ams"},
		{group: "Commands", label: "Toggle sidebar size", meta: "⌃B", icon: "◧", run: "app:sidebar", tabs: "am"},
		{group: "Commands", label: "Change theme…", icon: "◐", run: "app:themes", tabs: "am"},
		m.agentPalItem(),
	}
	if p, _, ok := m.waitingApproval(); ok {
		out = append(out, palItem{group: "Commands", label: "Approval waiting · " + m.chanTitle(p.ch), desc: sanitize.Line(p.action) + "?", meta: "⌥O", icon: "✋", run: "app:approval", tabs: "am"})
	}
	out = append(out, []palItem{
		{group: "Messages", label: "Activity", desc: "mentions, replies, reactions, DMs", meta: "⌥A", icon: "@", run: "/activity", tabs: "m"},
		{group: "Messages", label: "Search", meta: "/search", icon: m.icon(icSearch), run: "/search ", tabs: "m"},
		{group: "Messages", label: "Insert emoji", desc: "picker; + reacts in select mode, :name completes", meta: "⌥E", icon: m.icon(icReact), run: "app:emoji", tabs: "m"},
		{group: "Messages", label: "Pinned messages", meta: "/pins", icon: m.icon(icPin), run: "/pins", tabs: "m"},
		{group: "Messages", label: "Remind me", meta: "/remind", icon: "⏰", run: "/remind ", tabs: "m"},
		{group: "You", label: "Set away", meta: "/away", icon: "◑", run: "/away", tabs: "m"},
		{group: "You", label: "Back from away", meta: "/back", icon: m.icon(icOnline), run: "/back", tabs: "m"},
		{group: "You", label: "Do not disturb", meta: "/dnd", icon: "☾", run: "/dnd 2h", desc: "for 2 hours", tabs: "m"},
		{group: "You", label: "Notifications", meta: "/notify", icon: "⚑", run: "app:settings:notifications", tabs: "ms"},
		{group: "You", label: "Quit", meta: "⌃C ⌃C", icon: "✕", run: "/quit", tabs: "m"},
	}...)
	if m.thread.open() {
		out = append(out, palItem{group: "Commands", label: "Close thread", meta: "esc", icon: m.icon(icThread), run: "app:closethread", tabs: "am"})
	}
	if m.active != 0 {
		hud := palItem{group: "Commands", label: "Join huddle here", meta: "/huddle", icon: m.icon(icHuddle), iconTone: 'i', run: "/huddle", tabs: "am"}
		if m.hud.in {
			hud.label, hud.meta, hud.run = "Leave huddle", "/leave-huddle", "/leave-huddle"
		}
		out = append(out, hud)
	}
	for _, c := range m.compCommands() {
		it := palItem{group: "All commands", label: "/" + c.name, desc: c.desc, icon: "/", tabs: "m", run: "/" + c.name}
		if c.args != "" {
			it.label += " " + c.args
			it.run += " "
		}
		out = append(out, it)
	}
	return out
}

func (m *Model) palSettings() []palItem {
	out := []palItem{
		{group: "Settings", label: "Appearance", desc: "theme, density, sidebar", icon: "◐", run: "app:settings:appearance", tabs: "s"},
		{group: "Settings", label: "Notifications", desc: "mentions, sounds, quiet hours", icon: "⚑", run: "app:settings:notifications", tabs: "s"},
		{group: "Settings", label: "Keyboard shortcuts", meta: "F1", icon: "⌨", run: "app:keys", tabs: "s"},
		{group: "Settings", label: "Account & devices", desc: "you, your devices", icon: m.icon(icE2E), run: "app:settings:account", tabs: "s"},
		{group: "Settings", label: "About", desc: "version, updates", icon: "ⓘ", run: "app:settings:about", tabs: "s"},
	}
	anim := "Animations: on"
	if m.fx.off {
		anim = "Animations: off"
	}
	out = append(out, palItem{group: "Settings", label: anim, desc: "fades, pops, slides", icon: "✦", run: "app:motion", tabs: "s"})
	layout := "Messages: left"
	if m.ui.Split {
		layout = "Messages: split"
	}
	out = append(out, palItem{group: "Settings", label: layout, desc: "everyone on the left, or yours on the right", icon: "≡", run: "app:split", tabs: "s"})
	for _, s := range skins {
		it := palItem{group: "Skin", label: s.label, desc: "the chrome; any theme fits", icon: "▣", run: "app:skin:" + s.id, tabs: "as"}
		if s == m.skin() {
			it.meta = "✓ current"
		}
		out = append(out, it)
	}
	for _, id := range themeOrder {
		t := themes[id]
		it := palItem{group: "Theme", label: id, desc: t.desc, icon: "■", iconFg: t.accent, iconBg: t.bg, run: "theme:" + id, tabs: "as"}
		if t.name == m.st.th.name {
			it.meta = "✓ current"
		}
		out = append(out, it)
	}
	return out
}

func (m *Model) palItems() []palItem {
	var out []palItem
	for _, s := range m.switchItems() {
		s := s
		it := palItem{sw: &s, label: strings.TrimSpace(s.label[2:]), tabs: "a"}
		switch {
		case strings.HasPrefix(s.label, "# "):
			it.group, it.icon, it.tabs = "Channels", m.icon(icChannel), "ac"
			if v, ok := m.chanView(s.ch); ok {
				if _, enc := m.e2e[v.ID]; enc || v.E2E {
					it.icon, it.iconTone, it.desc = m.icon(icPrivate), 'i', "encrypted"
				}
			}
		default:
			it.group, it.icon, it.tabs = "People", m.icon(icOnline), "ap"
			it.iconTone = 'f'
			if s.user != 0 {
				it.pres, it.nick = s.user, s.user
			} else {
				it.icon = "◇"
			}
			if s.ch == 0 {
				it.desc = "new direct message"
			}
		}
		switch {
		case s.mention:
			it.meta = "@" + unreadCount(max(s.unread, 1))
		case s.unread > 0:
			it.meta = unreadCount(s.unread) + " new"
		}
		out = append(out, it)
	}
	out = append(out, m.palCommands()...)
	out = append(out, m.palSettings()...)
	for i := range out {
		out[i].order = i
	}
	return out
}

func (m *Model) openPalette(tab string) {
	m.leaveSelect()
	m.mode = modeSwitcher
	m.fx.dialogAt = m.now()
	m.line.Reset()
	m.line.Prompt = ""
	m.line.Placeholder = "Search channels, people, commands…"
	m.line.Focus()
	m.pal = palState{items: m.palItems()}
	for i, t := range palTabs {
		if t.id == tab {
			m.pal.tab = i
		}
	}
	m.sw = switcher{items: m.switchItems()}
	m.filterPalette()
}

// openSwitcher is Option-K: the palette on its All tab.
func (m *Model) openSwitcher() { m.openPalette("all") }

func (m *Model) filterPalette() {
	q := m.line.Value()
	m.sw.filter(q)
	tab := palTabs[m.pal.tab].id[:1]
	if tab == "c" && palTabs[m.pal.tab].id == "commands" {
		tab = "m"
	}
	p := &m.pal
	p.shown = p.shown[:0]
	for _, it := range p.items {
		if !strings.Contains(it.tabs, tab) && !(tab == "a" && q != "") {
			continue // a search on All looks through everything
		}
		if q != "" {
			sc, ok := fuzzyScore(q, it.label+" "+it.desc)
			if it.sw != nil {
				sc, ok = fuzzyScore(q, it.sw.match)
			}
			if !ok {
				continue
			}
			if strings.HasPrefix(q, "/") && (it.label == q || strings.HasPrefix(it.label, q+" ")) {
				sc += 1 << 20 // the command typed in full comes first
			}
			it.score = sc
		}
		p.shown = append(p.shown, it)
	}
	if q != "" {
		// Jump targets keep the switcher's ranking and come first.
		rank := make(map[int]int, len(m.sw.shown))
		for i, s := range m.sw.shown {
			rank[s.order] = i
		}
		slices.SortStableFunc(p.shown, func(a, b palItem) int {
			if (a.sw != nil) != (b.sw != nil) {
				if a.sw != nil {
					return -1
				}
				return 1
			}
			if a.sw != nil {
				return rank[a.sw.order] - rank[b.sw.order]
			}
			return b.score - a.score
		})
	} else {
		// Unread and mentioned jump targets first, as the switcher did.
		slices.SortStableFunc(p.shown, func(a, b palItem) int {
			ua := a.sw != nil && (a.sw.unread > 0 || a.sw.mention)
			ub := b.sw != nil && (b.sw.unread > 0 || b.sw.mention)
			if ua != ub {
				if ua {
					return -1
				}
				return 1
			}
			return a.order - b.order
		})
	}
	p.sel = 0
}

func (m *Model) switcherKey(k tea.KeyMsg) tea.Cmd {
	p := &m.pal
	switch keyName(k) {
	case kEsc, kSwitcher, "ctrl+k":
		m.mode = modeNormal
		return nil
	case kUp, "ctrl+p":
		if p.sel > 0 {
			p.sel--
		}
		return nil
	case kDown, "ctrl+n":
		if p.sel < len(p.shown)-1 {
			p.sel++
		}
		return nil
	case kTab:
		p.tab = (p.tab + 1) % len(palTabs)
		m.filterPalette()
		return nil
	case kShiftTab:
		p.tab = (p.tab + len(palTabs) - 1) % len(palTabs)
		m.filterPalette()
		return nil
	case kEnter:
		if p.sel >= len(p.shown) {
			m.mode = modeNormal
			return nil
		}
		return m.palRun(p.shown[p.sel])
	}
	before := m.line.Value()
	var cmd tea.Cmd
	m.line, cmd = m.line.Update(k)
	if m.line.Value() != before {
		m.filterPalette()
	}
	return cmd
}

// palRun does what a row says.
func (m *Model) palRun(it palItem) tea.Cmd {
	if it.sw != nil {
		m.mode = modeNormal
		s := *it.sw
		if s.ch != 0 {
			return m.openChannel(s.ch)
		}
		if s.user == m.me.ID {
			return m.openDM(nil) // my notes
		}
		return m.openDM([]uint64{s.user})
	}
	run := it.run
	switch {
	case strings.HasPrefix(run, "theme:"):
		m.setTheme(strings.TrimPrefix(run, "theme:"))
		m.pal.items = m.palItems()
		sel := m.pal.sel
		m.filterPalette()
		m.pal.sel = min(sel, max(len(m.pal.shown)-1, 0))
		return nil
	case run == "app:themes":
		m.pal.tab = len(palTabs) - 1
		m.filterPalette()
		for i, s := range m.pal.shown {
			if s.group == "Theme" {
				m.pal.sel = i
				break
			}
		}
		return nil
	case strings.HasPrefix(run, "app:skin:"):
		m.setSkin(strings.TrimPrefix(run, "app:skin:"))
		m.pal.items = m.palItems()
		sel := m.pal.sel
		m.filterPalette()
		m.pal.sel = min(sel, max(len(m.pal.shown)-1, 0))
		return nil
	case run == "app:split":
		m.setSplit(!m.ui.Split)
		m.pal.items = m.palItems()
		sel := m.pal.sel
		m.filterPalette()
		m.pal.sel = sel
		return nil
	case run == "app:motion":
		m.setMotion(m.fx.off)
		m.pal.items = m.palItems()
		sel := m.pal.sel
		m.filterPalette()
		m.pal.sel = sel
		return nil
	}
	m.mode = modeNormal
	return m.appAction(run)
}

// appAction runs an action named by a palette row, a click or a key.
func (m *Model) appAction(run string) tea.Cmd {
	switch {
	case run == "app:agent":
		return m.openAgentPanel()
	case run == "app:approval":
		return m.jumpToApproval()
	case run == "app:settings":
		m.openSettings("")
	case strings.HasPrefix(run, "app:settings:"):
		m.openSettings(strings.TrimPrefix(run, "app:settings:"))
	case run == "app:keys":
		m.openHelp()
	case run == "app:sidebar":
		m.cycleSidebar()
	case run == "app:emoji":
		m.openPicker(false)
	case run == "app:closethread":
		m.closeThread()
		m.layoutInput()
	case strings.HasSuffix(run, " "):
		// Needs arguments: type it for the user to finish.
		m.input.SetValue(run)
		m.input.CursorEnd()
		m.layoutInput()
		m.refreshComp()
	case strings.HasPrefix(run, "/"):
		cmd, err := m.runCommand(run)
		if err != nil {
			m.setErr("", err)
			return nil
		}
		return cmd
	}
	return nil
}

func (m *Model) drawPalette(c *canvas) {
	th := m.st.th
	r := m.dialog(c, 76, 30, "Command palette")
	old := c.setClip(r)
	defer c.setClip(old)
	x0, y0 := r.x0, r.y0
	w, h := r.x1-r.x0, r.y1-r.y0
	in := 2
	if w < 40 {
		in = m.inside(r).x0 - r.x0 // clear of the frame, if any
	}
	iw := w - 2*in
	y := y0 + 3
	if h < 12 {
		y = y0 + 2
	}
	// search field
	c.fill(x0+in, y, iw, 1, th.element, "")
	c.put(x0+in+1, y, m.icon(icSearch), fgc(th.faint), "")
	c.putANSI(x0+in+3, y, dropBidi(m.line.View()), iw-4, fgc(th.fg).on(th.element), "")
	y += 2
	// tabs
	x := x0 + in + 1
	for i, t := range palTabs {
		on := i == m.pal.tab
		bg, st := th.panel, fgc(th.faint)
		if on {
			bg, st = th.element, fgb(th.fg).with(aMonoRev)
		}
		lab := " " + t.label + " "
		if x+strWidth(lab)+2 > x0+w-in {
			break
		}
		x = c.pill(x, y, lab, st.on(bg), "paltab:"+strconv.Itoa(i)) + 1
	}
	if w >= 76 {
		c.put(x0+w-14, y, "⇥ next tab", fgc(th.faint), "")
	}
	y += 2
	foot := y0 + h - 2
	if h < 12 && !m.skin().framed {
		foot = y0 + h - 1
	}
	rows := foot - 1 - y
	// lay out: group titles, then items
	type line struct {
		title string
		item  int
	}
	var lines []line
	last := ""
	for i, it := range m.pal.shown {
		if it.group != last {
			if last != "" && m.line.Value() == "" {
				lines = append(lines, line{item: -1})
			}
			lines = append(lines, line{title: it.group, item: -1})
			last = it.group
		}
		lines = append(lines, line{item: i})
	}
	if len(m.pal.shown) == 0 {
		lines = append(lines, line{title: "no matches", item: -1})
	}
	selLine := 0
	for i, l := range lines {
		if l.item == m.pal.sel {
			selLine = i
		}
	}
	start, end := window(len(lines), selLine, rows)
	for end > start && lines[end-1].item < 0 {
		end-- // never end on a group title with nothing under it
	}
	for i := start; i < end; i++ {
		l := lines[i]
		if l.item < 0 {
			if l.title != "" {
				c.put(x0+in+1, y, l.title, fgc(th.faint), "")
			}
			y++
			continue
		}
		m.palRow(c, x0+in, y, iw, l.item)
		y++
	}
	if foot > y0+4 {
		kx := x0 + in + 1
		for _, k := range [][2]string{{"↑↓", "move"}, {"⏎", "open"}, {"⇥", "tab"}, {"esc", "close"}} {
			if kx+strWidth(k[0])+strWidth(k[1])+4 > x0+w {
				break
			}
			kx = m.keycap(c, kx, foot, k[0])
			kx = c.put(kx+1, foot, k[1], fgc(th.faint), "") + 3
		}
	}
}

func (m *Model) palRow(c *canvas, x, y, w, i int) {
	th := m.st.th
	it := m.pal.shown[i]
	act := "pal:" + strconv.Itoa(i)
	old := c.setClip(rect{x, y, x + w, y + 1})
	defer c.setClip(old)
	on := i == m.pal.sel
	bg := th.panel
	switch {
	case on:
		bg = th.accent
	case m.hover == act:
		bg = th.element // hover is a hint; Enter acts on the selected row
	}
	c.fill(x, y, w, 1, bg, act)
	pick := func(normal rgb) cstyle {
		if on {
			return fgc(th.accentFg).on(bg)
		}
		return fgc(normal)
	}
	e := x + 1
	if it.icon != "" {
		ic := it.iconFg
		switch {
		case it.pres != 0:
			ic = m.presenceColor(it.pres)
		case it.iconTone == 'i':
			ic = th.info
		case it.iconTone == 'f':
			ic = th.faint
		case ic == 0:
			ic = th.dim
		}
		st := pick(ic)
		if it.iconBg != 0 {
			st = fgc(ic).on(it.iconBg)
		}
		c.put(e, y, it.icon, st, act)
		e += 3
	}
	fg := th.fg
	if it.nick != 0 {
		fg = th.nick(it.nick)
	}
	metaW := strWidth(it.meta)
	room := w - (e - x) - metaW - 3
	lab := truncate(it.label, room)
	st := pick(fg)
	if on {
		st.attr |= aBold | aMonoRev
	}
	e = c.put(e, y, lab, st, act)
	if it.desc != "" && room-strWidth(lab) > 6 {
		c.put(e+2, y, truncate(it.desc, room-strWidth(lab)-2), pick(th.faint), act)
	}
	if it.meta != "" {
		c.put(x+w-1-metaW, y, it.meta, pick(th.faint), act)
	}
}
