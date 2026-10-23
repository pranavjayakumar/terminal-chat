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
	dLogoIn    = 300 * time.Millisecond // the Modern logo's fade in
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

// introMoving: something in the intro moves now (the logo fading in, the
// opening), so frames tick fast; a still hold only waits (introWake).
func (m *Model) introMoving() bool {
	if !m.introShown() {
		return false
	}
	if !m.fx.zoomAt.IsZero() {
		return true
	}
	return m.skin().intro == introFade && m.now().Sub(m.fx.introAt) < dLogoIn
}

// introWake is how long a still hold waits before the intro may open:
// until the minimum when ready, else until the maximum; 0 otherwise.
func (m *Model) introWake() time.Duration {
	if !m.introShown() || !m.fx.zoomAt.IsZero() || m.introMoving() {
		return 0
	}
	at := dIntroHold
	if !m.introReady() {
		at = dIntroMax
	}
	return max(m.fx.introAt.Add(at).Sub(m.now()), time.Millisecond)
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
	p := 0.0 // share of the opening done
	if !m.fx.zoomAt.IsZero() {
		p = min(float64(m.now().Sub(m.fx.zoomAt))/float64(dIntroOpen), 1)
	}
	if m.skin().intro == introFade {
		m.drawFadeIntro(c, p)
		return
	}
	k := ease(p)
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

// drawFadeIntro is the Modern intro, by elapsed share p of the opening:
// the logo eases in (cubic) while it holds, then fades out and the chat
// fades in, each half smoothstepped so the speed never jumps where they
// meet.
func (m *Model) drawFadeIntro(c *canvas, p float64) {
	th := m.st.th
	in := ease(float64(m.now().Sub(m.fx.introAt)) / float64(dLogoIn))
	const out = 0.4 // share of the opening the logo takes to fade out
	if p < out {
		alpha := in * (1 - smoothstep(p/out))
		c.fill(0, 0, c.w, c.h, th.bg, "")
		m.drawLogo(c, rect{0, 0, c.w, c.h}, th.bg)
		for i := range c.cells {
			c.cells[i].fg = mix(th.bg, c.cells[i].fg, alpha)
			c.cells[i].ul = 0
			c.cells[i].act = 0
		}
		return
	}
	q := smoothstep((p - out) / (1 - out))
	for i := range c.cells {
		c.cells[i].fg = mix(th.bg, c.cells[i].fg, q)
		c.cells[i].ul = mix(th.bg, c.cells[i].ul, q)
		c.cells[i].bg = mix(th.bg, c.cells[i].bg, q)
	}
}

// smoothstep eases x (0..1) in and out.
func smoothstep(x float64) float64 {
	x = min(max(x, 0), 1)
	return x * x * (3 - 2*x)
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
