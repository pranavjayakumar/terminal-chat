package tui

import (
	"strconv"
	"strings"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/sanitize"
)

// The loading screen shows only when there is no local cache yet (first
// run): a start with a cache draws the chat at once. It ends when the first
// channels arrive or the connection stops, then fades into the chat.

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
	c.fill(0, 0, c.w, c.h, th.bg, "")
	old := c.setClip(rect{0, 0, c.w, c.h})
	defer c.setClip(old)
	logo := []string{"▀█▀  █▀▀", " █   █▄▄"}
	ly := max(c.h/2-7, 0)
	lx := (c.w - 8) / 2
	sweep := -9.0
	if !m.fx.off {
		sweep = float64(m.now().UnixMilli()/60%14) - 3
	}
	for j, ln := range logo {
		for i, r := range []rune(ln) {
			glow := max(0, 1-abs(float64(i)-sweep)/2.5)
			c.put(lx+i, ly+j, string(r), fgc(mix(th.accent, th.fg, glow*0.55)), "")
		}
	}
	tag := "terminal chat"
	c.put((c.w-len(tag))/2, ly+3, tag, fgc(th.faint), "")
	st := m.status.State
	connected := st == client.Syncing || st == client.Online
	transport := m.status.Transport
	if transport == "" {
		transport = "…"
	}
	steps := []struct {
		label, done string
		ok          bool
	}{
		{"opening cache", "first run", true},
		{"connecting", transport, connected},
		{"checking keys", "", st == client.Online || len(m.chans) > 0},
		{"syncing", strconv.Itoa(len(m.chans)) + " channels", len(m.chans) > 0},
	}
	sw := min(40, c.w-2)
	sx := (c.w - sw) / 2
	spin := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	frame := int(m.now().UnixMilli()/80) % len(spin)
	done := 0
	for j, s := range steps {
		y := ly + 6 + j
		if s.ok {
			done++
			c.put(sx, y, "✓", fgc(th.ok), "")
			c.put(sx+2, y, s.label, fgc(th.dim), "")
			if s.done != "" {
				d := truncate(s.done, max(sw-strWidth(s.label)-4, 0))
				c.put(sx+sw-strWidth(d), y, d, fgc(th.faint), "")
			}
			continue
		}
		g := "·"
		if !m.fx.off {
			g = string(spin[frame])
		}
		c.put(sx, y, g, fgc(th.accent), "")
		c.put(sx+2, y, s.label, fgc(th.fg), "")
		if j == 1 && m.status.Err != "" {
			e := truncate(sanitize.Line(m.status.Err), max(sw-strWidth(s.label)-4, 0))
			c.put(sx+sw-strWidth(e), y, e, fgc(th.warn), "")
		}
		break
	}
	n := sw * done / len(steps)
	c.put(sx, ly+11, strings.Repeat("━", n), fgc(th.accent), "")
	c.put(sx+n, ly+11, strings.Repeat("━", sw-n), fgc(th.line), "")
	if v := sanitize.Line(m.opt.Version); v != "" {
		c.put((c.w-strWidth(v))/2, min(c.h-2, ly+16), v, fgc(th.faint), "")
	}
	if p < 1 {
		for i := range c.cells {
			if p < 0.5 {
				c.cells[i] = chat.cells[i]
				q := 1 - p*2
				c.cells[i].fg = mix(th.bg, c.cells[i].fg, q)
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
