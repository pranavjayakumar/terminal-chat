package tui

import (
	"math"
	"strconv"
	"strings"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// View renders the screen: exactly h lines, none wider than w.
func (m *Model) View() string {
	if m.w <= 0 || m.h <= 0 {
		return ""
	}
	if m.reuseFrame && m.lastFrame != "" {
		m.reuseFrame = false
		return m.lastFrame
	}
	c := m.frame()
	m.spare, m.lastCanvas = m.lastCanvas, c
	m.lastFrame = c.encode(m.st.enc)
	return m.lastFrame
}

// frame draws the whole screen into a canvas.
func (m *Model) frame() *canvas {
	th := m.st.th
	c := reuseCanvas(m.spare, m.w, m.h, th)
	m.spare = nil
	g := m.geo()
	m.emo.anchored = false
	m.drawMain(c, g)
	if g.split {
		m.drawThreadPane(c, g)
	}
	m.drawDesktop(c, g)
	if g.split && !m.thread.open() {
		// The pane is sliding out: what follows stays left of it.
		old := c.setClip(rect{0, 0, g.RX, m.h})
		m.drawInput(c, g)
		m.drawPopup(c, g)
		m.drawPicker(c, g)
		c.setClip(old)
	} else {
		m.drawInput(c, g)
		m.drawPopup(c, g)
		m.drawPicker(c, g)
	}
	if g.SB > 0 {
		m.drawSidebar(c, g)
	}
	m.drawStatusBar(c, g)
	m.drawMenuBar(c, g)
	m.drawOverlay(c, g)
	m.drawToast(c, g)
	m.hoverPass(c)
	m.drawReactTip(c, g)
	m.drawTips(c, g)
	if m.booting() || m.running(m.fx.bootAt, dBoot) {
		m.drawBoot(c)
	}
	m.drawIntro(c)
	if p := m.fx.themePrev; p != nil && m.running(m.fx.themeAt, dTheme) && p.w == c.w && p.h == c.h {
		k := ease(m.progress(m.fx.themeAt, dTheme))
		old := m.fx.skinSwap && k < 0.5 // a skin change keeps the old chrome until halfway
		for i := range c.cells {
			if old {
				c.cells[i].s, c.cells[i].w, c.cells[i].attr = p.cells[i].s, p.cells[i].w, p.cells[i].attr
			}
			c.cells[i].fg = mix(p.cells[i].fg, c.cells[i].fg, k)
			c.cells[i].bg = mix(p.cells[i].bg, c.cells[i].bg, k)
		}
	}
	return c
}

// hoverPass tints what the pointer is over.
func (m *Model) hoverPass(c *canvas) {
	if m.hover == "" {
		return
	}
	ai, ok := c.actIx[m.hover]
	if !ok {
		return
	}
	th := m.st.th
	for i := range c.cells {
		cl := &c.cells[i]
		if cl.act == ai && cl.hover && (cl.bg == th.panel || cl.bg == th.bg) {
			cl.bg = th.element
		}
	}
}

func (m *Model) drawMain(c *canvas, g geo) {
	th := m.st.th
	if m.mode == modeViewer && m.view.wide {
		m.drawLines(c, 0, 0, m.w, g.statusY, m.viewerLines(m.w, g.statusY))
		return
	}
	// header
	if g.top < g.msgTop {
		m.drawHeader(c, g)
	}
	if g.hud >= 0 {
		m.drawHuddleBar(c, g)
	}
	if g.banner >= 0 {
		c.putANSI(g.CX, g.banner, m.st.noticeErr.Render(truncate(m.bannerText(), g.CW)), g.CW, fgc(th.fg), "")
	}
	h := g.msgBot - g.msgTop
	if h > 0 {
		unread := 0
		if v, ok := m.chanView(m.active); ok {
			unread = v.Unread
		}
		if g.thrFull {
			m.drawThreadFull(c, g)
		} else {
			m.viewPane(c, &m.main, "main", g.CX, g.msgTop, g.CW, h, unread)
		}
	}
	if g.noteY >= 0 && g.noteY < g.statusY {
		c.putANSI(g.CX, g.noteY, m.statusLine(g.CW), g.CW, fgc(th.fg), "")
	}
	if g.inX != g.CX {
		m.drawIdleInput(c, g)
	}
}

// drawLines copies styled lines (overlay content) into a rectangle.
func (m *Model) drawLines(c *canvas, x, y, w, h int, lines []string) {
	for i := 0; i < h && i < len(lines); i++ {
		c.putANSI(x, y+i, lines[i], w, fgc(m.st.th.fg), "")
	}
}

func (m *Model) drawHeader(c *canvas, g geo) {
	th := m.st.th
	y := g.top
	if g.thrFull {
		title := "Thread"
		c.put(g.CX, y, title, fgb(th.fg), "")
		c.put(g.CX+len(title)+1, y, m.chanTitle(m.thread.ch), fgc(th.faint), "")
		c.put(g.CX+g.CW-1, y, "✕", fgc(th.dim), "thread:close")
		return
	}
	right := []struct{ t, act string }{}
	if m.active != 0 && g.CW >= 50 {
		v, _ := m.chanView(m.active)
		if n := m.membersOf(v); n > 0 {
			right = append(right, struct{ t, act string }{"◉ " + strconv.Itoa(n), "cmd:/members"})
		}
		right = append(right, struct{ t, act string }{"⌕", "palette:"})
	}
	rw := 0
	for _, r := range right {
		rw += strWidth(r.t) + 3
	}
	lw := max(g.CW-rw, 1)
	c.putANSI(g.CX, y, m.headerLine(lw), lw, fgc(th.fg), "")
	x := g.CX + g.CW
	for _, r := range right {
		x -= strWidth(r.t)
		c.put(x, y, r.t, fgc(th.dim), r.act)
		c.tag(x, y, strWidth(r.t), 1, r.act, true, true)
		x -= 3
	}
}

func (m *Model) membersOf(v client.ChannelView) int {
	if v.Kind == proto.ChanDM {
		return 0
	}
	return len(v.Members)
}

// viewPane draws the visible window of p, bottom-anchored, with effects.
func (m *Model) viewPane(c *canvas, p *pane, key string, x, y, w, h, unread int) {
	th := m.st.th
	if h <= 0 || w <= 0 {
		return
	}
	if !p.open() {
		msg := truncate("no channel — ⌥K to open one", w)
		c.put(x+(w-strWidth(msg))/2, y+h/2, msg, fgc(th.dim), "palette:")
		return
	}
	rows, owner := m.paneLines(p, w)
	if len(rows) == 0 && p == &m.main && !m.histBusy[p.ch] && m.histDone[p.ch] {
		hint := "no messages yet — say hi · F1 help · ⌥K channels & people"
		switch v, _ := m.chanView(p.ch); {
		case v.Kind == proto.ChanDM && len(v.Members) == 1:
			hint = "your notes — only you see them; /remind lands here"
		case w < 26:
			hint = "no messages yet"
		case w < 64:
			hint = "no messages yet · F1 help"
		}
		hint = truncate(hint, w)
		c.put(x+(w-strWidth(hint))/2, y+h-1, hint, fgc(th.dim), "")
		return
	}
	total := len(rows)
	scroll := min(max(p.scroll, 0), max(total-h, 0))
	end := total - scroll
	off := 0
	if p.scroll == 0 && m.running(m.fx.slideAt, dSlide) && p == m.focusedPane() {
		off = int(math.Round(float64(m.fx.slideN) * (1 - ease(m.progress(m.fx.slideAt, dSlide)))))
	}
	old := c.setClip(rect{x - 3, y, x + w + 4, y + h})
	defer c.setClip(old)
	sel := -1
	if m.mode == modeSelect || m.mode == modeConfirmDelete || m.mode == modeEmoji && m.emo.react {
		if p == m.focus() {
			sel = p.sel
		}
	}
	hov := -1
	if hk, i, ok := m.hoverMsg(); ok && hk == key {
		hov = i
	}
	firstRow := map[int]int{} // message → its first screen row
	for i := max(end-h-off, 0); i < end; i++ {
		yy := y + h - (end - i) + off
		if yy < y || yy >= y+h {
			continue
		}
		r := rows[i]
		o := owner[i]
		if r.b == nil {
			continue
		}
		if _, ok := firstRow[o]; !ok && o >= 0 {
			firstRow[o] = yy
		}
		m.blitRow(c, r, o, p, key, x, yy, w, o >= 0 && (o == sel || o == hov), o == sel)
		if o >= 0 && o == sel {
			m.emo.ayEnd = yy
		}
	}
	for o, yy := range firstRow {
		m.drawMsgFx(c, p, o, x, yy, w)
		if o == sel || o == hov {
			m.drawToolbar(c, p, key, o, x, yy, w)
		}
		if o == sel {
			m.emo.anchored, m.emo.ax, m.emo.ay, m.emo.aw = true, x, yy, w
		}
	}
	if p == &m.main && m.histBusy[p.ch] {
		c.fill(x, y, w, 1, th.bg, "")
		c.put(x, y, truncate(" loading older messages…", w), fgc(th.dim), "")
	}
	if scroll > 0 && unread > 0 {
		pill := " ↓ " + strconv.Itoa(unread) + " new "
		px := x + w - strWidth(pill) - 2
		c.pill(px, y+h-1, pill, fgb(th.accentFg).on(th.accent).with(aMonoRev), "scroll:bottom")
	}
}

func (m *Model) focusedPane() *pane { return m.focus() }

// blitRow copies one layout row to the screen, giving its cells the
// message's actions and the selection or hover tint.
func (m *Model) blitRow(c *canvas, r prow, o int, p *pane, key string, x, y, w int, lit, sel bool) {
	th := m.st.th
	var x0 client.Message
	if o >= 0 && o < len(p.msgs) {
		x0 = p.msgs[o]
	}
	dx, fade := 0, 1.0
	if o >= 0 {
		if t, ok := m.fx.arrive[keyOf(x0.ID, x0.Local)]; ok && m.running(t, dArrive) {
			fade = ease(m.progress(t, dArrive))
			dx = int(math.Round((1 - fade) * 3))
		}
	}
	pop := 1.0
	if o >= 0 && x0.Local != 0 {
		if t, ok := m.fx.sent[x0.Local]; ok && m.running(t, dSend) {
			pop = ease(m.progress(t, dSend))
		}
	}
	idx := ""
	if o >= 0 {
		idx = msgRef(key, o, x0)
	}
	selBg := th.element
	if !sel {
		selBg = th.panel
	}
	for bx := 0; bx < r.b.w && bx < w; bx++ {
		src := r.b.at(bx, r.y)
		if src.w == 0 {
			continue
		}
		cx := x + bx + dx
		if !c.in(cx, y) || !c.in(cx+int(src.w)-1, y) {
			continue
		}
		c.clearHalf(cx, y)
		if src.w == 2 {
			c.clearHalf(cx+1, y)
		}
		dst := c.at(cx, y)
		*dst = *src
		dst.act = 0
		if a := r.b.acts[src.act]; a != "" && idx != "" {
			kind, rest, _ := strings.Cut(a, "|")
			dst.act = c.act(kind + ":" + idx + ":" + rest)
			dst.hover = true
		} else if idx != "" {
			dst.act = c.act("msg:" + idx)
		}
		if fade < 1 {
			dst.fg = mix(th.bg, dst.fg, fade)
		}
		if pop < 1 {
			if isBar(dst.s) {
				dst.fg = mix(th.fg, th.accent, pop) // the text color, so the flash shows on light themes too
			} else if dst.s != " " {
				dst.fg = mix(th.accent, dst.fg, pop)
			}
			if dst.bg == th.bg {
				dst.bg = mix(th.accentBg, th.bg, pop)
			}
		}
		if lit && dst.bg == th.bg {
			dst.bg = selBg
		}
		if sel && (bx == 0 || bx == r.b.w-1) && isBar(dst.s) {
			dst.s, dst.fg, dst.attr = "▌", th.accent, dst.attr|aBold // selected, whatever the colors
		}
		if sel && !m.st.color {
			dst.attr |= aMonoRev
		}
		if src.w == 2 {
			n := c.at(cx+1, y)
			*n = *dst
			n.s, n.w = "", 0
		}
	}
	if lit && o >= 0 && m.st.color {
		// the gap the arrival slide leaves gets the tint too
		for i := 0; i < dx; i++ {
			if c.in(x+i, y) && c.at(x+i, y).bg == th.bg {
				c.at(x+i, y).bg = selBg
			}
		}
	}
}

// drawMsgFx draws the effects that live outside a message's own cells:
// send sparkles, the sent-status flash, the floating "+1" of a reaction.
func (m *Model) drawMsgFx(c *canvas, p *pane, o, x, y, w int) {
	if m.fx.off || o < 0 || o >= len(p.msgs) {
		return
	}
	th := m.st.th
	msg := p.msgs[o]
	k := keyOf(msg.ID, msg.Local)
	if t, ok := m.fx.acked[k]; ok && m.running(t, dAck) {
		pr := m.progress(t, dAck)
		for yy := y; yy < y+12 && yy < c.clip.y1; yy++ {
			for xx := x; xx < x+w; xx++ {
				if c.in(xx, yy) && c.at(xx, yy).s == "✓" && c.actionAt(xx, yy) == c.actionAt(x+w-1, y) {
					c.at(xx, yy).fg = mix(th.accent, th.faint, pr)
				}
			}
		}
	}
	if t, ok := m.fx.sent[msg.Local]; ok && msg.Local != 0 && m.running(t, dSpark) {
		age := m.progress(t, dSpark) // 0..1 over the sparkle's life
		lx := x + w
		for xx := x; xx < x+w; xx++ {
			if c.in(xx, y) && c.at(xx, y).s != " " && c.at(xx, y).s != "│" {
				lx = xx
				break
			}
		}
		d := int(math.Round(age * 2))
		glyph := "✦"
		switch {
		case age > 0.6:
			glyph = "·"
		case age > 0.3:
			glyph = "✧"
		}
		col := func(i int) rgb {
			base := th.accent
			if i%2 == 1 {
				base = th.info
			}
			return mix(base, th.bg, age)
		}
		spots, above := []int{lx - 2 - d, x + w + 1 + d}, x+w-3-d
		if !m.ui.Split || p.root != 0 {
			// ours is on the left: around the end of its first row
			rx := x + 1
			for xx := x + w - 1; xx > x; xx-- {
				if c.in(xx, y) && c.at(xx, y).s != " " {
					rx = xx
					break
				}
			}
			spots, above = []int{rx + 2 + d, x - 2 - d}, rx-1+d
		}
		for i, px := range spots {
			if c.in(px, y) && c.at(px, y).s == " " {
				c.put(px, y, glyph, fgc(col(i)), "")
			}
		}
		if age > 0.15 && c.in(above, y-1) && c.at(above, y-1).s == " " {
			c.put(above, y-1, "·", fgc(col(1)), "")
		}
	}
	if r, ok := m.fx.react[k]; ok && m.running(r.at, dReact) {
		pr := m.progress(r.at, dReact)
		target := ""
		for yy := y; yy < c.clip.y1 && target == ""; yy++ {
			for xx := x; xx < x+w; xx++ {
				a := c.actionAt(xx, yy)
				if strings.HasPrefix(a, "react:") && strings.HasSuffix(a, ":"+r.emoji) {
					target = a
					if pr < 0.5 {
						ai := c.at(xx, yy).act
						for x2 := xx; x2 < x+w && c.at(x2, yy).act == ai; x2++ {
							cl := c.at(x2, yy)
							cl.bg = mix(th.accent, cl.bg, ease(pr*2))
							if pr < 0.33 {
								cl.fg = th.accentFg
							}
						}
					}
					fy := yy - 1 - int(pr*3)
					if c.in(xx+3, fy) && c.at(xx+2, fy).s == " " && c.at(xx+3, fy).s == " " {
						c.put(xx+2, fy, "+1", fgb(mix(th.accent, th.bg, pr)), "") // floats through gaps, never over text
					}
					break
				}
			}
		}
	}
}

// drawToolbar draws ☺ ↩ ⧉ ⋯ on a hovered or selected message.
func (m *Model) drawToolbar(c *canvas, p *pane, key string, o, x, y, w int) {
	if w < 30 || o >= len(p.msgs) {
		return
	}
	th := m.st.th
	x0 := p.msgs[o]
	if x0.ID == 0 || x0.Deleted {
		return
	}
	idx := msgRef(key, o, x0)
	tx := x + w - 13
	if x0.Author == m.me.ID && p.root == 0 && m.ui.Split {
		tx = x + 1 // ours is on the right: the toolbar goes left
	}
	for _, b := range [][2]string{{"☺", "react"}, {"↩", "reply"}, {"⧉", "copy"}, {"⋯", "more"}} {
		a := "tb:" + idx + ":" + b[1]
		tx = c.put(tx, y, " "+b[0]+" ", fgc(th.dim).on(th.element), a)
	}
}

func (m *Model) drawThreadFull(c *canvas, g geo) {
	m.viewPane(c, &m.thread, "thr", g.CX, g.msgTop, g.CW, g.msgBot-g.msgTop, 0)
}

func (m *Model) drawThreadPane(c *canvas, g geo) {
	th := m.st.th
	if g.tpW <= 0 {
		return
	}
	c.fill(g.RX, g.T0, g.tpW, g.statusY-g.T0, th.panel, "")
	old := c.setClip(rect{g.RX, g.T0, m.w, g.statusY})
	defer c.setClip(old)
	tx := g.RX + 2
	c.put(tx, g.top, "Thread", fgb(th.fg), "")
	if m.thread.open() {
		c.put(tx+7, g.top, truncate(m.chanTitle(m.thread.ch), g.TW-14), fgc(th.faint), "")
	}
	c.put(g.RX+g.TW-3, g.top, "✕", fgc(th.dim), "thread:close") // slides in with the pane
	m.drawThreadGrip(c, g)
	pw := g.TW - 4
	top := g.top + 2
	h := m.paneHeight(&m.thread)
	if m.thread.open() && h > 0 {
		m.viewPane(c, &m.thread, "thr", tx, top, pw, h, 0)
		c.each(tx, top, pw, h, func(cl *cell) {
			if cl.bg == th.bg {
				cl.bg = th.panel
			}
		})
	}
}

// drawIdleInput is the main column's input while the thread pane has the
// input: a quiet box that says how to get back.
func (m *Model) drawIdleInput(c *canvas, g geo) {
	th := m.st.th
	h := 3
	y := g.statusY - (g.statusY - g.inY - g.inH) - h
	m.box(c, g.CX, y, g.CW, h, th.line, th.bg)
	c.put(g.CX+2, y, " "+m.chanTitle(m.active)+" ", fgc(th.faint), "")
	c.put(g.CX+2, y+1, truncate("Esc closes the thread", g.CW-4), fgc(th.faint), "thread:close")
}

// box draws a frame in the skin's style: rounded, or double-lined.
func (m *Model) box(c *canvas, x, y, w, h int, col, bg rgb) {
	if w < 2 || h < 2 {
		return
	}
	b := m.skin().box
	c.fill(x+1, y+1, w-2, h-2, bg, "")
	c.put(x, y, b.tl+strings.Repeat(b.h, w-2)+b.tr, fgc(col), "")
	for j := y + 1; j < y+h-1; j++ {
		c.put(x, j, b.v, fgc(col), "")
		c.put(x+w-1, j, b.v, fgc(col), "")
	}
	c.put(x, y+h-1, b.bl+strings.Repeat(b.h, w-2)+b.br, fgc(col), "")
}

func (m *Model) drawInput(c *canvas, g geo) {
	th := m.st.th
	x, y, w, h := g.inX, g.inY, g.inW, g.inH
	if !g.boxed || y < 0 {
		// Tiny terminal: just the text rows, no frame.
		y = max(y, 0)
		for i, l := range m.inputView() {
			if i < h {
				c.putANSI(x, y+i, dropBidi(l), w, fgc(th.fg).on(th.panel), "focus:input")
			}
		}
		return
	}
	col := mix(th.bg, th.accent, 0.75)
	m.box(c, x, y, w, h, col, th.panel)
	title := m.chanTitle(m.active)
	if m.thread.open() {
		title = "reply"
	}
	if m.editing != 0 {
		title = "editing"
	}
	if w >= 20 {
		c.put(x+2, y, " "+truncate(title, w/2)+" ", fgc(th.dim), "")
		if v, ok := m.chanView(m.active); ok && !m.thread.open() && w >= 40 {
			kind, kc := " public ", th.faint
			if _, enc := m.e2e[m.active]; enc {
				kind, kc = " ◆ e2e ", th.info
			} else if v.Kind != proto.ChanPublic {
				kind = " private "
			}
			c.put(x+w-2-strWidth(kind), y, kind, fgc(kc), "")
		}
	}
	lines := m.inputView()
	for i := 0; i < h-2 && i < len(lines); i++ {
		c.putANSI(x+2, y+1+i, dropBidi(lines[i]), w-4, fgc(th.fg).on(th.panel), "focus:input")
	}
	// key hints in the bottom edge
	keys := [][3]string{{"⌥K", "commands", "palette:"}, {"⌥⏎", "newline", ""}, {"⏎", "send", "send"}}
	if m.mode == modeEmoji {
		keys = [][3]string{{"esc", "close emoji", ""}}
	} else if m.mode == modeSelect {
		keys = [][3]string{{"esc", "back", ""}}
	}
	kw := 0
	for i, k := range keys {
		kw += strWidth(k[0]) + 1 + strWidth(k[1])
		if i > 0 {
			kw += 3
		}
	}
	if kw+6 <= w {
		kx := x + w - 3 - kw
		c.put(kx-1, y+h-1, " ", fgc(th.fg), "")
		for i, k := range keys {
			if i > 0 {
				kx = c.put(kx, y+h-1, " · ", fgc(th.faint), "")
			}
			kx = c.put(kx, y+h-1, k[0], fgb(th.dim), k[2])
			kx = c.put(kx, y+h-1, " "+k[1], fgc(th.faint), k[2])
		}
		c.put(kx, y+h-1, " ", fgc(th.fg), "")
	}
}

// inputView is the input's text rows.
func (m *Model) inputView() []string {
	return strings.Split(m.input.View(), "\n")
}

// drawStatusBar is the bottom row: tc, the channel, then keys, encryption,
// connection and theme on the right.
func (m *Model) drawStatusBar(c *canvas, g geo) {
	th := m.st.th
	y := g.statusY
	c.fill(0, y, m.w, 1, th.bg, "")
	x := c.put(1, y, " tc ", fgb(th.accentFg).on(th.accent).with(aMonoRev), "palette:")
	x = c.put(x+1, y, truncate(m.chanTitle(m.active), max(m.w/2-x, 1)), fgc(th.dim), "")
	conn, cc := m.connLabel()
	type part struct {
		t   string
		fg  rgb
		act string
	}
	parts := []part{{"F1 keys", th.faint, "keys"}}
	if _, enc := m.e2e[m.active]; enc {
		parts = append(parts, part{"◆ e2e", th.info, ""})
	}
	parts = append(parts, part{"● " + conn, cc, ""}, part{"◐ " + th.name, th.dim, "palette:settings"})
	if m.newRelease != "" {
		parts = append([]part{{"↑ " + m.newRelease, th.info, "settings:about"}}, parts...)
	}
	// Too narrow: drop the least useful first; the connection and a way
	// to the keys stay longest.
	fits := func() bool {
		w := 0
		for _, p := range parts {
			w += strWidth(p.t) + 3
		}
		return x+2+w <= m.w
	}
	for _, drop := range []string{"◐", "↑", "◆", "F1 keys"} {
		if fits() {
			break
		}
		for i, p := range parts {
			if strings.HasPrefix(p.t, drop) {
				if drop == "F1 keys" {
					parts[i].t = "F1" // shortened, not dropped
				} else {
					parts = append(parts[:i], parts[i+1:]...)
				}
				break
			}
		}
	}
	for !fits() && len(parts) > 0 {
		parts = parts[1:]
	}
	rx := m.w - 2
	for i := len(parts) - 1; i >= 0; i-- {
		p := parts[i]
		rx -= strWidth(p.t)
		c.put(rx, y, p.t, fgc(p.fg), p.act)
		rx -= 3
	}
}

// connLabel is the connection state for the status bar.
func (m *Model) connLabel() (string, rgb) {
	th := m.st.th
	if m.status.Stopped {
		return "stopped", th.err
	}
	switch m.status.State {
	case client.Online:
		if m.status.Transport != "" {
			return m.status.Transport, th.ok
		}
		return "online", th.ok
	case client.Syncing:
		return "syncing", th.warn
	}
	return strings.ToLower(m.statusText()), th.warn
}

// dots is the typing indicator's animation.
func (m *Model) dots() string {
	if m.fx.off {
		return "···"
	}
	return [...]string{"•··", "·•·", "··•"}[(m.now().UnixMilli()/400)%3]
}

// msgRef names message o of a pane in a mouse action: "main:3~i500". The
// index finds it fast; the id (or outbox id) checks it is still the same
// message when the click arrives.
func msgRef(key string, o int, x client.Message) string {
	ref := "i" + strconv.FormatUint(x.ID, 10)
	if x.ID == 0 {
		ref = "l" + strconv.FormatInt(x.Local, 10)
	}
	return key + ":" + strconv.Itoa(o) + "~" + ref
}

// findRef is the index in msgs that ref ("3~i500") names, or -1 when that
// message is gone.
func findRef(msgs []client.Message, ref string) int {
	is, id, _ := strings.Cut(ref, "~")
	i, err := strconv.Atoi(is)
	if err != nil {
		return -1
	}
	match := func(x client.Message) bool {
		if x.ID == 0 {
			return id == "l"+strconv.FormatInt(x.Local, 10)
		}
		return id == "i"+strconv.FormatUint(x.ID, 10)
	}
	if i >= 0 && i < len(msgs) && match(msgs[i]) {
		return i
	}
	for j, x := range msgs {
		if match(x) {
			return j
		}
	}
	return -1
}

func isBar(s string) bool { return s == "│" || s == "┃" || s == "┇" }
