package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Messages as "thin bar, split sides": other people on the left under their
// name, a bar in their color; ours on the right with an accent bar and the
// time and send status under the text. Each message is drawn once into its
// own small canvas (a block), cached, and copied into the frame.

type concern uint8

const (
	cNone concern = iota
	cMention
	cHighlight
)

type blockKey struct {
	renderKey
	th      *theme
	left    bool // thread pane: everyone on the left
	split   bool // Settings › Messages: Split (ours on the right)
	tail    bool // last of its group: ours shows time and status
	concern concern
	appr    apprState // an agent's approval card (approval.go)
	fold    string    // an agent run's folded steps line (agentlive.go)
}

// blockCache memoizes message blocks in two generations: when the current
// one fills up it becomes the old one, and blocks still in use move back,
// so a busy channel never loses its whole working set at once. Reset when
// names or the theme change.
type blockCache struct{ cur, old map[blockKey]*canvas }

func (c *blockCache) reset() { c.cur, c.old = nil, nil }

func (c *blockCache) get(k blockKey, draw func() *canvas) *canvas {
	if b, ok := c.cur[k]; ok {
		return b
	}
	b, ok := c.old[k]
	if !ok {
		b = draw()
	}
	if c.cur == nil || len(c.cur) >= maxBlocks {
		c.old, c.cur = c.cur, make(map[blockKey]*canvas, maxBlocks/4)
	}
	c.cur[k] = b
	return b
}

// A message draws at most this many rows in the log; v shows all of it.
// A code block shows at most maxCodeRows of its lines.
const (
	maxBlockRows = 120
	maxCodeRows  = 40
	footerRows   = 3    // reaction rows under one message
	maxBlocks    = 1500 // cached blocks: a few screens' worth across channels
)

// bodyW is the widest a message's text gets in a column of width cw.
func bodyW(cw int) int {
	if cw < 40 {
		return max(cw-3, 1)
	}
	return min(64, cw-6)
}

// grower is a canvas that grows by rows as a block is drawn.
type grower struct {
	c *canvas
	n int
}

func newGrower(w int, th *theme) *grower { return &grower{c: newCanvas(w, 8, th)} }

// full: the block reached its row limit; the rest is cut.
func (g *grower) full() bool { return g.n >= maxBlockRows }

func (g *grower) row() int {
	if g.n == g.c.h {
		n := newCanvas(g.c.w, g.c.h*2, g.c.th)
		copy(n.cells, g.c.cells)
		n.acts, n.actIx = g.c.acts, g.c.actIx
		g.c = n
	}
	g.n++
	return g.n - 1
}

func (g *grower) done() *canvas {
	c := g.c
	c.h = g.n
	c.cells = c.cells[:g.n*c.w]
	c.clip = rect{0, 0, c.w, c.h}
	return c
}

func (m *Model) concernOf(x client.Message) concern {
	if m.ownMsg(x) || x.Deleted || m.users[x.Author].Bot {
		return cNone
	}
	switch {
	case client.Mentioned(outsideCode(x.Body), m.mentionAs):
		return cMention
	case client.Highlighted(x.Body, m.highlights):
		return cHighlight
	}
	return cNone
}

// viaMark marks a message an outside Claude session posted for its author
// (tc mcp, proto.ViaMCP).
const viaMark = "⇢"

// ownMsg: a message I sent myself (not one my agent sent as my device).
func (m *Model) ownMsg(x client.Message) bool { return x.Author == m.me.ID && !x.FromAgent() }

// msgBlock draws one message at column width cw.
func (m *Model) msgBlock(x client.Message, cw int, header, tail, left bool) *canvas {
	k := m.blockKeyOf(x, cw, header, tail, left)
	return m.msgBlockFrom(nil, k, x)
}

