package tui

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/muesli/termenv"
	"github.com/rivo/uniseg"
)

type attr uint8

const (
	aBold attr = 1 << iota
	aFaint
	aItalic
	aUnderline
	aReverse
	aMonoRev // reverse only when there is no color (selection under NO_COLOR)
)

// cstyle is a cell's look. A zero fg/bg flag keeps what is under it.
type cstyle struct {
	fg, bg       rgb
	hasFg, hasBg bool
	attr         attr
}

func fgc(c rgb) cstyle              { return cstyle{fg: c, hasFg: true} }
func fgb(c rgb) cstyle              { return cstyle{fg: c, hasFg: true, attr: aBold} }
func bgc(c rgb) cstyle              { return cstyle{bg: c, hasBg: true} }
func (s cstyle) on(c rgb) cstyle    { s.bg, s.hasBg = c, true; return s }
func (s cstyle) with(a attr) cstyle { s.attr |= a; return s }

// cell is one terminal cell. A wide grapheme fills its first cell and
// leaves w == 0 in the next.
type cell struct {
	s     string
	w     uint8
	fg    rgb
	bg    rgb
	attr  attr
	act   uint16 // index into canvas.acts; 0 none
	hover bool   // gets the hover tint when the pointer is over its action
}

// canvas is one frame: every view draws into it, then it is encoded once.
// Each cell may carry a mouse action, so a click maps back to what is
// drawn there.
type canvas struct {
	w, h  int
	cells []cell
	acts  []string
	actIx map[string]uint16
	clip  rect
	th    *theme
}

type rect struct{ x0, y0, x1, y1 int } // [x0, x1) × [y0, y1)

func newCanvas(w, h int, th *theme) *canvas {
	return reuseCanvas(nil, w, h, th)
}

// reuseCanvas blanks c for a new frame, or makes a canvas when c is nil or
// the wrong size, so steady frames don't allocate a new grid.
func reuseCanvas(c *canvas, w, h int, th *theme) *canvas {
	if c == nil || c.w != w || cap(c.cells) < w*h {
		c = &canvas{cells: make([]cell, w*h)}
	}
	c.w, c.h, c.th = w, h, th
	c.cells = c.cells[:w*h]
	c.acts = append(c.acts[:0], "")
	clear(c.actIx)
	blank := cell{s: " ", w: 1, fg: th.fg, bg: th.bg}
	for i := range c.cells {
		c.cells[i] = blank
	}
	c.clip = rect{0, 0, w, h}
	return c
}

// clone copies c (the theme cross-fade keeps a frame while new ones draw).
func (c *canvas) clone() *canvas {
	n := *c
	n.cells = append([]cell(nil), c.cells...)
	n.acts = append([]string(nil), c.acts...)
	n.actIx = nil
	return &n
}

func (c *canvas) setClip(r rect) rect {
	old := c.clip
	c.clip = rect{max(r.x0, 0), max(r.y0, 0), min(r.x1, c.w), min(r.y1, c.h)}
	return old
}

func (c *canvas) in(x, y int) bool {
	return x >= c.clip.x0 && x < c.clip.x1 && y >= c.clip.y0 && y < c.clip.y1
}

func (c *canvas) at(x, y int) *cell { return &c.cells[y*c.w+x] }

func (c *canvas) act(a string) uint16 {
	if a == "" {
		return 0
	}
	if i, ok := c.actIx[a]; ok {
		return i
	}
	if c.actIx == nil {
		c.actIx = map[string]uint16{}
	}
	if len(c.acts) >= 1<<16-1 {
		return 0 // out of room: the cell gets no action rather than a wrong one
	}
	c.acts = append(c.acts, a)
	i := uint16(len(c.acts) - 1)
	c.actIx[a] = i
	return i
}

// actionAt is the action drawn at x, y, or "".
func (c *canvas) actionAt(x, y int) string {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return ""
	}
	return c.acts[c.at(x, y).act]
}

