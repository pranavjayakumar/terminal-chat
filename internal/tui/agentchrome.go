package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Agents in the chrome: their sidebar rows, the agent DM's header, the
// palette's Agent row.

// drawAgentRow draws `└ <icon> Name` under its owner: dim with 💤 while
// asleep, its glyph moving with what it does. Mine opens our DM (and, from
// there, the panel); someone else's puts @handle in the input.
func (m *Model) drawAgentRow(c *canvas, a proto.Agent, y, sb int, mini bool) {
	th := m.st.th
	act := "agent:" + strconv.FormatUint(a.Owner, 10)
	mine := a.Owner == m.me.ID
	on := mine && m.ag.dm != 0 && m.active == m.ag.dm
	bg := th.panel
	if on {
		bg = th.element
	}
	c.fill(1, y, sb-2, 1, bg, act)
	c.tag(1, y, sb-2, 1, act, !on, true)
	if on {
		c.put(0, y, "▎", fgc(th.accent), act)
	}
	state, since := m.agentStateOf(a)
	glyph, gcol := m.agentGlyph(a, state, since, bg)
	if mini {
		c.put(3, y, glyph, fgc(gcol), act)
		return
	}
	x := c.put(3, y, "└", fgc(th.faint), act)
	gx := x + 1
	x = c.put(gx, y, glyph, fgc(gcol), act+":icon")
	if w := strWidth(glyph); w < 2 {
		x++ // names line up whatever the glyph's width
	}
	nameSt := fgc(m.agentColor(a))
	if state == proto.AgentAsleep {
		nameSt = fgc(th.faint)
	}
	var badge string
	var badgeFg rgb
	if mine {
		if _, _, waiting := m.waitingApproval(); waiting {
			badge, badgeFg = "✋", th.warn
		} else if v, ok := m.chanView(m.ag.dm); ok && v.Unread > 0 && !on {
			badge, badgeFg = unreadCount(v.Unread), th.accent
		}
	}
	room := sb - 2 - (x + 1)
	if badge != "" {
		room -= strWidth(badge) + 1
	}
	name := truncate(agentName(a.Handle), max(room, 1))
	if on {
		nameSt = nameSt.with(aBold | aMonoRev)
	}
	x = c.put(x+1, y, name, nameSt, act)
	if state == proto.AgentAsleep && room-strWidth(name) >= 3 {
		c.put(x+1, y, "💤", fgc(th.faint), act)
	}
	if badge != "" {
		c.put(sb-2-strWidth(badge), y, badge, fgb(badgeFg), act)
	}
}

// agentRowClick is a click on an agent's sidebar row.
func (m *Model) agentRowClick(rest string) tea.Cmd {
	ownerS, part, _ := strings.Cut(rest, ":")
	owner, err := strconv.ParseUint(ownerS, 10, 64)
	if err != nil {
		return nil
	}
	a, ok := m.ag.byOwner[owner]
	if !ok {
		return nil
	}
	m.leaveSelect()
	if owner != m.me.ID {
		m.insertMention(a.Handle) // someone else's agent: call it
		return nil
	}
	switch {
	case part == "icon" || (m.ag.dm != 0 && m.active == m.ag.dm):
		return m.openAgentPanel()
	}
	if _, _, waiting := m.waitingApproval(); waiting {
		return m.openApproval()
	}
	return m.openAgentDM()
}

// insertMention adds "@handle " to the end of the composer.
func (m *Model) insertMention(handle string) {
	v := m.input.Value()
	if v != "" && !strings.HasSuffix(v, " ") {
		v += " "
	}
	m.input.SetValue(v + "@" + sanitize.Line(handle) + " ")
	m.input.CursorEnd()
	m.layoutInput()
}

// agentDMRight is the agent DM header's right side: an approval waiting
// elsewhere, or the mode (both open something).
func (m *Model) agentDMRight() struct{ t, act string } {
	if p, _, ok := m.waitingApproval(); ok {
		return struct{ t, act string }{"✋ waiting in " + m.chanTitle(p.ch), "agentappr:"}
	}
	mode := "Talk"
	if m.ag.home.ok {
		switch m.ag.home.cfg.Mode {
		case "work":
			mode = "Work"
		case "crew":
			mode = "Crew"
		}
		if m.ag.home.cfg.Paused {
			mode = "Paused"
			return struct{ t, act string }{mode, "app:agent"}
		}
	}
	return struct{ t, act string }{mode + " mode", "app:agent"}
}

// agentPalItem is the palette's Agent row.
func (m *Model) agentPalItem() palItem {
	it := palItem{group: "Commands", label: "Agent", desc: "your agent: mode, spend, memory", icon: m.icon(icAgent), iconTone: 'i', run: "app:agent", tabs: "ams"}
	if a, ok := m.myAgent(); ok {
		it.icon = agentFrame(parseAgentIcon(a.Icon), "", 0, false)
		it.desc = agentName(a.Handle) + ": mode, spend, memory"
	} else {
		it.desc = "set up your agent"
	}
	return it
}

// drawAgentHeaderGlyph puts my agent's moving glyph in the room the agent
// DM's header leaves before its name (headerLeft).
func (m *Model) drawAgentHeaderGlyph(c *canvas, g geo) {
	a, ok := m.myAgent()
	if !ok || !m.agentHeaderRoom() || g.thrFull {
		return
	}
	st, since := m.agentStateOf(a)
	glyph, col := m.agentGlyph(a, st, since, m.st.th.bg)
	c.put(g.CX, g.top, glyph, fgc(col), "app:agent")
}

// agentHeaderRoom: the header starts with room for the agent's glyph.
func (m *Model) agentHeaderRoom() bool {
	_, ok := m.myAgent()
	return ok && m.isAgentDM(m.active) && (m.sidebarShown() || m.status.State == client.Online)
}