// msgBlockFrom draws the message k names, first looking in prev (a pane's
// last layout).
func (m *Model) msgBlockFrom(prev map[blockKey]*canvas, k blockKey, x client.Message) *canvas {
	own := m.ownMsg(x)
	mine := own && !k.left && m.ui.Split // drawn on the right
	if b, ok := prev[k]; ok {
		return b
	}
	return m.blocks.get(k, func() *canvas { return m.drawBlock(x, k, mine, own) })
}

func (m *Model) blockKeyOf(x client.Message, cw int, header, tail, left bool) blockKey {
	return blockKey{renderKey: keyFor(x, cw, header), th: m.st.th, left: left, split: m.ui.Split, tail: tail, concern: m.concernOf(x)}
}

// drawBlock draws one message. mine: ours on the right (split layout);
// own: ours, wherever it goes.
func (m *Model) drawBlock(x client.Message, k blockKey, mine, own bool) *canvas {
	cw, header, tail, cc := k.width, k.header, k.tail, k.concern
	th := m.st.th
	agent := x.FromAgent()
	g := newGrower(cw, th)
	c := func() *canvas { return g.c }
	bw := bodyW(cw)
	// at is where a line of width lw starts: after the bar on the left, or
	// ending two cells before our bar on the right.
	at := func(lw int) int {
		if mine {
			return max(cw-2-lw, 1)
		}
		return 2
	}
	textFg := th.fg
	if x.Pending {
		textFg = th.dim
	}
	first := 0
	if header && agent {
		m.agentHeader(g, x, cw)
		first = 1
	} else if header && !mine {
		y := g.row()
		name := m.r.userName(x.Author)
		e := 2
		if x.Via == proto.ViaMCP {
			e = c().put(e, y, viaMark+" ", fgc(th.faint), "") // posted for them by an outside Claude session
		}
		e = c().put(e, y, name, fgb(th.nick(x.Author)), "")
		if m.users[x.Author].Bot {
			e = c().put(e, y, botTag, fgc(th.faint), "")
		}
		if !own { // ours shows its time and status under the text
			ts := time.UnixMilli(x.ShownTime()).Local().Format("15:04")
			c().put(e+2, y, ts, fgc(th.faint), "")
		}
		first = 1
	}
	line := func(text string, st cstyle) {
		text = truncate(text, bw)
		y := g.row()
		c().put(at(strWidth(text)), y, text, st, "")
	}
	switch {
	case x.Deleted:
		line(deletedText(x), fgc(th.faint).with(aItalic))
	default:
		switch x.Sec {
		case client.SecWaiting:
			line(m.waitingText(x), fgc(th.dim))
		case client.SecInvalid:
			line("⚠ invalid encrypted message (hidden)", fgb(th.err))
		case client.SecInserted:
			line("⚠ inserted by server", fgb(th.err))
		case client.SecLegacy:
			line("⚠ unauthenticated (legacy)", fgb(th.err))
		case client.SecLegacyClaimed:
			line("⚠ unauthenticated (legacy, server-claimed)", fgb(th.err))
		}
		if x.Sec != client.SecWaiting && x.Sec != client.SecInvalid {
			if card, ok := parseApproval(x); ok && agent {
				m.drawApproval(g, x, card, k.appr, bw, at, textFg)
			} else {
				m.drawText(g, x, bw, at, textFg)
				if agent {
					m.milestoneMark(g, first)
				}
			}
		}
	}
	if g.full() {
		y := g.row()
		cut := truncate("… v views all of it", bw)
		g.c.put(at(strWidth(cut)), y, cut, fgc(th.faint), "")
	}
	if k.fold != "" && !x.Deleted {
		m.drawFold(g, x, k.fold, bw)
	}
	if !x.Deleted {
		m.drawFooter(g, x, cw, bw, mine)
	}
	if x.Failed != "" {
		f := "! failed — r to retry, d to discard"
		why := " (" + sanitize.Line(x.Failed) + ")"
		full := truncate(f+why, bw)
		y := g.row()
		e := c().put(at(strWidth(full)), y, truncate(f, bw), fgb(th.err), "")
		if strWidth(f) < bw {
			c().put(e, y, truncate(why, bw-strWidth(f)), fgc(th.dim), "")
		}
	}
	barEnd := g.n
	if own && (tail || x.Pending) && x.Failed == "" {
		y := g.row()
		st := "✓"
		if x.Pending {
			st = "◌"
		}
		meta := time.UnixMilli(x.ShownTime()).Local().Format("15:04") + " " + st
		if x.Via == proto.ViaMCP && mine {
			meta = viaMark + " " + meta // no name row on the right: the mark goes here
		}
		c().put(at(strWidth(meta)), y, meta, fgc(th.faint), "")
		if !mine {
			barEnd = g.n // on the left the bar runs down to it
		}
	}
	if g.n == first {
		g.row()
		barEnd = g.n
	}
	out := g.done()
	bar := mix(th.bg, th.nick(x.Author), 0.9)
	bx, glyph := 0, "│"
	if agent {
		bar, glyph = mix(th.bg, m.agentColor(m.agentOfMsg(x)), 0.9), agentBar
	}
	switch {
	case mine:
		bar, bx = th.accent, cw-1
	case cc == cMention && agent:
		bar = th.accent // the dashed bar stays: it says "agent" without color
	case cc == cMention:
		bar, glyph = th.accent, "┃" // a heavier bar: still a mark without color
	case cc == cHighlight && !agent:
		bar, glyph = th.warn, "┇"
	}
	for y := 0; y < barEnd; y++ {
		out.put(bx, y, glyph, fgc(bar), "")
	}
	if cc == cMention {
		tint := mix(th.bg, th.accent, 0.06)
		out.each(1, 0, cw-1, out.h, func(cl *cell) {
			if cl.bg == th.bg {
				cl.bg = tint
			}
		})
	}
	return out
}

