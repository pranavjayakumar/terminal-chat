package tui

import (
	"math"
	"time"

	"github.com/skailama/tc/internal/client"
)

// A skin with an intro (Retro) opens with a zoom while tc connects: a bare
// desktop of its pattern, then a frame in its box glyphs that grows from
// the center with the UI inside. The desktop holds at least dIntroHold and until the cache or the
// connection is ready (dIntroMax at most, so a slow network never hides
// the UI); then the zoom plays. A key or a click ends it at once.
const (
	dIntroHold = 250 * time.Millisecond
	dIntroMax  = 2500 * time.Millisecond
	dIntroZoom = 450 * time.Millisecond
)

// introWanted: this start plays the intro.
func (m *Model) introWanted() bool {
	return m.skin().intro && !m.fx.off && m.ui.Startup != startInstant
}

// introShown: the intro covers the screen now.
func (m *Model) introShown() bool { return m.fx.intro && !m.fx.off && !m.fx.introAt.IsZero() }

// startIntro starts the clock when the screen first has a size.
func (m *Model) startIntro() {
	if m.fx.intro && m.fx.introAt.IsZero() && m.w > 0 && m.h > 0 {
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
	if now.Sub(f.zoomAt) >= dIntroZoom {
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

// introWake is how long until the desktop may give way to the zoom, while
// it holds; 0 otherwise.
func (m *Model) introWake() time.Duration {
	f := &m.fx
	if !m.introShown() || !f.zoomAt.IsZero() {
		return 0
	}
	at := dIntroHold
	if !m.introReady() {
		at = dIntroMax
	}
	return max(f.introAt.Add(at).Sub(m.now()), time.Millisecond)
}

// introZooming: the rectangle is growing (frames tick fast).
func (m *Model) introZooming() bool {
	return m.introShown() && !m.fx.zoomAt.IsZero()
}

// drawIntro covers the frame with the desktop, leaving the growing
// rectangle open.
func (m *Model) drawIntro(c *canvas) {
	if !m.introShown() {
		return
	}
	th := m.st.th
	k := 0.0
	if !m.fx.zoomAt.IsZero() {
		k = ease(float64(m.now().Sub(m.fx.zoomAt)) / float64(dIntroZoom))
	}
	hw := int(math.Round(k * float64(c.w) / 2))
	hh := int(math.Round(k * float64(c.h) / 2))
	sk := m.skin()
	pat, glyph := th.patternColor(), sk.desktop
	if glyph == "" {
		glyph = " "
	}
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
				if k >= 1 || (dx != hw-1 && dy != hh-1) {
					continue
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
}
