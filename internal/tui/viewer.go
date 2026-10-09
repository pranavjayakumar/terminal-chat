package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/sanitize"
)

// viewer shows one message full-screen without wrapping, so long code and
// log lines can be read by scrolling sideways instead of being cut with "…".
type viewer struct {
	title string
	lines []string
	x, y  int
}

const viewerStep = 8

func (m *Model) openViewer(msg client.Message) {
	body := sanitize.Text(msg.Body)
	if msg.Deleted {
		body = "message deleted"
	}
	when := time.UnixMilli(msg.Time).Format("Mon 2 Jan 15:04")
	m.view = viewer{
		title: sanitize.Line(m.r.userName(msg.Author)) + " · " + when,
		lines: strings.Split(body, "\n"),
	}
	m.mode = modeViewer
}

func (m *Model) viewerKey(key string) {
	v := &m.view
	page := max(m.dims().bodyH-2, 1)
	switch key {
	case kEsc, "q", kEnter, kSelView:
		m.mode = modeSelect
		return
	case kUp, "k":
		v.y--
	case kDown, "j":
		v.y++
	case kPgUp:
		v.y -= page
	case kPgDown, " ":
		v.y += page
	case "left", "h":
		v.x -= viewerStep
	case "right", "l":
		v.x += viewerStep
	case kHome, "g":
		v.x, v.y = 0, 0
	case kEnd, "G":
		v.y = len(v.lines)
	}
	widest := 0
	for _, l := range v.lines {
		widest = max(widest, ansi.StringWidth(l))
	}
	d := m.dims()
	v.x = min(max(v.x, 0), max(widest-d.mainW+1, 0))
	v.y = min(max(v.y, 0), max(len(v.lines)-(d.bodyH-1), 0))
}

func (m *Model) viewerLines(w, h int) []string {
	v := m.view
	hint := "←/→ ↑/↓ scroll · y copy · Esc closes"
	if w < 50 {
		hint = "Esc closes"
	}
	out := []string{fit(m.st.overlayTitle.Render(truncate(v.title+" — "+hint, w)), w)}
	rows := h - 1
	end := min(v.y+rows, len(v.lines))
	for _, l := range v.lines[min(v.y, end):end] {
		out = append(out, fit(ansi.Cut(l, v.x, v.x+w), w))
	}
	return padLines(out, w, h)
}