// clear blanks a cell, fixing up a wide grapheme it splits.
func (c *canvas) clearHalf(x, y int) {
	cl := c.at(x, y)
	if cl.w == 0 && x > 0 {
		p := c.at(x-1, y)
		if p.w == 2 {
			p.s, p.w = " ", 1
		}
	}
	if cl.w == 2 && x+1 < c.w {
		n := c.at(x+1, y)
		n.s, n.w = " ", 1
	}
}

// put draws s at x, y and returns the column after it. Graphemes that
// don't fit the clip are dropped; a wide one never straddles the edge.
func (c *canvas) put(x, y int, s string, st cstyle, act string) int {
	ai := c.act(act)
	state := -1
	for s != "" {
		var g string
		var w int
		g, s, w, state = uniseg.FirstGraphemeClusterInString(s, state)
		if !utf8.ValidString(g) {
			g, w = "\ufffd", 1 // never pass raw bytes to the terminal
		}
		if w > 2 {
			// A cluster wider than a wide character (Prepend marks) is
			// drawn one rune at a time, as terminals measure it.
			for _, r := range g {
				x = c.put(x, y, string(r), st, act)
			}
			continue
		}
		if w == 0 {
			continue
		}
		if c.in(x, y) && c.in(x+w-1, y) {
			c.clearHalf(x, y)
			if w == 2 {
				c.clearHalf(x+1, y)
			}
			cl := c.at(x, y)
			cl.s, cl.w = g, uint8(w)
			c.style(cl, st)
			if ai != 0 {
				cl.act = ai
			}
			if w == 2 {
				n := c.at(x+1, y)
				*n = *cl
				n.s, n.w = "", 0
			}
		}
		x += w
	}
	return x
}

func (c *canvas) style(cl *cell, st cstyle) {
	if st.hasFg {
		cl.fg = st.fg
	} else {
		cl.fg = c.th.fg
	}
	if st.hasBg {
		cl.bg = st.bg
	}
	cl.attr = st.attr
}

// fill paints a rectangle's background, clearing its text.
func (c *canvas) fill(x, y, w, h int, bg rgb, act string) {
	ai := c.act(act)
	for j := y; j < y+h; j++ {
		for i := x; i < x+w; i++ {
			if !c.in(i, j) {
				continue
			}
			c.clearHalf(i, j)
			*c.at(i, j) = cell{s: " ", w: 1, fg: c.th.fg, bg: bg, act: ai}
		}
	}
}

// tag gives every cell of a rectangle an action without changing its look.
// Cells that already have one keep it unless force is set.
func (c *canvas) tag(x, y, w, h int, act string, hover, force bool) {
	ai := c.act(act)
	for j := y; j < y+h; j++ {
		for i := x; i < x+w; i++ {
			if c.in(i, j) {
				cl := c.at(i, j)
				if cl.act == 0 || force {
					cl.act = ai
					cl.hover = hover
				}
			}
		}
	}
}

// each runs f on every cell of a rectangle inside the clip.
func (c *canvas) each(x, y, w, h int, f func(*cell)) {
	for j := y; j < y+h; j++ {
		for i := x; i < x+w; i++ {
			if c.in(i, j) {
				f(c.at(i, j))
			}
		}
	}
}

// pill draws ▐text▌ with rounded-looking ends on bg.
func (c *canvas) pill(x, y int, text string, st cstyle, act string) int {
	end := cstyle{fg: st.bg, hasFg: true}
	x = c.put(x, y, "▐", end, act)
	x = c.put(x, y, text, st, act)
	return c.put(x, y, "▌", end, act)
}

