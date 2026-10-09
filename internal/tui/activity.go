package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// activityLimit is how many items the overlay lists.
const activityLimit = 100

// maxJumpLimit caps how many top-level messages a jump will load to reach
// an old message.
const maxJumpLimit = 2000

type activityState struct {
	items []client.ActivityItem
	sel   int
	// seen is the newest item time that had been shown before the overlay
	// was opened; later items get a "new" dot. 0: it was never opened.
	seen  int64
	saved int64 // the last time written to the cache

	// Refreshes while open are rate-limited: last is when the list was
	// last read, pending says an update came in since, timer that a tick
	// is already scheduled.
	last    time.Time
	pending bool
	timer   bool
}

// activityEvery is the shortest time between two reads of the list while
// the overlay is open.
const activityEvery = time.Second

type activityRefreshMsg struct{}

// activityID identifies an item across refreshes.
type activityID struct {
	kind  client.ActivityKind
	msg   uint64
	actor uint64
	emoji string
}

func idOf(it client.ActivityItem) activityID {
	return activityID{it.Kind, it.Msg, it.Author, it.Emoji}
}

// openActivity shows the overlay (Alt-A, /activity). The list comes from
// the local cache.
//
// The selection survives closing and reopening while the list is the same,
// so Alt-A after a jump comes back to where Enter was pressed.
func (m *Model) openActivity() {
	m.cancelEdit()
	m.leaveSelect()
	m.mode = modeActivity
	a := &m.act
	items := m.c.Activity(activityLimit)
	if !slices.Equal(items, a.items) {
		a.sel = 0
	}
	a.items = items
	a.last, a.pending = m.now(), false
	a.sel = min(max(a.sel, 0), max(len(items)-1, 0))
	a.seen = m.c.ActivitySeen()
	m.markActivitySeen()
}

// markActivitySeen persists the time of the newest item on screen.
func (m *Model) markActivitySeen() {
	var newest int64
	for _, it := range m.act.items {
		newest = max(newest, it.Time)
	}
	if newest > m.act.saved {
		if err := m.c.SetActivitySeen(newest); err != nil {
			m.setErr("", err)
			return
		}
		m.act.saved = newest
	}
}

// refreshActivity re-reads the list while the overlay is open, keeping the
// selection on the same item when it is still there.
func (m *Model) refreshActivity() {
	a := &m.act
	var keep activityID
	had := a.sel >= 0 && a.sel < len(a.items)
	if had {
		keep = idOf(a.items[a.sel])
	}
	a.items = m.c.Activity(activityLimit)
	a.last, a.pending = m.now(), false
	m.markActivitySeen()
	if had {
		for i, it := range a.items {
			if idOf(it) == keep {
				a.sel = i
				return
			}
		}
	}
	a.sel = min(max(a.sel, 0), max(len(a.items)-1, 0))
}

// activityAffected says whether u can change the list: new or changed
// messages in a channel I'm in, or a change to the channels themselves.
// Users and presence updates never do.
func (m *Model) activityAffected(u client.Update) bool {
	if u.Channels {
		return true
	}
	for ch := range u.Msgs {
		if v, ok := m.chanView(ch); ok && v.Member {
			return true
		}
	}
	return false
}

// queueActivityRefresh refreshes the open overlay at once unless it was
// read less than activityEvery ago; then one refresh is scheduled for when
// that time is up, however many updates arrive meanwhile.
func (m *Model) queueActivityRefresh() tea.Cmd {
	a := &m.act
	wait := activityEvery - m.now().Sub(a.last)
	if wait <= 0 {
		m.refreshActivity()
		return nil
	}
	a.pending = true
	if a.timer {
		return nil
	}
	a.timer = true
	return tea.Tick(wait, func(time.Time) tea.Msg { return activityRefreshMsg{} })
}

// activityWheel moves the selection like the wheel scrolls the log: three
// rows a notch.
func (m *Model) activityWheel(b tea.MouseButton) {
	a := &m.act
	switch b {
	case tea.MouseButtonWheelUp:
		a.sel -= 3
	case tea.MouseButtonWheelDown:
		a.sel += 3
	default:
		return
	}
	a.sel = min(max(a.sel, 0), max(len(a.items)-1, 0))
}

