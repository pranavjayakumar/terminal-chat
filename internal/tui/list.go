package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/sanitize"
)

// listState is a read-only overlay of plain rows (/groups, /reminders).
// Rows are sanitized when drawn, so callers may pass raw text.
type listState struct {
	title string
	rows  []string
	sel   int
	urls  []string // a list of links (/links, l): Enter opens the row's, y copies it
	back  mode     // where closing returns (modeNormal, or modeSelect for l)
}

// closeList leaves the list for the mode it came from.
func (m *Model) closeList() {
	m.mode = m.list.back
	if m.mode == modeSelect {
		if _, ok := m.selected(); !ok {
			m.leaveSelect()
		}
		m.updatePlaceholder()
	}
}

func (m *Model) listKey(key string) tea.Cmd {
	s := &m.list
	switch key {
	case kEnter:
		m.closeList()
		if s.sel < len(s.urls) {
			return m.openLink(s.urls[s.sel])
		}
	case kSelCopy:
		if s.sel < len(s.urls) {
			return m.copyCmd(s.urls[s.sel])
		}
	case kEsc, "q":
		m.closeList()
	case kUp, "ctrl+p":
		s.sel = max(s.sel-1, 0)
	case kDown, "ctrl+n":
		s.sel = min(s.sel+1, max(len(s.rows)-1, 0))
	case kPgUp:
		s.sel = max(s.sel-10, 0)
	case kPgDown:
		s.sel = min(s.sel+10, max(len(s.rows)-1, 0))
	case kHome:
		s.sel = 0
	case kEnd:
		s.sel = max(len(s.rows)-1, 0)
	}
	return nil
}

func (m *Model) listLines(w, h int) []string {
	s := &m.list
	out := []string{fit(m.st.overlayTitle.Render(truncate(sanitize.Line(s.title)+" — Esc closes", w)), w)}
	rows := h - 1
	cut := len(s.rows) > rows && rows > 1
	if cut {
		rows-- // a last row says what is hidden, as help's "↓ more" does
	}
	start, end := window(len(s.rows), s.sel, rows)
	for i := start; i < end; i++ {
		l := truncate(" "+sanitize.Line(s.rows[i]), w)
		if i == s.sel {
			out = append(out, fit(m.st.overlaySel.Render(fit(l, w)), w))
		} else {
			out = append(out, fit(l, w))
		}
	}
	if cut {
		var more []string
		if start > 0 {
			more = append(more, fmt.Sprintf("↑ %d", start))
		}
		if end < len(s.rows) {
			more = append(more, fmt.Sprintf("↓ %d", len(s.rows)-end))
		}
		out = append(out, fit(m.st.dim.Render(truncate(" "+strings.Join(more, " · ")+" more", w)), w))
	}
	return padLines(out, w, h)
}
