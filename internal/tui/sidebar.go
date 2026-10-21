package tui

import (
	"strconv"

	"github.com/rivo/uniseg"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// sideItem is one sidebar row: a section title (ch 0) or a channel.
type sideItem struct {
	ch      uint64
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
			if (v.Kind == proto.ChanDM) != dm {
				continue
			}
			if first {
				if len(out) > 0 {
					out = append(out, sideItem{})
				}
				out = append(out, sideItem{section: name})
				first = false
			}
			out = append(out, m.sideEntry(v))
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
			it.icon = "▪" // my notes
		case len(others) == 1:
			it.icon = "○"
			switch m.presence[others[0]].State {
			case proto.PresOnline:
				it.icon, it.iconFg = "●", th.ok
			case proto.PresAway:
				it.icon, it.iconFg, it.extra = "◐", th.warn, "away"
			}
		default:
			it.icon = "◇"
		}
	} else {
		it.icon = "#"
		if _, enc := m.e2e[v.ID]; enc || v.E2E {
			it.icon, it.iconFg = "◆", th.info
		}
		it.label = sanitize.Line(v.Name)
	}
	it.unread = v.Mention || (v.Unread > 0 && !it.muted)
	switch {
	case v.Mention:
		n := max(v.Unread, 1)
		it.badge, it.badgeFg = "@"+strconv.Itoa(n), th.accent
		if t, ok := m.fx.pulse[v.ID]; ok && m.running(t, dPulse) {
			it.badgeFg = mix(th.accent, 0xffffff, 0.5*absSin(m.progress(t, dPulse)*6))
		}
	case it.muted:
	case v.Unread > 99:
		it.badge, it.badgeFg = "99+", th.dim
	case v.Unread > 0:
		it.badge, it.badgeFg = strconv.Itoa(v.Unread), th.dim
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
	c.fill(0, 0, sb, g.statusY, th.panel, "")
	old := c.setClip(rect{0, 0, sb, g.statusY})
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
		c.put(sb/2-1, y, "⌕", fgc(th.dim), "palette:")
	} else {
		c.put(2, y, "⌕ Jump to…", fgc(th.dim), "palette:")
		c.put(sb-4, y, "⌃K", fgc(th.faint), "palette:")
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
	if len(items) > room && active >= room {
		start = min(active-room/2, len(items)-room)
	}
	for i := start; i < len(items) && y < meY-1; i++ {
		m.drawSideItem(c, items[i], y, sb, mini)
		y++
	}
	if meY > y {
		c.put(2, meY, "●", fgc(m.presenceColor(m.me.ID)), "")
		if !mini {
			e := c.put(4, meY, truncate(sanitize.Line(m.me.Name), sb-14), fgb(th.fg), "status")
			state := m.presence[m.me.ID].State
			if state == "" {
				state = "offline"
			}
			c.put(e+1, meY, truncate(state, max(sb-4-e-2, 0)), fgc(th.faint), "status")
			c.put(sb-3, meY, "⚙", fgc(th.dim), "settings:")
			c.tag(sb-3, meY, 1, 1, "settings:", true, true)
		}
	}
	c.setClip(old)
	// the draggable edge
	hot := m.hover == "sbresize"
	for j := 0; j < g.statusY; j++ {
		mid := j >= g.statusY/2-2 && j <= g.statusY/2+2
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
	for y := 0; y < g.statusY; y++ {
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
		x = c.put(x, y, "▐", fgc(th.element), "")
		x = c.put(x, y, " "+t+" ", fgc(th.fg).on(th.element), "")
		c.put(x, y, "▌", fgc(th.element), "")
		return
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