func (m *Model) activityKey(k tea.KeyMsg) tea.Cmd {
	a := &m.act
	page := max(m.dims().bodyH-2, 1)
	switch keyName(k) {
	case kEsc, kActivity, "q":
		m.mode = modeNormal
	case kUp, "ctrl+p", "k":
		a.sel--
	case kDown, "ctrl+n", "j":
		a.sel++
	case kPgUp:
		a.sel -= page
	case kPgDown:
		a.sel += page
	case kHome, "g":
		a.sel = 0
	case kEnd, "G":
		a.sel = len(a.items) - 1
	case kEnter:
		m.mode = modeNormal
		if a.sel < 0 || a.sel >= len(a.items) {
			return nil
		}
		it := a.items[a.sel]
		return m.jumpTo(proto.Message{ID: it.Msg, Chan: it.Chan, Thread: it.Thread})
	}
	a.sel = min(max(a.sel, 0), max(len(a.items)-1, 0))
	return nil
}

func (m *Model) activityLines(w, h int) []string {
	a := &m.act
	n := len(a.items)
	title := fmt.Sprintf("activity — %d %s · ↑/↓ move, Enter jumps, Esc closes", n, plural(n, "item", "items"))
	if w < wideRows {
		title = fmt.Sprintf("activity %d · Enter · Esc", n)
	}
	out := []string{fit(m.st.overlayTitle.Render(truncate(title, w)), w)}
	if n == 0 {
		out = append(out, fit(m.st.dim.Render(truncate(" nothing yet — mentions, replies, reactions and DMs show up here", w)), w))
	}
	start, end := window(n, a.sel, h-1)
	for i := start; i < end; i++ {
		out = append(out, m.activityRow(a.items[i], w, i == a.sel, a.seen > 0 && a.items[i].Time > a.seen))
	}
	return padLines(out, w, h)
}

// Row layout by width. The reason marker always comes first; narrow
// terminals lose the time, then the author, and shorten the channel name so
// the preview keeps the room.
const (
	wideRows   = 60 // below: no time, channel name cut to narrowChan
	mediumRows = 48 // below: no author either
	narrowChan = 10
)

// activityRow is "marker time · #chan · author · preview", one line of
// exactly w cells. Everything from the cache goes through sanitize.
func (m *Model) activityRow(it client.ActivityItem, w int, selected, fresh bool) string {
	where := m.chanTitle(it.Chan)
	if w < wideRows {
		where = truncate(where, narrowChan)
	}
	type part struct{ plain, styled string }
	parts := []part{}
	if w >= wideRows {
		when := fmtWhen(it.Time)
		parts = append(parts, part{when, m.st.time.Render(when)})
	}
	parts = append(parts, part{where, where})
	if w >= mediumRows {
		who := m.r.userName(it.Author)
		parts = append(parts, part{who, m.st.author(it.Author).Render(who)})
	}
	text := activityText(it)
	parts = append(parts, part{text, text})

	// The first cell is a dim dot for items newer than the last time the
	// overlay was open.
	dot, styledDot := " ", " "
	if fresh {
		dot, styledDot = "•", m.st.dim.Render("•")
	}
	head := dot + activityMarker(it) + " "
	if selected {
		var plain []string
		for _, p := range parts {
			plain = append(plain, p.plain)
		}
		l := head + strings.Join(plain, " · ")
		return fit(m.st.overlaySel.Render(fit(truncate(l, w), w)), w)
	}
	sep := m.st.dim.Render(" · ")
	var styled []string
	for _, p := range parts {
		styled = append(styled, p.styled)
	}
	return fit(truncate(styledDot+activityMarker(it)+" "+strings.Join(styled, sep), w), w)
}

// activityMarker is the two-cell reason for an item: @ mention, * highlight
// word, ↳ reply, » direct message, the emoji for a reaction.
func activityMarker(it client.ActivityItem) string {
	mark := "  "
	switch it.Kind {
	case client.ActMention:
		mark = "@ "
	case client.ActHighlight:
		mark = "* "
	case client.ActReply:
		mark = "↳ "
	case client.ActDM:
		mark = "» "
	case client.ActReaction:
		mark = truncate(strings.TrimSpace(sanitize.Line(it.Emoji)), 2)
		if strWidth(mark) == 0 {
			mark = "+"
		}
	}
	return fit(mark, 2)
}

// activityText is the item's one-line text.
func activityText(it client.ActivityItem) string {
	p := sanitize.Line(it.Preview)
	if it.Kind == client.ActReaction {
		return "on: " + p
	}
	return p
}
