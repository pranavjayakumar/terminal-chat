package tui

import (
	"math"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/sanitize"
)

// Every skin opens with a short intro while tc connects, kept quick so a
// start never feels slow: the tc logo in the theme's accent over the line
// "connecting…". It holds at least dIntroHold, and until the cache or the
// connection is ready (dIntroMax at most); then it opens. Retro shows the
// logo in a double-line dialog with a drop shadow on its desktop and
// opens with a zoom: a frame in its box glyphs grows from the dialog to the
// whole screen with the UI inside. Modern shows the logo on a plain screen
// and fades into the chat. A key or a click ends it at once; Startup ›
// Instant and Animations off skip it.
const (
	dIntroHold = 700 * time.Millisecond
	dIntroMax  = 1200 * time.Millisecond
	dIntroOpen = 500 * time.Millisecond
)

const (
	introZoom = "zoom"
	introFade = "fade"
)

// introWanted: this start plays the intro.
func (m *Model) introWanted() bool {
	return m.skin().intro != "" && !m.fx.off && m.ui.Startup != startInstant
}

// introShown: the intro covers the screen now.
func (m *Model) introShown() bool { return m.fx.intro && !m.fx.off && !m.fx.introAt.IsZero() }

// startIntro starts the intro's clock when the screen first has a size.
func (m *Model) startIntro() {
	if m.w <= 0 || m.h <= 0 {
		return
	}
	if m.fx.intro && m.fx.introAt.IsZero() {
		m.fx.introAt = m.now()
	}
}

func (m *Model) introReady() bool {
	return len(m.chans) > 0 || m.status.State == client.Online || m.status.Stopped
}

// stepIntro moves the intro from the desktop to the zoom, and ends it.
func (m *Model) stepIntro() {
	f := &m.fx
	if !f.intro || f.introAt.IsZero() {
		return
	}
	if f.off {
		f.intro = false
		return
	}
	now := m.now()
	if f.zoomAt.IsZero() {
		el := now.Sub(f.introAt)
		if el >= dIntroHold && (m.introReady() || el >= dIntroMax) {
			f.zoomAt = now
		}
		return
	}
	if now.Sub(f.zoomAt) >= dIntroOpen {
		f.intro = false
	}
}

// skipIntro ends the intro; it reports whether one was showing.
func (m *Model) skipIntro() bool {
	if !m.introShown() {
		return false
	}
	m.fx.intro = false
	return true
}

// introZooming: the rectangle is growing (frames tick fast).
func (m *Model) introZooming() bool {
	return m.introShown() && !m.fx.zoomAt.IsZero()
}

// introBox is the Retro connecting dialog's rectangle, centered.
func introBox(w, h int) rect {
	dw, dh := min(30, w-4), min(8, h-2)
	x, y := (w-dw)/2, (h-dh)/2
	return rect{x, y, x + dw, y + dh}
}