// drawText draws the body: markdown-lite text and fenced code blocks.
func (m *Model) drawText(g *grower, x client.Message, bw int, at func(int) int, textFg rgb) {
	th := m.st.th
	text := sanitize.Text(x.Body)
	lastY, lastEnd, lastCode := -1, 0, false
	for _, sg := range splitFences(strings.Split(text, "\n")) {
		if sg.code {
			if len(sg.lines) > 0 && !g.full() {
				m.drawCode(g, sg, bw, at, x.Pending)
				lastCode = true
			}
			continue
		}
		for _, ln := range sg.lines {
			if g.full() {
				break
			}
			for _, l := range wrapSpans(parseInline(ln, m.me.Name), bw) {
				if g.full() {
					break
				}
				w := 0
				for _, sp := range l {
					w += strWidth(sp.text)
				}
				y := g.row()
				xx := at(w)
				for _, sp := range l {
					f, act := sp.f, ""
					if f&fChannel != 0 {
						// only a channel that exists is a link; it opens on a click
						if id := m.chanNamed(sp.text[1:]); id != 0 {
							act = "chanlink|" + strconv.FormatUint(id, 10)
						} else {
							f &^= fChannel
						}
					}
					xx = g.c.put(xx, y, sp.text, m.spanStyle(f, textFg), act)
				}
				lastY, lastEnd, lastCode = y, xx, false
			}
		}
	}
	if x.Edited == 0 {
		return
	}
	const ed = "(edited)"
	if lastY >= 0 && !lastCode && lastY == g.n-1 && lastEnd-at(0)+1+len(ed) <= bw && at(0) == 2 {
		g.c.put(lastEnd+1, lastY, ed, fgc(th.faint), "")
		return
	}
	y := g.row()
	g.c.put(at(len(ed)), y, ed, fgc(th.faint), "")
}

