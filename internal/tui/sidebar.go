package tui

import (
	"strconv"

	"github.com/rivo/uniseg"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// sideItem is one sidebar row: a section title (ch 0), a channel, or an
// agent under its owner (agent set).
type sideItem struct {
	ch      uint64
	agent   *proto.Agent
	section string
	icon    string
	iconFg  rgb
	label   string
	extra   string // after the label, faint ("away")
	badge   string
	badgeFg rgb
	unread  bool
	muted   bool
}

func (m *Model) sideItems() []sideItem {
	var out []sideItem
	section := func(name string, dm bool) {
		first := true
		for _, v := range m.sidebarOrder() {
			if (v.Kind == proto.ChanDM) != dm || v.Kind == proto.ChanAgentDM {
				continue // my agent's DM sits under me (drawAgentRow)
			}
			if first {
				if len(out) > 0 {
					out = append(out, sideItem{})
				}
				out = append(out, sideItem{section: name})
				first = false
			}
			out = append(out, m.sideEntry(v))
			if others := m.dmOthers(v); dm && len(others) == 1 {
				if a, ok := m.ag.byOwner[others[0]]; ok {
					out = append(out, sideItem{agent: &a})
				}
			}
		}
	}
	section("Channels", false)
	section("Direct", true)
	return out
}

func (m *Model) sideEntry(v client.ChannelView) sideItem {
	th := m.st.th
	it := sideItem{ch: v.ID, iconFg: th.faint, muted: m.muted(v.ID)}
	if v.Kind == proto.ChanDM {
		it.label = sanitize.Line(v.Display)
		others := m.dmOthers(v)
		switch {
		case len(others) == 0:
			it.icon = m.icon(icNotes)
		case len(others) == 1:
			it.icon = "○"
			switch m.presence[others[0]].State {
			case proto.PresOnline:
				it.icon, it.iconFg = m.icon(icOnline), th.ok
			case proto.PresAway:
				it.icon, it.iconFg, it.extra = m.icon(icAway), th.warn, "away"
			}
		default:
			it.icon = "◇"
		}
	} else {
		it.icon = m.icon(icChannel)
		if _, enc := m.e2e[v.ID]; enc || v.E2E {
			it.icon, it.iconFg = m.icon(icPrivate), th.info
		}
		it.label = sanitize.Line(v.Name)
	}
	it.unread = v.Mention || (v.Unread > 0 && !it.muted)
	switch {
	case v.Mention:
		it.badge, it.badgeFg = "@"+unreadCount(max(v.Unread, 1)), th.accent
		if t, ok := m.fx.pulse[v.ID]; ok && m.running(t, dPulse) {
			// toward the text color: white would vanish on a light theme
			it.badgeFg = mix(th.accent, th.fg, 0.5*absSin(m.progress(t, dPulse)*6))
		}
	case it.muted:
	case v.Unread > 0:
		it.badge, it.badgeFg = unreadCount(v.Unread), th.dim
		if v.Kind == proto.ChanDM {
			it.badgeFg = th.accent
		}
	}
	it.extra = m.huddleMarker(v.ID)
	if m.hasDraft(v.ID) {
		it.extra += " ✎"
	}
	return it
}

func (m *Model) drawSidebar(c *canvas, g geo) {
	th := m.st.th
	sb := g.SB
	mini := sb < 14
	c.fill(0, g.T0, sb, g.statusY-g.T0, th.panel, "")
	old := c.setClip(rect{0, g.T0, sb, g.statusY})
	y := g.top
	c.put(2, y, "tc", fgb(th.accent), "")
	if !mini {
		online := 0
		for _, u := range m.userList {
			if m.presence[u.ID].State == proto.PresOnline {
				online++
			}
		}
		t := "· " + strconv.Itoa(online) + " online"
		if m.status.State != client.Online || m.status.Stopped {
			t = "· " + m.statusText()
		}
		c.put(5, y, t, fgc(th.faint), "")
	}
	step := 1
	if g.roomy {
		step = 2
	}
	y += step + boolInt(g.roomy)
	c.fill(1, y, sb-2, 1, th.element, "palette:")
	c.tag(1, y, sb-2, 1, "palette:", true, true)
	if mini {
		c.put(sb/2-1, y, m.icon(icSearch), fgc(th.dim), "palette:")
	} else {
		jump := m.icon(icSearch) + " Jump to…"
		c.put(2, y, jump, fgc(th.dim), "palette:")
		if k := m.kn.of("⌥K"); sb-2-strWidth(k) >= 2+strWidth(jump)+1 {
			c.put(sb-2-strWidth(k), y, k, fgc(th.faint), "palette:")
		}
	}
	y += step
	meY := g.statusY - 1 - boolInt(g.roomy)
	items := m.sideItems()
	room := meY - 1 - y
	start := 0
	active := -1
	for i, it := range items {
		if it.ch == m.active && it.ch != 0 {
			active = i
		}
	}
	mine, haveMine := m.myAgent()
	agentY := -1
	if haveMine {
		// my agent's row goes under mine: the blank row of a roomy
		// layout, or mine moves up one
		if g.roomy {
			agentY = meY + 1
		} else {
			agentY = meY
			meY--
		}
		room = meY - 1 - y
	}
	if len(items) > room && active >= room {
		start = min(active-room/2, len(items)-room)
	}
	for i := start; i < len(items) && y < meY-1; i++ {
		m.drawSideItem(c, items[i], y, sb, mini)
		y++
	}
	if agentY > y {
		m.drawAgentRow(c, mine, agentY, sb, mini)
	}
	if meY > y {
		c.put(2, meY, m.icon(icOnline), fgc(m.presenceColor(m.me.ID)), "")
		if !mini {
			e := c.put(4, meY, truncate(sanitize.Line(m.me.Name), sb-14), fgb(th.fg), "status")
			state := m.presence[m.me.ID].State
			if state == "" {
				state = "offline"
			}
			c.put(e+1, meY, truncate(state, max(sb-4-e-2, 0)), fgc(th.faint), "status")
			c.put(sb-3, meY, m.icon(icSettings), fgc(th.dim), "settings:")
			c.tag(sb-3, meY, 1, 1, "settings:", true, true)
		}
	}
	c.setClip(old)
	// the draggable edge
	hot := m.hover == "sbresize"
	midY := (g.T0 + g.statusY) / 2
	for j := g.T0; j < g.statusY; j++ {
		mid := j >= midY-2 && j <= midY+2
		glyph, col := "│", th.line
		switch {
		case hot && mid:
			glyph, col = "┃", th.accent
		case hot:
			col = mix(th.bg, th.accent, 0.35)
		case mid:
			col = mix(th.line, th.dim, 0.5)
		}
		c.put(sb, j, glyph, fgc(col), "sbresize")
	}
}

func (m *Model) presenceColor(u uint64) rgb {
	switch m.presence[u].State {
	case proto.PresOnline:
		return m.st.th.ok
	case proto.PresAway:
		return m.st.th.warn
	}
	return m.st.th.faint
}

func (m *Model) drawSideItem(c *canvas, it sideItem, y, sb int, mini bool) {
	th := m.st.th
	if it.agent != nil {
		m.drawAgentRow(c, *it.agent, y, sb, mini)
		return
	}
	if it.ch == 0 {
		if it.section != "" && !mini {
			c.put(2, y, it.section, fgc(th.faint), "")
		}
		return
	}
	act := "chan:" + strconv.FormatUint(it.ch, 10)
	on := it.ch == m.active
	bg := th.panel
	if on {
		bg = th.element
	}
	c.fill(1, y, sb-2, 1, bg, act)
	c.tag(1, y, sb-2, 1, act, !on, true)
	if on {
		c.put(0, y, "▎", fgc(th.accent), act)
	}
	nameFg := th.dim
	st := cstyle{}
	switch {
	case on || it.unread:
		nameFg = th.fg
		if it.unread || on {
			st.attr = aBold
		}
	case it.muted:
		nameFg = th.faint
	}
	if on {
		st.attr |= aMonoRev
	}
	if mini {
		// a channel shows its first letter; everything else its icon
		g, fg := it.icon, it.iconFg
		if it.icon == "#" {
			g, _, _, _ = uniseg.FirstGraphemeClusterInString(it.label, -1)
			fg = nameFg
		}
		c.put(3, y, g, cstyle{fg: fg, hasFg: true, attr: st.attr}, act)
		if it.badge != "" {
			c.put(sb-2, y, "•", fgc(it.badgeFg), act)
		}
		return
	}
	x := c.put(2, y, it.icon, fgc(it.iconFg), act)
	bw := 0
	if it.badge != "" {
		bw = strWidth(it.badge) + 1
	}
	room := sb - 2 - (x + 1) - bw
	lab := truncate(it.label, room)
	x = c.put(x+1, y, lab, cstyle{fg: nameFg, hasFg: true, attr: st.attr}, act)
	if it.extra != "" && room-strWidth(lab) > 2 {
		c.put(x+1, y, truncate(it.extra, room-strWidth(lab)-1), fgc(th.faint), act)
	}
	if it.badge != "" {
		c.put(sb-2-strWidth(it.badge), y, it.badge, fgb(it.badgeFg), act)
	}
}

// drawTips shows the hovered icon-mode sidebar row's name.
func (m *Model) drawTips(c *canvas, g geo) {
	if g.SB == 0 || g.SB >= 14 || len(m.hover) < 5 || m.hover[:5] != "chan:" || m.mode != modeNormal {
		return
	}
	id, err := strconv.ParseUint(m.hover[5:], 10, 64)
	if err != nil {
		return
	}
	th := m.st.th
	for y := g.T0; y < g.statusY; y++ {
		if c.actionAt(1, y) != m.hover {
			continue
		}
		v, _ := m.chanView(id)
		it := m.sideEntry(v)
		t := it.icon + " " + it.label
		if it.badge != "" {
			t += "  " + it.badge
		}
		x := g.SB + 1
		t = truncate(t, max(m.w-x-5, 1))
		x = c.put(x, y, "▐", fgc(th.element), "")
		x = c.put(x, y, " "+t+" ", fgc(th.fg).on(th.element), "")
		x = c.put(x, y, "▌", fgc(th.element), "")
		c.fill(x, y, 1, 1, th.bg, "") // a gap before the text it floats over
		return
	}
}

// unreadCount is an unread badge's number, capped so it fits the column.
func unreadCount(n int) string {
	if n > 99 {
		return "99+"
	}
	return strconv.Itoa(n)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
