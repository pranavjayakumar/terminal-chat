package tui

import "math"

// Layout constants (the V4 design at 160×48).
const (
	sbWNormal    = 24
	sbWWide      = 32
	sbWIcons     = 7
	threadW      = 44
	maxColumn    = 86
	sbHideBelow  = 60 // columns: no sidebar at all
	sbIconsBelow = 90 // columns: the sidebar shrinks to icons
)

// geo is where everything goes for the current size and state.
type geo struct {
	W, H           int
	SB             int // sidebar width, 0 when hidden; its edge is column SB
	sb             string
	L              int // first content column
	RX             int // thread pane's first column (W when none)
	tpW            int
	split, thrFull bool

	CX, CW int // the message column

	top            int // header row
	hud            int // huddle bar row, -1 none
	banner         int // key-change banner row, -1 none
	msgTop, msgBot int // message rows [msgTop, msgBot)
	noteY          int // typing / notices / toasts
	inY, inH       int // input box top row and height (text rows + 2)
	inX, inW       int // input box column and width
	statusY        int
	roomy          bool
	boxed          bool // the input has its frame (not on tiny terminals)
}

func sbWidth(mode string) int {
	switch mode {
	case sbWide:
		return sbWWide
	case sbIcons:
		return sbWIcons
	case sbNormal:
		return sbWNormal
	}
	return 0
}

// sbMode is the sidebar size shown now: the saved choice, shrunk to icons
// on narrow terminals and hidden on very narrow ones.
func (m *Model) sbMode() string {
	switch {
	case m.mode == modeViewer && m.view.wide:
		return "" // text to copy: a mouse selection mustn't pick up the sidebar
	case m.sbForce != "" && m.w < sbIconsBelow && m.w >= minSidebarCols:
		if m.sbForce == sbHidden {
			return ""
		}
		return m.sbForce
	case m.w < sbHideBelow:
		return ""
	case m.w < sbIconsBelow:
		return sbIcons
	}
	return m.ui.sidebarMode()
}

func (m *Model) sidebarShown() bool { return m.sbMode() != "" }

// sbW is the sidebar's drawn width, mid-animation included.
func (m *Model) sbW() int {
	to := sbWidth(m.sbMode())
	if to == 0 || !m.running(m.fx.sbAt, dSlide) || m.fx.sbTo != to {
		return to
	}
	p := ease(m.progress(m.fx.sbAt, dSlide))
	return int(math.Round(float64(m.fx.sbFrom) + float64(to-m.fx.sbFrom)*p))
}

func (m *Model) geo() geo {
	g := geo{W: m.w, H: m.h, hud: -1, banner: -1}
	if m.w <= 0 || m.h <= 0 {
		return g
	}
	g.sb = m.sbMode()
	g.SB = m.sbW()
	if g.SB > 0 {
		g.L = g.SB + 1
	}
	g.RX = m.w
	region := m.w - g.L
	if m.thread.open() || m.running(m.fx.paneAt, dPane) {
		if region >= threadW+50 {
			g.split = true
			k := ease(m.progress(m.fx.paneAt, dPane))
			if !m.thread.open() {
				k = 1 - k
			}
			g.tpW = int(math.Round(threadW * k))
			g.RX = m.w - g.tpW
		} else if m.thread.open() {
			g.thrFull = true
		}
	}
	// The column's width follows the final sidebar and thread pane sizes,
	// so their slides move the column instead of re-wrapping every message.
	tl := 0
	if w := sbWidth(g.sb); w > 0 {
		tl = w + 1
	}
	finalRX := m.w
	if g.split && m.thread.open() {
		finalRX = m.w - threadW
	}
	region = finalRX - tl
	switch {
	case region >= 70:
		g.CW = min(maxColumn, region-8)
	case region >= 30:
		g.CW = region - 2
	default:
		g.CW = max(region, 1)
	}
	g.CX = min(g.L+(g.RX-g.L-g.CW)/2, g.RX-g.CW)
	g.CX = max(g.CX, 0)

	g.roomy = m.h >= 30
	g.statusY = m.h - 1
	gap := 0
	if g.roomy {
		gap = 1
	}
	g.inX, g.inW = g.CX, g.CW
	if g.split && m.thread.open() {
		// final width, sliding in with the pane (the screen edge clips it)
		g.inX, g.inW = g.RX+2, threadW-4
	}
	g.boxed = m.h >= 10 && g.inW >= 6
	g.inH = m.inputRows(g.inW-4) + 2
	if !g.boxed {
		g.inH = m.inputRows(g.inW)
	}
	g.inY = g.statusY - gap - g.inH
	g.noteY = g.inY - 1
	if g.roomy {
		g.top = 1
	}
	y := g.top + 1
	if g.roomy {
		y++
	}
	if m.huddleBarShown() {
		g.hud = y
		y++
		if g.roomy {
			y++
		}
	}
	if m.bannerText() != "" {
		g.banner = y
		y++
	}
	g.msgTop, g.msgBot = y, max(g.noteY, y)
	if g.noteY < 0 {
		// Tiny terminals: keep the input, lose the rest from the top.
		g.noteY, g.msgTop, g.msgBot = -1, 0, 0
	}
	return g
}

// inputRows is how many text rows the input needs at text width w.
func (m *Model) inputRows(w int) int {
	return min(inputLines(m.input.Value(), max(w, 1)), m.inputMax())
}

// paneHeight is how many rows p shows: the split thread pane has its own
// header and ends above its input.
func (m *Model) paneHeight(p *pane) int {
	g := m.geo()
	if p == &m.thread && g.split {
		return max(g.inY-1-(g.top+2), 0)
	}
	return max(g.msgBot-g.msgTop, 0)
}

// dims is the layout for code that only needs widths and heights.
type dims struct {
	side           bool
	mainW          int // the message column
	inputW         int // the input's text width
	msgW, thrW     int
	split, thrFull bool
	inputH, bodyH  int
	banner         bool
}

func (m *Model) dims() dims {
	g := m.geo()
	if m.w <= 0 || m.h <= 0 {
		return dims{}
	}
	d := dims{side: g.SB > 0, mainW: g.CW, inputW: max(g.inW-4, 1), msgW: g.CW, split: g.split, thrFull: g.thrFull,
		inputH: g.inH - 2, bodyH: g.msgBot - g.msgTop, banner: g.banner >= 0}
	if !g.boxed {
		d.inputW, d.inputH = max(g.inW, 1), g.inH
	}
	switch {
	case g.split:
		d.thrW = max(threadW-4, 1)
	case g.thrFull:
		d.thrW = g.CW
	}
	return d
}
