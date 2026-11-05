package tui

import (
	"errors"
	"fmt"
	"slices"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// Catching up: a channel with unread messages opens at a "new" divider above
// the first of them; the read marker moves only over what has been on screen.

// maxNewLimit caps how many messages an open loads from the cache to reach
// the first unread one.
const maxNewLimit = 500

// maxRecent is how many channels the palette lists as Recent.
const maxRecent = 5

// noteNew remembers p's read marker as it opens, when it has unread messages,
// and loads enough of the cache to reach the first of them.
func (m *Model) noteNew(p *pane) {
	v, ok := m.chanView(p.ch)
	if !ok || !v.Member || v.Unread == 0 {
		return
	}
	p.newAt, p.hasNew, p.place = v.Read, true, true
	if want := min(v.Unread+20, maxNewLimit); want > m.limit(p.ch) {
		m.limits[p.ch] = want
	}
}

// newIndex is the index of the message the "new" divider goes above, -1 none.
func (m *Model) newIndex(p *pane) int {
	if !p.hasNew || p.root != 0 {
		return -1
	}
	for i, x := range p.msgs {
		if x.ID > p.newAt && !x.Deleted && !m.ownMsg(x) {
			return i
		}
	}
	return -1
}

// settleNew scrolls a just-opened channel so its "new" divider is the top
// row, or leaves it at the bottom when everything new fits. It waits for a
// layout, and for a history fetch that may bring the first unread message.
func (m *Model) settleNew() {
	p := &m.main
	if !p.place || !p.open() {
		return
	}
	w, h := m.paneWidth(p), m.paneHeight(p)
	if w <= 0 || h <= 0 {
		if m.w > 0 && m.h > 0 {
			p.place = false // sized, but the pane is hidden (a full-width thread)
		}
		return
	}
	rows, _ := m.paneLines(p, w)
	if p.memoNewRow < 0 {
		if m.histBusy[p.ch] {
			return
		}
		p.place = false
		return
	}
	p.place = false
	p.scroll = min(max(len(rows)-h-p.memoNewRow, 0), max(len(rows)-h, 0))
}

// readUpTo is how far a scrolled-up reader of p has read: the newest message
// whose last row is on screen or above it, but never past an unread message
// (newer than read) that sits wholly above the screen, which a jump skipped
// and nobody has seen.
func (m *Model) readUpTo(p *pane, read uint64) uint64 {
	w, h := m.paneWidth(p), m.paneHeight(p)
	if w <= 0 || h <= 0 {
		return 0
	}
	rows, owner := m.paneLines(p, w)
	bottom := min(len(rows)-p.scroll, len(owner))
	top := max(len(rows)-h-p.scroll, 0)
	var id uint64
	for r := 0; r < bottom; r++ {
		o := owner[r]
		if o < 0 || o >= len(p.msgs) || (r+1 < len(owner) && owner[r+1] == o) {
			continue
		}
		x := p.msgs[o]
		if r < top && x.ID > read && !x.Deleted && !m.ownMsg(x) {
			return id
		}
		id = max(id, x.ID)
	}
	return id
}

// newAfterSync gives the open channel its divider for what a sync after
// connecting brought (tc opens on its cache, before the night's messages
// arrive), unless the reader has moved already. Read markers don't move
// while syncing, so the channel's marker is still the one from before.
func (m *Model) newAfterSync() {
	p := &m.main
	if !p.open() || p.hasNew || p.scroll != 0 || m.mode != modeNormal {
		return
	}
	limit := m.limit(p.ch)
	m.noteNew(p)
	if !p.hasNew {
		return
	}
	if m.limit(p.ch) != limit {
		m.reloadPane(p)
	}
	p.ver++
	m.settleNew()
}

// huddleHere is Alt-H: leave the open channel's huddle when we're in it,
// else join it (leaving one elsewhere, as /huddle does).
func (m *Model) huddleHere() tea.Cmd {
	m.leaveSelect()
	if m.hud.in && m.hud.cur == m.active {
		return m.runQuiet("/leave-huddle")
	}
	return m.runQuiet("/huddle")
}

// readAll is /read-all: every channel's read marker to its newest message.
func (m *Model) readAll() error {
	if m.status.State == client.Syncing {
		// As markRead: mid-sync read state is incomplete.
		return errors.New("still syncing — try /read-all again in a moment")
	}
	n := 0
	for i := range m.chans {
		v := &m.chans[i]
		if !v.Member || (v.Unread == 0 && !v.Mention) {
			continue
		}
		last := v.Last
		if v.ID == m.main.ch {
			last = max(last, lastID(m.main.msgs))
		}
		if last > v.Read {
			if err := m.c.MarkRead(v.ID, last); err != nil {
				return fmt.Errorf("mark read: %w", err)
			}
			v.Read = last
		}
		if v.ID == m.main.ch && m.main.hasNew {
			m.main.hasNew, m.main.place = false, false // caught up: the divider goes
			m.main.ver++
		}
		v.Unread, v.Mention = 0, false
		m.al.Seen(v.ID) // nothing pending for it pops up afterwards
		n++
	}
	if n == 0 {
		m.setInfo("nothing unread")
		return nil
	}
	m.setInfo(fmt.Sprintf("marked %d %s read", n, plural(n, "channel", "channels")))
	return nil
}

// noteVisit records leaving ch for another channel: the previous channel
// key goes back to it, and the palette lists it under Recent.
func (m *Model) noteVisit(ch uint64) {
	m.prevCh = ch
	m.recent = slices.DeleteFunc(m.recent, func(id uint64) bool { return id == ch })
	m.recent = slices.Insert(m.recent, 0, ch)
	if len(m.recent) > maxRecent+1 { // +1: the open channel is skipped
		m.recent = m.recent[:maxRecent+1]
	}
}

// backChannel opens the channel open before this one (Ctrl-6, like vim's
// alternate file), so two channels can be flipped between.
func (m *Model) backChannel() tea.Cmd {
	ch := m.prevCh
	if ch == 0 || ch == m.active {
		m.setInfo("no previous channel yet")
		return nil
	}
	if v, ok := m.chanView(ch); !ok || (!v.Member && !v.Removed && v.Kind != proto.ChanPublic) {
		m.prevCh = 0
		m.setInfo("the previous channel is gone")
		return nil
	}
	return m.openChannel(ch)
}
