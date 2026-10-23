package tui

import "github.com/skailama/tc/internal/client"

// The loading screen shows only when there is no local cache yet (first
// run): a start with a cache draws the chat at once. It is the intro's logo
// screen (intro.go), and stays until the first channels arrive or the
// connection stops, then fades into the chat.

func (m *Model) booting() bool { return m.boot }

// endBoot leaves the loading screen when the chat has something to show.
func (m *Model) endBoot() {
	if !m.boot {
		return
	}
	if len(m.chans) > 0 || m.status.State == client.Online || m.status.Stopped {
		m.boot = false
		m.fx.bootAt = m.now()
	}
}

func (m *Model) drawBoot(c *canvas) {
	th := m.st.th
	p := 1.0
	if !m.boot {
		p = 1 - ease(m.progress(m.fx.bootAt, dBoot))
	}
	if p <= 0 {
		return
	}
	chat := c
	if p < 1 {
		chat = &canvas{w: c.w, h: c.h, cells: append([]cell(nil), c.cells...), th: th}
	}
	old := c.setClip(rect{0, 0, c.w, c.h})
	defer c.setClip(old)
	c.fill(0, 0, c.w, c.h, th.bg, "")
	m.drawLogo(c, rect{0, 0, c.w, c.h}, th.bg)
	if p < 1 {
		for i := range c.cells {
			if p < 0.5 {
				c.cells[i] = chat.cells[i]
				q := 1 - p*2
				c.cells[i].fg = mix(th.bg, c.cells[i].fg, q)
				c.cells[i].ul = mix(th.bg, c.cells[i].ul, q)
				c.cells[i].bg = mix(th.bg, c.cells[i].bg, q)
			} else {
				q := (p - 0.5) * 2
				c.cells[i].fg = mix(th.bg, c.cells[i].fg, q)
			}
		}
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