// putANSI draws a string styled with SGR sequences (lipgloss output), at
// most maxW cells. Unstyled text gets base.
func (c *canvas) putANSI(x, y int, s string, maxW int, base cstyle, act string) int {
	if maxW <= 0 {
		return x
	}
	old := c.clip
	c.clip = rect{max(old.x0, x), old.y0, min(old.x1, x+maxW), old.y1}
	defer func() { c.clip = old }()
	st := base
	for len(s) > 0 {
		if s[0] == 0x1b && len(s) == 1 {
			break // a lone trailing ESC
		}
		if s[0] == 0x1b {
			switch s[1] {
			case '[':
				j := 2
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j < len(s) && s[j] == 'm' {
					st = applySGR(st, base, s[2:j])
				}
				s = s[min(j+1, len(s)):]
				continue
			case ']': // OSC: skip to BEL or ST
				j := 2
				for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
					j++
				}
				if j < len(s) && s[j] == 0x1b {
					j++
				}
				s = s[min(j+1, len(s)):]
				continue
			}
			_, n := utf8.DecodeRuneInString(s[1:])
			s = s[1+n:] // ESC and one whole rune: never split a character
			continue
		}
		j := strings.IndexByte(s, 0x1b)
		if j < 0 {
			j = len(s)
		}
		x = c.put(x, y, s[:j], st, act)
		s = s[j:]
	}
	return x
}

func applySGR(st, base cstyle, params string) cstyle {
	if params == "" {
		return base
	}
	ps := strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ':' })
	num := func(i int) int {
		if i >= len(ps) {
			return 0
		}
		n, _ := strconv.Atoi(ps[i])
		return n
	}
	for i := 0; i < len(ps); i++ {
		switch n := num(i); {
		case n == 0:
			st = base
		case n == 1:
			st.attr |= aBold
		case n == 2:
			st.attr |= aFaint
		case n == 3:
			st.attr |= aItalic
		case n == 4:
			st.attr |= aUnderline
		case n == 7:
			st.attr |= aReverse
		case n == 22:
			st.attr &^= aBold | aFaint
		case n == 23:
			st.attr &^= aItalic
		case n == 24:
			st.attr &^= aUnderline
		case n == 27:
			st.attr &^= aReverse
		case n == 39:
			st.fg, st.hasFg = base.fg, base.hasFg
		case n == 49:
			st.bg, st.hasBg = base.bg, base.hasBg
		case n == 38 || n == 48:
			var col rgb
			switch num(i + 1) {
			case 2:
				col = rgb(num(i+2)<<16 | num(i+3)<<8 | num(i+4))
				i += 4
			case 5:
				col = xterm256(num(i + 2))
				i += 2
			default:
				continue
			}
			if n == 38 {
				st.fg, st.hasFg = col, true
			} else {
				st.bg, st.hasBg = col, true
			}
		case n >= 30 && n <= 37:
			st.fg, st.hasFg = xterm256(n-30), true
		case n >= 90 && n <= 97:
			st.fg, st.hasFg = xterm256(n-90+8), true
		case n >= 40 && n <= 47:
			st.bg, st.hasBg = xterm256(n-40), true
		case n >= 100 && n <= 107:
			st.bg, st.hasBg = xterm256(n-100+8), true
		}
	}
	return st
}

var ansi16 = [16]rgb{0x000000, 0xcd3131, 0x0dbc79, 0xe5e510, 0x2472c8, 0xbc3fbc, 0x11a8cd, 0xe5e5e5,
	0x666666, 0xf14c4c, 0x23d18b, 0xf5f543, 0x3b8eea, 0xd670d6, 0x29b8db, 0xffffff}

func xterm256(n int) rgb {
	switch {
	case n < 16:
		return ansi16[max(n, 0)]
	case n < 232:
		n -= 16
		lv := func(v int) uint32 {
			if v == 0 {
				return 0
			}
			return uint32(55 + v*40)
		}
		return rgb(lv(n/36)<<16 | lv(n/6%6)<<8 | lv(n%6))
	case n < 256:
		v := uint32(8 + (n-232)*10)
		return rgb(v<<16 | v<<8 | v)
	}
	return 0
}

// nearest256 is the closest xterm color to c among 16–255 (the cube and the
// grey ramp), so nearby dark surfaces stay distinct steps of the ramp
// instead of all becoming one grey.
func nearest256(c rgb) int {
	r, g, b := c.parts()
	best, bestD := 16, 1e18
	for i := 16; i < 256; i++ {
		x := xterm256(i)
		xr, xg, xb := x.parts()
		d := (r-xr)*(r-xr)*0.3 + (g-xg)*(g-xg)*0.59 + (b-xb)*(b-xb)*0.11
		if d < bestD {
			best, bestD = i, d
		}
	}
	return best
}