func (m *Model) spanStyle(f flag, base rgb) cstyle {
	th := m.st.th
	st := fgc(base)
	switch {
	case f&fMention != 0:
		st = fgb(th.accent)
		if m.touches() {
			st = st.under(aCurly, th.accent)
		}
	case f&fChannel != 0:
		st = fgc(th.info)
		if m.touches() {
			st = st.under(aDotted, th.info)
		}
	case f&fURL != 0:
		st = fgc(th.info).with(aUnderline)
	case f&fCode != 0:
		st = fgc(th.warn)
	}
	if f&fBold != 0 {
		st.attr |= aBold
	}
	if f&fItalic != 0 {
		st.attr |= aItalic
	}
	return st
}

// drawCode draws a fenced block as a panel with a header row: its name, the
// diff's +/− counts, and a copy button.
func (m *Model) drawCode(g *grower, sg seg, bw int, at func(int) int, pending bool) {
	th := m.st.th
	w := min(bw, g.c.w-4)
	if w < 6 {
		w = max(g.c.w-2, 1)
	}
	x0 := at(w)
	var ks []diffKind
	adds, dels := 0, 0
	if sg.diff {
		ks = classifyDiff(sg.lines)
		for _, k := range ks {
			switch k {
			case dAdd:
				adds++
			case dDel:
				dels++
			}
		}
	}
	name := sg.info
	if name == "" {
		name = "code"
		if sg.diff {
			name = "diff"
		}
	}
	y := g.row()
	g.c.fill(x0, y, w, 1, th.element, "")
	ic := m.icon(icCode)
	if sg.diff {
		ic = m.icon(icFile)
	}
	e := x0 + 1
	if w >= 16 {
		e = g.c.put(e, y, ic, fgc(th.faint), "") + 1
	}
	e = g.c.put(e, y, truncate(name, max(x0+w-8-e, 1)), fgc(th.dim), "")
	if sg.diff && w >= 24 {
		e = g.c.put(e+2, y, "+"+strconv.Itoa(adds), fgc(th.add), "")
		g.c.put(e+1, y, "−"+strconv.Itoa(dels), fgc(th.del), "")
	}
	if w >= 12 {
		g.c.put(x0+w-5, y, "copy", fgc(th.faint), "copy|")
	}
	for i, ln := range sg.lines {
		if i == maxCodeRows || g.full() {
			y := g.row()
			g.c.fill(x0, y, w, 1, th.panel, "")
			more := fmt.Sprintf("… %d more lines · v views all", len(sg.lines)-i)
			g.c.put(x0+1, y, truncate(more, max(w-2, 1)), fgc(th.faint), "")
			break
		}
		bg, fg := th.panel, th.fg
		if sg.diff {
			switch ks[i] {
			case dAdd:
				bg, fg = th.addBg, th.add
			case dDel:
				bg, fg = th.delBg, th.del
			case dHunk:
				fg = th.hunk
			case dHdr:
				fg = th.dim
			}
		}
		if pending {
			fg = mix(bg, fg, 0.6)
		}
		y := g.row()
		g.c.fill(x0, y, w, 1, bg, "")
		g.c.put(x0+1, y, truncate(ln, max(w-2, 1)), fgc(fg), "")
	}
}

