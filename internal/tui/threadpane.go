package tui

import tea "github.com/charmbracelet/bubbletea"

// drawThreadGrip draws the thread pane's draggable left edge.
func (m *Model) drawThreadGrip(c *canvas, g geo) {
	th := m.st.th
	if !m.thread.open() {
		return
	}
	hot := m.hover == "thrresize" || m.drag == dragThread
	midY := (g.T0 + g.statusY) / 2
	for j := g.T0; j < g.statusY; j++ {
		mid := j >= midY-2 && j <= midY+2
		glyph, col := " ", th.panel
		switch {
		case hot && mid:
			glyph, col = "┃", th.accent
		case hot:
			glyph, col = "│", mix(th.panel, th.accent, 0.35)
		case mid:
			glyph, col = "│", mix(th.panel, th.line, 0.8)
		}
		c.put(g.RX, j, glyph, fgc(col), "thrresize")
	}
}

// threadKey is ⌥[ / ⌥] while a split thread is open; it reports whether
// it used the key. The macOS Option characters “ ‘ count only on an empty
// input, so a reply can still hold curly quotes.
func (m *Model) threadKey(k tea.KeyMsg, key string) bool {
	if key != kThrWider && key != kThrNarrow || !m.thread.open() || m.mode != modeNormal && m.mode != modeSelect {
		return false
	}
	g := m.geo()
	if !g.split || !k.Alt && m.input.Value() != "" {
		return false
	}
	d := threadStep
	if key == kThrNarrow {
		d = -d
	}
	if !m.resizeThread(g.TW+d, true) {
		m.setInfo("thread pane: " + map[bool]string{true: "widest", false: "narrowest"}[d > 0] + " here")
	}
	return true
}

// resizeThread sets the thread pane's width, kept within its limits; it
// reports whether the width changed.
func (m *Model) resizeThread(w int, save bool) bool {
	g := m.geo()
	if !g.split {
		return false
	}
	tl := 0
	if sw := sbWidth(g.sb); sw > 0 {
		tl = sw + 1
	}
	w = max(min(w, m.w-tl-besideThread), minThreadW)
	if w == g.TW {
		return false
	}
	m.ui.ThreadW = w
	if w == threadW {
		m.ui.ThreadW = 0
	}
	if save {
		m.savePrefs()
	}
	m.thread.ver++
	m.main.ver++
	m.layoutInput()
	m.clampScroll(&m.main)
	m.clampScroll(&m.thread)
	return true
}