// drawIntro covers the frame while the intro holds, and opens it after.
func (m *Model) drawIntro(c *canvas) {
	if !m.introShown() {
		return
	}
	k := 0.0
	if !m.fx.zoomAt.IsZero() {
		k = ease(float64(m.now().Sub(m.fx.zoomAt)) / float64(dIntroOpen))
	}
	if m.skin().intro == introFade {
		m.drawFadeIntro(c, k)
		return
	}
	th := m.st.th
	sk := m.skin()
	box := introBox(c.w, c.h)
	hw, hh := 0, 0 // the open rectangle's half sizes
	if !m.fx.zoomAt.IsZero() {
		bw, bh := float64(box.x1-box.x0)/2, float64(box.y1-box.y0)/2
		hw = int(math.Round(bw + (float64(c.w)/2-bw)*k))
		hh = int(math.Round(bh + (float64(c.h)/2-bh)*k))
	}
	glyph := sk.desktop
	if glyph == "" {
		glyph = " "
	}
	pat := th.patternOn(th.bg)
	if !m.st.color {
		pat = th.faint
	}
	b := sk.box
	cx, cy := float64(c.w)/2-0.5, float64(c.h)/2-0.5
	old := c.setClip(rect{0, 0, c.w, c.h})
	defer c.setClip(old)
	for y := 0; y < c.h; y++ {
		dy := int(math.Abs(float64(y) - cy))
		top := float64(y) < cy
		for x := 0; x < c.w; x++ {
			dx := int(math.Abs(float64(x) - cx))
			left := float64(x) < cx
			if dx < hw && dy < hh {
				if dx != hw-1 && dy != hh-1 {
					continue // the UI shows through
				}
				g := b.v
				switch {
				case dx == hw-1 && dy == hh-1 && top:
					g = map[bool]string{true: b.tl, false: b.tr}[left]
				case dx == hw-1 && dy == hh-1:
					g = map[bool]string{true: b.bl, false: b.br}[left]
				case dy == hh-1:
					g = b.h
				}
				c.clearHalf(x, y)
				*c.at(x, y) = cell{s: g, w: 1, fg: th.accent, bg: th.bg}
				continue
			}
			c.clearHalf(x, y)
			*c.at(x, y) = cell{s: glyph, w: 1, fg: pat, bg: th.bg}
		}
	}
	if m.fx.zoomAt.IsZero() {
		m.box(c, box.x0, box.y0, box.x1-box.x0, box.y1-box.y0, th.line, th.panel)
		m.shade(c, box)
		m.drawLogo(c, box, th.panel)
	}
}

// drawFadeIntro is the Modern intro: the logo screen, fading into the
// chat once it opens.
func (m *Model) drawFadeIntro(c *canvas, k float64) {
	th := m.st.th
	if k <= 0 {
		c.fill(0, 0, c.w, c.h, th.bg, "")
		m.drawLogo(c, rect{0, 0, c.w, c.h}, th.bg)
		return
	}
	logo := &canvas{w: c.w, h: c.h, cells: make([]cell, len(c.cells)), th: th, clip: rect{0, 0, c.w, c.h}}
	logo.fill(0, 0, c.w, c.h, th.bg, "")
	m.drawLogo(logo, rect{0, 0, c.w, c.h}, th.bg)
	for i := range c.cells {
		if k < 0.5 { // the logo fades out, then the chat fades in
			c.cells[i] = logo.cells[i]
			c.cells[i].fg = mix(c.cells[i].fg, th.bg, k*2)
			continue
		}
		c.cells[i].fg = mix(th.bg, c.cells[i].fg, (k-0.5)*2)
		c.cells[i].bg = mix(th.bg, c.cells[i].bg, (k-0.5)*2)
	}
}

// logoRows is the tc wordmark.
var logoRows = [2]string{"▀█▀  █▀▀", " █   █▄▄"}

// drawLogo centers the tc logo, in the solid accent, over "connecting…"
// (or the connection error) inside r on bg.
func (m *Model) drawLogo(c *canvas, r rect, bg rgb) {
	th := m.st.th
	w, h := r.x1-r.x0, r.y1-r.y0
	text := "connecting…"
	if m.status.State == client.Online {
		text = "connected"
	} else if e := sanitize.Line(m.status.Err); e != "" {
		text = e
	}
	text = truncate(text, max(w-4, 1))
	y := r.y0 + (h-4)/2
	if h < 6 || w < 12 {
		c.put(r.x0+(w-strWidth(text))/2, r.y0+h/2, text, fgc(th.dim).on(bg), "")
		return
	}
	lx := r.x0 + (w-strWidth(logoRows[0]))/2
	for j, row := range logoRows {
		c.put(lx, y+j, row, fgc(th.accent).on(bg), "")
	}
	c.put(r.x0+(w-strWidth(text))/2, y+3, text, fgc(th.dim).on(bg), "")
}