// drawFooter draws reaction pills and the reply count, wrapping onto at
// most footerRows rows.
func (m *Model) drawFooter(g *grower, x client.Message, cw, bw int, mine bool) {
	th := m.st.th
	type part struct {
		text string
		st   cstyle
		act  string
		pill bool
	}
	var parts []part
	keys := make([]string, 0, len(x.Reactions))
	for k, us := range x.Reactions {
		if len(us) > 0 && sanitize.Line(k) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		us := x.Reactions[k]
		own := false
		for _, u := range us {
			own = own || u == m.me.ID
		}
		st := fgc(th.dim).on(th.element)
		if own {
			st = fgb(th.accent).on(th.accentBg).with(aMonoRev)
		}
		parts = append(parts, part{" " + sanitize.Line(k) + " " + strconv.Itoa(len(us)) + " ", st, "react|" + k, true})
	}
	if len(parts) > 0 && x.ID != 0 && !x.Deleted {
		parts = append(parts, part{" " + m.icon(icReact) + "+ ", fgc(th.dim).on(th.element), "addreact|", true})
	}
	var tail []part
	if x.Replies > 0 {
		tail = append(tail, part{fmt.Sprintf("%s %d %s", m.icon(icThread), x.Replies, plural(x.Replies, "reply", "replies")), fgc(th.info), "thread|", false})
	}
	if x.Pinned {
		tail = append(tail, part{m.icon(icPin), fgc(th.dim), "", false})
	}
	if x.TimeDrift() {
		tail = append(tail, part{driftMark, fgc(th.warn), "", false})
	}
	if mine {
		parts = append(tail, parts...)
	} else {
		parts = append(parts, tail...)
	}
	if len(parts) == 0 {
		return
	}
	width := func(p part) int {
		if p.pill {
			return strWidth(p.text) + 2
		}
		return strWidth(p.text)
	}
	// Parts that don't fit the row wrap onto the next, like Slack's.
	room := max(bw, 1)
	rows := func(ps []part) int {
		n, total := 1, 0
		for i, p := range ps {
			add := width(p)
			if i > 0 && total > 0 {
				add += 2
			}
			if total > 0 && total+add > room {
				n, total = n+1, width(p)
				continue
			}
			total += add
		}
		return n
	}
	// Too many reactions: keep the reply count and ☺+, fold the rest into +N.
	var react []int
	for i, p := range parts {
		if strings.HasPrefix(p.act, "react|") {
			react = append(react, i)
		}
	}
	for hidden := 1; len(react) > 0 && rows(parts) > footerRows && hidden <= len(react); hidden++ {
		var kept []part
		kept = append(kept, parts[:react[0]]...)
		for _, i := range react[:len(react)-hidden] {
			kept = append(kept, parts[i])
		}
		kept = append(kept, part{"+" + strconv.Itoa(hidden), fgc(th.faint), "", false})
		kept = append(kept, parts[react[len(react)-1]+1:]...)
		if rows(kept) <= footerRows || hidden == len(react) {
			parts = kept
			break
		}
	}
	for len(parts) > 0 {
		n, total := 0, 0
		for n < len(parts) {
			add := width(parts[n])
			if n > 0 {
				add += 2
			}
			if n > 0 && total+add > room {
				break
			}
			total += add
			n++
		}
		xx := 2
		if mine {
			xx = max(cw-2-min(total, bw), 1)
		}
		y := g.row()
		for i, p := range parts[:n] {
			if i > 0 {
				xx += 2
			}
			if p.pill {
				xx = g.c.pill(xx, y, p.text, p.st, p.act)
			} else {
				xx = g.c.put(xx, y, p.text, p.st, p.act)
			}
		}
		parts = parts[n:]
	}
}

// sepBlock is a centered divider row: a day ("Today") or "replies".
func (m *Model) sepBlock(label string, cw int) *canvas {
	k := blockKey{renderKey: renderKey{failed: "sep:" + label, width: cw}, th: m.st.th}
	return m.blocks.get(k, func() *canvas {
		th := m.st.th
		c := newCanvas(cw, 1, th)
		lab := " " + truncate(label, max(cw-2, 1)) + " "
		lw := strWidth(lab)
		half := max((cw-lw)/2, 0)
		c.put(0, 0, strings.Repeat("─", half), fgc(th.line), "")
		c.put(half, 0, lab, fgc(th.faint), "")
		c.put(half+lw, 0, strings.Repeat("─", max(cw-half-lw, 0)), fgc(th.line), "")
		return c
	})
}

func dayLabel(t time.Time, now time.Time) string {
	switch {
	case sameDay(t.UnixMilli(), now.UnixMilli()):
		return "Today"
	case sameDay(t.UnixMilli(), now.AddDate(0, 0, -1).UnixMilli()):
		return "Yesterday"
	case t.Year() != now.Year():
		return t.Format("Mon 2 Jan 2006")
	}
	return t.Format("Mon 2 Jan")
}