// encoder turns cells into escape sequences for one color profile.
type encoder struct {
	prof   termenv.Profile
	color  bool
	cache  map[rgb][2]string
	pinned map[rgb]int // 256 colors: a theme's surfaces, kept distinct
}

// pinSurfaces gives th's bg, panel, element and line distinct 256-color
// steps, each one further from bg than the last.
func (e *encoder) pinSurfaces(th *theme) {
	e.pinned = map[rgb]int{}
	prev := -1
	for _, c := range []rgb{th.bg, th.panel, th.element, th.line} {
		i := nearest256(c)
		if i >= 232 && prev >= 232 {
			if th.light && i >= prev {
				i = max(prev-1, 232)
			} else if !th.light && i <= prev {
				i = min(prev+1, 255)
			}
		}
		e.pinned[c], prev = i, i
	}
}

func newEncoder(p termenv.Profile, color bool) *encoder {
	return &encoder{prof: p, color: color, cache: map[rgb][2]string{}}
}

func (e *encoder) seq(c rgb, bg bool) string {
	if bg && e.prof == termenv.ANSI {
		return "" // 16 colors can't hold the theme's surfaces: keep the terminal's own
	}
	k, ok := e.cache[c]
	if !ok {
		var col termenv.Color
		if e.prof == termenv.ANSI256 {
			i, pinned := e.pinned[c]
			if !pinned {
				i = nearest256(c)
			}
			col = termenv.ANSI256Color(i)
		} else {
			col = e.prof.Color(c.String())
		}
		if col != nil {
			k = [2]string{col.Sequence(false), col.Sequence(true)}
		}
		e.cache[c] = k
	}
	if bg {
		return k[1]
	}
	return k[0]
}

// encode renders the canvas: h lines of exactly w cells, joined by "\n".
// Without color, the theme's quiet colors become faint text.
func (c *canvas) encode(e *encoder) string {
	var b strings.Builder
	b.Grow(c.w * c.h * 4)
	type key struct {
		fg, bg rgb
		attr   attr
	}
	quiet := func(fg rgb) bool { return contrast(fg, c.th.bg) < 6 }
	for y := 0; y < c.h; y++ {
		if y > 0 {
			b.WriteByte('\n')
		}
		var cur key
		first := true
		for x := 0; x < c.w; x++ {
			cl := c.at(x, y)
			if cl.w == 0 {
				continue
			}
			k := key{cl.fg, cl.bg, cl.attr}
			if e.color && e.prof == termenv.ANSI && cl.attr&aMonoRev != 0 {
				k.attr |= aReverse // 16 colors draw no backgrounds: reverse marks it
			}
			if !e.color {
				k.fg, k.bg = 0, 0
				if cl.attr&aMonoRev != 0 {
					k.attr |= aReverse
				}
				if cl.s != " " && quiet(cl.fg) && cl.attr&aBold == 0 {
					k.attr |= aFaint
				}
			}
			k.attr &^= aMonoRev
			if first || k != cur {
				b.WriteString("\x1b[0")
				for _, a := range [...]struct {
					f attr
					s string
				}{{aBold, ";1"}, {aFaint, ";2"}, {aItalic, ";3"}, {aUnderline, ";4"}, {aReverse, ";7"}} {
					if k.attr&a.f != 0 {
						b.WriteString(a.s)
					}
				}
				if e.color {
					if s := e.seq(k.fg, false); s != "" {
						b.WriteString(";" + s)
					}
					if s := e.seq(k.bg, true); s != "" {
						b.WriteString(";" + s)
					}
				}
				b.WriteByte('m')
				cur, first = k, false
			}
			b.WriteString(cl.s)
		}
		b.WriteString("\x1b[0m")
	}
	return b.String()
}
