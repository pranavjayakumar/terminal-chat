package tui

import (
	"github.com/skailama/tc/internal/sanitize"
)

// listState is a read-only overlay of plain rows (/groups, /reminders).
// Rows are sanitized when drawn, so callers may pass raw text.
type listState struct {
	title string
	rows  []string
	sel   int
}

func (m *Model) listKey(key string) {
	s := &m.list
	switch key {
	case kEsc, "q", kEnter:
		m.mode = modeNormal
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
}

func (m *Model) listLines(w, h int) []string {
	s := &m.list
	out := []string{fit(m.st.overlayTitle.Render(truncate(sanitize.Line(s.title)+" — Esc closes", w)), w)}
	start, end := window(len(s.rows), s.sel, h-1)
	for i := start; i < end; i++ {
		l := truncate(" "+sanitize.Line(s.rows[i]), w)
		if i == s.sel {
			out = append(out, fit(m.st.overlaySel.Render(fit(l, w)), w))
		} else {
			out = append(out, fit(l, w))
		}
	}
	return padLines(out, w, h)
}
