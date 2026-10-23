package tui

import (
	"strconv"
	"strings"
)

// The completion popup (/ commands, @ people, # channels): a floating
// panel on the input's left edge, one blank row above the input. Its kind
// and position sit in the top edge, the keys in the bottom edge.

const popupMaxW = 72

// emojiPopupW is the ":" popup's width: an emoji and a shortcode need less.
const emojiPopupW = 44

// popupRect is the panel's box and how many items it lists. framed is
// false when only bare rows fit (tiny terminals).
func (m *Model) popupRect(g geo) (r rect, n int, framed bool) {
	w := min(popupMaxW, g.inW, g.CW)
	if m.comp.kind == ':' {
		w = min(w, emojiPopupW)
	}
	bottom := g.inY - 1 // the blank row above the input
	if !g.boxed {
		bottom = g.inY
	}
	room := bottom - g.msgTop
	if w < 12 || room <= 0 {
		return rect{}, 0, false
	}
	n = min(len(m.comp.items), compRows)
	if room >= 5 { // top edge, blank, a row, blank, bottom edge
		n = min(n, room-4)
		return rect{g.inX, bottom - n - 4, g.inX + w, bottom}, n, true
	}
	n = min(n, room)
	return rect{g.inX, bottom - n, g.inX + w, bottom}, n, false
}

// popupCols are the columns for the completion, its argument hint and its
// description: 3, 12 and 32 in the design, pushed right by long names and
// dropped when the panel is too narrow.
func (m *Model) popupCols(w, start, end int) (args, desc int) {
	tw, hw, hints := 0, 0, false
	for _, it := range m.comp.items[start:end] {
		tw = max(tw, strWidth(it.text))
		hw = max(hw, strWidth(it.hint))
		hints = hints || it.hint != ""
	}
	args = max(12, 3+tw+2)
	desc = max(32, args+hw+2)
	if !hints {
		desc = args // people and channels: name | detail
	}
	if desc+8 > w-3 {
		desc = 0 // too narrow for descriptions
	}
	if args+4 > w-3 {
		args = 0
	}
	return args, desc
}

func (m *Model) drawPopup(c *canvas, g geo) {
	if !m.compShown() || m.overlayShown() {
		return
	}
	r, n, framed := m.popupRect(g)
	if n <= 0 {
		return
	}
	th := m.st.th
	cp := &m.comp
	k := ease(m.progress(m.fx.compAt, dToast))
	if k < 0.6 && framed {
		r.y0++ // it rises into place over the blank row
		r.y1++
	}
	m.floatGutter(c, g, r)
	if framed {
		m.shade(c, r)
	}
	old := c.setClip(rect{r.x0, max(r.y0, 0), r.x1, r.y1})
	defer c.setClip(old)
	w := r.x1 - r.x0
	pbg := mix(th.bg, th.panel, k)
	c.fill(r.x0, r.y0, w, r.y1-r.y0, pbg, "noop")
	y := r.y0
	if framed {
		m.box(c, r.x0, r.y0, w, r.y1-r.y0, mix(th.bg, th.line, k), pbg)
		kind := " " + map[byte]string{'/': "Commands", '@': "People", '#': "Channels", ':': "Emoji"}[cp.kind] + " "
		c.put(r.x0+2, y, kind, fgc(mix(pbg, th.dim, k)), "noop")
		if count := " " + strconv.Itoa(cp.sel+1) + "/" + strconv.Itoa(len(cp.items)) + " "; w >= strWidth(kind)+strWidth(count)+6 {
			c.put(r.x1-2-strWidth(count), y, count, fgc(th.faint), "noop")
		}
		hint := " ↑↓ move · ⇥ complete · ⏎ run · esc "
		if cp.kind == ':' {
			hint = " ↑↓ move · ⇥/⏎ insert · esc "
		}
		if w >= strWidth(hint)+4 {
			c.put(r.x1-2-strWidth(hint), r.y1-1, hint, fgc(th.faint), "noop")
		} else if short := " ↑↓ ⇥ ⏎ esc "; w >= strWidth(short)+4 {
			c.put(r.x1-2-strWidth(short), r.y1-1, short, fgc(th.faint), "noop")
		}
		c.tag(r.x0, r.y0, w, r.y1-r.y0, "noop", false, true)
		y += 2
	}
	start, end := window(len(cp.items), cp.sel, n)
	args, desc := m.popupCols(w, start, end)
	// The pointer over a row highlights it; otherwise the selection.
	lit := cp.sel
	if strings.HasPrefix(m.hover, "comp:") {
		if i, err := strconv.Atoi(m.hover[5:]); err == nil && i >= start && i < end {
			lit = i
		}
	}
	for i := start; i < end; i++ {
		it := cp.items[i]
		act := "comp:" + strconv.Itoa(i)
		on := i == lit
		bg := pbg
		if on {
			bg = th.accent
		}
		pick := func(normal rgb, a attr) cstyle {
			col := normal
			if on {
				col, a = th.accentFg, a|aMonoRev
			}
			return cstyle{fg: mix(pbg, col, k), hasFg: true, attr: a, bg: bg, hasBg: true}
		}
		c.fill(r.x0+1, y, w-2, 1, bg, act)
		c.tag(r.x0+1, y, w-2, 1, act, false, true)
		x0 := r.x0 + 3
		if !framed {
			x0 = r.x0 + 1
		}
		limit := r.x1 - 3
		if len(cp.items) > n {
			limit-- // a gap before the ↑/↓ mark
		}
		cut := func(s string, from int) string { return truncate(s, max(limit-from, 0)) }
		if cp.kind == ':' {
			m.drawEmojiRow(c, it, act, on, x0, y, limit, pick)
			y++
			continue
		}
		c.put(x0, y, cut(it.text, x0), pick(th.fg, aBold), act)
		switch {
		case args > 0 && it.hint != "":
			c.put(r.x0+args, y, cut(it.hint, r.x0+args), pick(th.dim, 0), act)
		}
		if desc > 0 && it.desc != "" {
			c.put(r.x0+desc, y, cut(it.desc, r.x0+desc), pick(th.faint, 0), act)
		}
		if i == end-1 && end < len(cp.items) {
			c.put(r.x1-3, y, "↓", pick(th.faint, 0), act)
		}
		if i == start && start > 0 {
			c.put(r.x1-3, y, "↑", pick(th.faint, 0), act)
		}
		y++
	}
}