// prow is one row of a pane's layout: row y of block b (nil: blank).
type prow struct {
	b *canvas
	y int
}

// text is the row's characters, for tests and copy.
func (r prow) text() string {
	if r.b == nil {
		return ""
	}
	var s strings.Builder
	for x := 0; x < r.b.w; x++ {
		s.WriteString(r.b.at(x, r.y).s)
	}
	return strings.TrimRight(s.String(), " ")
}

// paneLines lays out every message of p at column width w and says which
// message owns each row (-1 for separators and gaps). Memoized: it only
// re-runs after a reload, resize, theme or density change.
func (m *Model) paneLines(p *pane, w int) ([]prow, []int) {
	left := p.root != 0
	live := m.liveShown(p)
	sig := liveSig(live)
	if p.memoLines != nil && p.memoVer == p.ver && p.memoW == w && p.memoCompact == m.ui.Compact && p.memoDay == m.today() {
		if p.memoLive != sig {
			// only the live rows changed: keep the messages' layout
			n := p.memoBase
			p.memoLines, p.memoOwner = m.liveLines(p, live, w, p.memoLines[:n:n], p.memoOwner[:n:n])
			p.memoLive = sig
			p.keepView(len(p.memoLines) - n)
		}
		return p.memoLines, p.memoOwner
	}
	appr := m.approvalStates(p.msgs)
	var rows []prow
	var owner []int
	now := m.now()
	// Blocks of the last layout are reused directly: a new message or one
	// changed reaction redraws only what changed, however long the list.
	prev := p.memoBlocks
	if p.memoW != w {
		prev = nil
	}
	used := make(map[blockKey]*canvas, len(p.msgs))
	for i, x := range p.msgs {
		sep := i == 0 || !sameDay(p.msgs[i-1].Time, x.Time)
		header := sep || !grouped(p.msgs[i-1], x)
		// Cozy: exactly one blank row between groups and around a
		// divider, none inside a group. Compact: none at all.
		if i > 0 && (sep || header) && !m.ui.Compact {
			rows = append(rows, prow{})
			owner = append(owner, -1)
		}
		if sep {
			rows = append(rows, prow{b: m.sepBlock(dayLabel(time.UnixMilli(x.Time).Local(), now), w)})
			owner = append(owner, -1)
			if !m.ui.Compact {
				rows = append(rows, prow{})
				owner = append(owner, -1)
			}
		}
		tail := i == len(p.msgs)-1 || !sameDay(x.Time, p.msgs[i+1].Time) || !grouped(x, p.msgs[i+1])
		k := m.blockKeyOf(x, w, header, tail, left)
		if x.FromAgent() {
			k.appr, k.fold = appr[x.ID], m.foldKey(x)
		}
		b := m.msgBlockFrom(prev, k, x)
		used[k] = b
		for y := 0; y < b.h; y++ {
			rows = append(rows, prow{b: b, y: y})
			owner = append(owner, i)
		}
		if p.root != 0 && i == 0 && len(p.msgs) > 1 {
			if !m.ui.Compact {
				rows = append(rows, prow{})
				owner = append(owner, -1)
			}
			n := len(p.msgs) - 1
			rows = append(rows, prow{b: m.sepBlock(fmt.Sprintf("%d %s", n, plural(n, "reply", "replies")), w)})
			owner = append(owner, -1)
		}
	}
	p.memoBase = len(rows)
	rows, owner = m.liveLines(p, live, w, rows, owner)
	p.keepView(len(rows) - p.memoBase)
	if rows == nil {
		rows, owner = []prow{}, []int{}
	}
	p.memoLines, p.memoOwner, p.memoVer, p.memoW, p.memoBlocks, p.memoLive = rows, owner, p.ver, w, used, sig
	p.memoCompact, p.memoDay = m.ui.Compact, m.today()
	return rows, owner
}

func (m *Model) today() int { return m.now().YearDay() }
