package tui

import (
	"fmt"
	"strings"

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
}

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
func (m *Model) openActivity() {
	m.leaveSelect()
	m.mode = modeActivity
	m.act = activityState{items: m.c.Activity(activityLimit)}
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
	out := []string{fit(m.st.overlayTitle.Render(truncate(title, w)), w)}
	if n == 0 {
		out = append(out, fit(m.st.dim.Render(truncate(" nothing yet — mentions, replies, reactions and DMs show up here", w)), w))
	}
	start, end := window(n, a.sel, h-1)
	for i := start; i < end; i++ {
		out = append(out, m.activityRow(a.items[i], w, i == a.sel))
	}
	return padLines(out, w, h)
}

// activityRow is "time · #chan · author · preview", one line of exactly w
// cells. Everything from the cache goes through sanitize.
func (m *Model) activityRow(it client.ActivityItem, w int, selected bool) string {
	when := fmtWhen(it.Time)
	where := m.chanTitle(it.Chan)
	who := m.r.userName(it.Author)
	text := activityPreview(it)
	if selected {
		l := " " + when + " · " + where + " · " + who + " · " + text
		return fit(m.st.overlaySel.Render(fit(truncate(l, w), w)), w)
	}
	sep := m.st.dim.Render(" · ")
	l := " " + m.st.time.Render(when) + sep + where + sep + m.st.author(it.Author).Render(who) + sep + text
	return fit(truncate(l, w), w)
}

// activityPreview is the one-line text of an item with a marker for why it
// is there: @ mention, * highlight word, ↳ reply, the emoji for a reaction.
func activityPreview(it client.ActivityItem) string {
	p := sanitize.Line(it.Preview)
	switch it.Kind {
	case client.ActMention:
		return "@ " + p
	case client.ActHighlight:
		return "* " + p
	case client.ActReply:
		return "↳ " + p
	case client.ActReaction:
		e := strings.TrimSpace(sanitize.Line(it.Emoji))
		if e == "" {
			e = "reacted"
		}
		return e + " on: " + p
	}
	return p
}