// floatGutter blanks the cell on each side of a floating panel, so text
// under it never runs into its frame; a sliver too thin to read goes too.
func (m *Model) floatGutter(c *canvas, g geo, r rect) {
	lo, hi := g.CX, g.CX+g.CW
	if g.split && r.x0 >= g.RX {
		lo, hi = g.RX, m.w
	}
	y0 := max(r.y0, 0)
	blank := func(x0, x1 int) {
		for x := max(x0, g.L); x < min(x1, m.w); x++ {
			bg := m.st.th.bg
			if g.split && x >= g.RX {
				bg = m.st.th.panel
			}
			c.fill(x, y0, 1, r.y1-y0, bg, "")
		}
	}
	const sliver = 8
	if r.x0-lo < sliver {
		blank(lo, r.x0)
	} else {
		blank(r.x0-1, r.x0)
	}
	if hi-r.x1 < sliver {
		blank(r.x1, hi)
	} else {
		blank(r.x1, r.x1+1)
	}
}

// drawEmojiRow draws a ":" completion row: the emoji, then :shortcode:
// with the typed letters in the accent, and "frequent" on the right.
func (m *Model) drawEmojiRow(c *canvas, it compItem, act string, on bool, x, y, limit int, pick func(rgb, attr) cstyle) {
	th := m.st.th
	hit := aBold
	if on {
		hit |= aUnderline // the accent is the row's background here
	}
	if x+2 <= limit {
		c.put(x, y, it.text, pick(th.fg, 0), act)
	}
	x += 4
	right := limit
	if it.desc != "" && limit-x >= strWidth(it.desc)+10 {
		right = limit - strWidth(it.desc)
		c.put(right, y, it.desc, pick(th.faint, 0), act)
		right -= 2
	}
	name := ":" + it.name + ":"
	if strWidth(name) > right-x {
		c.put(x, y, truncate(name, max(right-x, 0)), pick(th.fg, 0), act)
		return
	}
	q := len(m.comp.query)
	x = c.put(x, y, ":", pick(th.faint, 0), act)
	if it.at >= 0 && it.at+q <= len(it.name) {
		x = c.put(x, y, it.name[:it.at], pick(th.fg, 0), act)
		x = c.put(x, y, it.name[it.at:it.at+q], pick(th.accent, hit), act)
		x = c.put(x, y, it.name[it.at+q:], pick(th.fg, 0), act)
	} else {
		x = c.put(x, y, it.name, pick(th.fg, 0), act)
	}
	c.put(x, y, ":", pick(th.faint, 0), act)
}
