package tui

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/sanitize"
)

// The emoji picker: a floating panel with a search box, category tabs, a
// grid and a preview row. In select mode (+, the ☺ toolbar button, the ☺+
// pill) it sits by the selected message and reacts to it; from the input
// (⌥E) it sits above the input box and inserts at the cursor.

const (
	pickCols    = 10 // emoji per grid row at full width
	pickGrid    = 8  // grid rows at full height
	pickMaxQ    = 32 // runes in the search box
	pickSecFreq = -1 // section of the frequently used row
	pickSecHits = -2 // section of search results
)

type pickItem struct {
	e   string
	def int // emojiTable row, -1 for a literal emoji typed into the search
	sec int // emojiCats index, pickSecFreq or pickSecHits
}

// pickRow is one grid row: a section title, or items[from:to].
type pickRow struct {
	sec      int
	title    bool
	from, to int
}

type emojiPicker struct {
	open   bool
	react  bool // reacting to m.target; otherwise inserting into the input
	back   mode // the mode Esc returns to
	query  string
	all    []pickItem // every item for the query
	items  []pickItem // all, with the frequently used row cut to two grid rows
	rows   []pickRow
	cols   int // rows are laid out for this many columns
	sel    int // index into items
	top    int // first grid row on screen
	at     time.Time
	shutAt time.Time // closed: it fades out from here

	// Where the selected message's first row was drawn in the last frame.
	anchored   bool
	ax, ay, aw int
	ayEnd      int // its last row
}

// pickBox is the picker's geometry for the current screen.
type pickBox struct {
	r       rect
	cols    int
	grid    int  // grid rows on screen
	framed  bool // rounded frame with title and hints
	tabs    bool // the category row and its divider
	preview bool // the divider and preview row at the bottom
}

// pickSize sizes the panel; r has the size only, pickPlace positions it.
func (m *Model) pickSize(g geo) pickBox {
	bottom := g.inY - 1
	if !g.boxed {
		bottom = g.inY
	}
	availW := m.w - g.L // never under the sidebar
	if !m.emo.react {
		availW = g.inW
	}
	availH := bottom - g.top
	var b pickBox
	if availW < 4 || availH < 1 {
		return b
	}
	b.cols = min(pickCols, (availW-6)/4)
	w := 4*b.cols + 6
	b.framed = b.cols >= 1 && availH >= 4
	if !b.framed {
		b.cols = max(min(pickCols, availW/4), 1)
		w = 4 * b.cols
		b.grid = availH - 1
		b.r = rect{0, 0, w, availH}
		return b
	}
	h := 0
	switch {
	case availH >= 9:
		b.tabs, b.preview = true, true
		b.grid = min(pickGrid, availH-7)
		h = b.grid + 7
	default:
		b.grid = min(pickGrid, availH-3)
		h = b.grid + 3
	}
	b.r = rect{0, 0, w, h}
	return b
}

// pickPlace puts the panel by the selected message (react) or above the
// input box's left edge (insert), always inside [g.top, the input).
func (m *Model) pickPlace(g geo, b pickBox) rect {
	bottom := g.inY - 1
	if !g.boxed {
		bottom = g.inY
	}
	w, h := b.r.x1, b.r.y1
	x, y := g.inX, bottom-h
	if m.emo.react {
		x, y = m.w-w, bottom-h
		if e := &m.emo; e.anchored {
			// Below the message, else above it, so it stays readable;
			// over its body (under the name row) only when neither fits.
			x = e.ax + e.aw - w
			switch {
			case e.ayEnd+1+h <= bottom:
				y = e.ayEnd + 1
			case e.ay-h >= g.top:
				y = e.ay - h
			default:
				y = e.ay + 1
			}
		}
		x = min(max(x, g.L), m.w-w)
	}
	y = min(max(y, g.top), bottom-h)
	return rect{x, y, x + w, y + h}
}

// openPicker opens the picker: react to the selected message, or insert.
func (m *Model) openPicker(react bool) {
	back := modeNormal
	if react {
		sel, ok := m.selected()
		if !ok || sel.Deleted {
			return
		}
		if sel.ID == 0 {
			m.setInfo("not sent yet — react once it is")
			return
		}
		m.target = sel
		back = modeSelect
	} else {
		m.hideComp()
	}
	m.emo = emojiPicker{open: true, react: react, back: back, at: m.now()}
	m.mode = modeEmoji
	m.fillPicker()
}

// closePicker goes back to the mode the picker came from; fade lets it
// fade out (Esc, a pick) rather than vanish (a channel switch).
func (m *Model) closePicker(fade bool) {
	if !m.emo.open {
		return
	}
	m.emo.open = false
	m.emo.shutAt = time.Time{}
	if fade {
		m.emo.shutAt = m.now()
	}
	if m.mode == modeEmoji {
		m.mode = m.emo.back
	}
	if m.mode == modeSelect {
		if _, ok := m.selected(); !ok {
			m.leaveSelect()
		}
	}
}

// fillPicker rebuilds the items for the query: every category with the
// frequently used row first, or the search results.
func (m *Model) fillPicker() {
	e := &m.emo
	e.all = e.all[:0]
	freq := m.freqList()
	if q := strings.ToLower(strings.TrimSpace(e.query)); q != "" {
		if lit, ok := resolveEmoji(q); ok && gridSafe(lit) && emojiDefOf(lit) < 0 && !isASCII(q) {
			e.all = append(e.all, pickItem{e: lit, def: -1, sec: pickSecHits})
		}
		for _, h := range searchEmoji(q, freq, true, 0) {
			e.all = append(e.all, pickItem{e: emojiTable[h.def].e, def: h.def, sec: pickSecHits})
		}
	} else {
		for _, f := range freq {
			if gridSafe(f) {
				e.all = append(e.all, pickItem{e: f, def: emojiDefOf(f), sec: pickSecFreq})
			}
		}
		for i, d := range emojiTable {
			e.all = append(e.all, pickItem{e: d.e, def: i, sec: int(d.cat)})
		}
	}
	e.sel, e.top, e.cols, e.rows, e.items = 0, 0, 0, nil, e.items[:0]
	m.hover = "" // its index named another emoji
	m.pickLayout(max(m.pickSize(m.geo()).cols, 1))
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// pickLayout lays the items out in cols columns: a title row per section,
// then its emoji. The frequently used row shows at most two grid rows. A
// new column count (a resize) keeps the same emoji selected.
func (m *Model) pickLayout(cols int) {
	e := &m.emo
	if e.cols == cols && e.rows != nil {
		return
	}
	var keep pickItem
	if e.sel >= 0 && e.sel < len(e.items) {
		keep = e.items[e.sel]
	}
	e.cols, e.rows, e.items = cols, e.rows[:0], e.items[:0]
	nfreq := 0
	for _, it := range e.all {
		if it.sec == pickSecFreq {
			if nfreq++; nfreq > 2*cols {
				continue
			}
		}
		if n := len(e.items); n == 0 || e.items[n-1].sec != it.sec {
			e.rows = append(e.rows, pickRow{sec: it.sec, title: true, from: n, to: n})
		}
		e.items = append(e.items, it)
	}
	if len(e.items) == 0 {
		e.rows = append(e.rows, pickRow{sec: pickSecHits, title: true})
	}
	// Split each section's run of items into grid rows.
	var rows []pickRow
	for i, t := range e.rows {
		end := len(e.items)
		if i+1 < len(e.rows) {
			end = e.rows[i+1].from
		}
		rows = append(rows, t)
		for k := t.from; k < end; k += cols {
			rows = append(rows, pickRow{sec: t.sec, from: k, to: min(k+cols, end)})
		}
	}
	e.rows = rows
	if keep.e != "" {
		if i := slices.Index(e.items, keep); i >= 0 {
			e.sel = i
		}
	}
	e.sel = min(e.sel, max(len(e.items)-1, 0))
}

// pickRowOf is the grid row holding item i, or -1.
func (m *Model) pickRowOf(i int) int {
	for r, row := range m.emo.rows {
		if !row.title && i >= row.from && i < row.to {
			return r
		}
	}
	return -1
}

// pickShow scrolls so the selection is on screen, with its section's title
// when the selection is in the section's first row.
func (m *Model) pickShow(grid int) {
	e := &m.emo
	r := m.pickRowOf(e.sel)
	if r < 0 || grid <= 0 {
		return
	}
	first := r
	if r > 0 && e.rows[r-1].title {
		first = r - 1
	}
	if first < e.top {
		e.top = first
	}
	if r >= e.top+grid {
		e.top = r - grid + 1
	}
	e.top = min(max(e.top, 0), max(len(e.rows)-grid, 0))
}

// pickSection is the section the selection is in.
func (m *Model) pickSection() int {
	if e := &m.emo; e.sel >= 0 && e.sel < len(e.items) {
		return e.items[e.sel].sec
	}
	return pickSecHits
}

// pickTabs are the tab strip's sections: frequently used, then each
// category.
func pickTabs() []int {
	out := []int{pickSecFreq}
	for i := range emojiCats {
		out = append(out, i)
	}
	return out
}

// pickJump selects the first emoji of section sec and scrolls its title to
// the top; a search is cleared first.
func (m *Model) pickJump(sec int, b pickBox) {
	e := &m.emo
	if e.query != "" {
		e.query = ""
		m.fillPicker()
		m.pickLayout(b.cols)
	}
	for r, row := range e.rows {
		if row.title && row.sec == sec {
			e.sel, e.top = row.from, r
			e.top = min(e.top, max(len(e.rows)-b.grid, 0))
			return
		}
	}
}

// emojiKey handles a key while the picker is open.
func (m *Model) emojiKey(k tea.KeyMsg) tea.Cmd {
	e := &m.emo
	b := m.pickSize(m.geo())
	m.pickLayout(max(b.cols, 1))
	key := keyName(k)
	move := func(r int) { // to grid row r, same column where it can
		if r < 0 || r >= len(e.rows) {
			return
		}
		cur := m.pickRowOf(e.sel)
		col := 0
		if cur >= 0 {
			col = e.sel - e.rows[cur].from
		}
		e.sel = e.rows[r].from + min(col, e.rows[r].to-e.rows[r].from-1)
	}
	step := func(dir int) { // the next emoji row up (-1) or down (1)
		for r := m.pickRowOf(e.sel) + dir; r >= 0 && r < len(e.rows); r += dir {
			if !e.rows[r].title {
				move(r)
				return
			}
		}
	}
	switch key {
	case kEsc, kEmoji:
		m.closePicker(true)
		return nil
	case kEnter:
		return m.pickEmoji(e.sel)
	case kLeft:
		e.sel = max(e.sel-1, 0)
	case kRight:
		e.sel = min(e.sel+1, max(len(e.items)-1, 0))
	case kUp, "ctrl+p":
		step(-1)
	case kDown, "ctrl+n":
		step(1)
	case kPgUp, kPgDown:
		dir := 1
		if key == kPgUp {
			dir = -1
		}
		for range max(b.grid-1, 1) {
			step(dir)
		}
	case kTab, kShiftTab:
		tabs := pickTabs()
		i := slices.Index(tabs, m.pickSection())
		switch {
		case i < 0 && key == kTab:
			i = 0
		case i < 0:
			i = len(tabs) - 1
		case key == kTab:
			i = (i + 1) % len(tabs)
		default:
			i = (i - 1 + len(tabs)) % len(tabs)
		}
		m.pickJump(tabs[i], b)
		return nil
	case "backspace", "ctrl+h":
		if e.query != "" {
			_, n := utf8.DecodeLastRuneInString(e.query)
			m.setPickQuery(e.query[:len(e.query)-n])
		}
		return nil
	case kClearLine:
		m.setPickQuery("")
		return nil
	default:
		if k.Type == tea.KeyRunes && !k.Alt && key != kSwitcher {
			m.setPickQuery(e.query + string(k.Runes))
		} else if k.Type == tea.KeySpace {
			m.setPickQuery(e.query + " ")
		}
		return nil
	}
	m.pickShow(b.grid)
	return nil
}

// setPickQuery changes the search: one line, no controls or bidi marks,
// at most pickMaxQ runes.
func (m *Model) setPickQuery(q string) {
	q = dropBidi(sanitize.Line(strings.TrimLeftFunc(q, unicode.IsSpace)))
	if rs := []rune(q); len(rs) > pickMaxQ {
		q = string(rs[:pickMaxQ])
	}
	if q == m.emo.query {
		return
	}
	m.emo.query = q
	m.fillPicker()
}

// pickEmoji uses item i: reacts to the target (or takes its reaction back
// when it is already mine) or inserts it at the input's cursor.
func (m *Model) pickEmoji(i int) tea.Cmd {
	e := &m.emo
	if i < 0 || i >= len(e.items) {
		return nil
	}
	emoji := e.items[i].e
	react := e.react
	m.closePicker(true)
	m.noteEmoji(emoji)
	if !react {
		m.input.SetHeight(m.inputMax()) // see layoutInput
		m.input.InsertString(emoji)
		m.layoutInput()
		return nil
	}
	return m.reactWith(m.currentTarget(), emoji)
}

// currentTarget is the message being reacted to as it is now: reactions
// from elsewhere may have arrived while the picker was open.
func (m *Model) currentTarget() client.Message {
	for _, p := range []*pane{&m.main, &m.thread} {
		for _, x := range p.msgs {
			if x.ID == m.target.ID {
				return x
			}
		}
	}
	return m.target
}

// reactWith toggles my reaction emoji on message x.
func (m *Model) reactWith(x client.Message, emoji string) tea.Cmd {
	id, mine := x.ID, slices.Contains(x.Reactions[emoji], m.me.ID)
	return m.call("react", func(ctx context.Context) error {
		if mine {
			return m.c.Unreact(ctx, id, emoji)
		}
		return m.c.React(ctx, id, emoji)
	})
}

// pickMouse handles a click on the picker's actions.
func (m *Model) pickMouse(kind, rest string) tea.Cmd {
	if m.mode != modeEmoji {
		return nil
	}
	switch kind {
	case "emo":
		i, err := strconv.Atoi(rest)
		if err != nil {
			return nil
		}
		return m.pickEmoji(i)
	case "emotab":
		sec, err := strconv.Atoi(rest)
		if err != nil {
			return nil
		}
		b := m.pickSize(m.geo())
		m.pickLayout(max(b.cols, 1))
		m.pickJump(sec, b)
	case "emoclose":
		m.closePicker(true)
	}
	return nil
}

// pickWheel scrolls the grid; the selection follows into the view so the
// preview and Enter match what is on screen.
func (m *Model) pickWheel(up bool) {
	e := &m.emo
	b := m.pickSize(m.geo())
	m.pickLayout(max(b.cols, 1))
	if up {
		e.top -= 3
	} else {
		e.top += 3
	}
	e.top = min(max(e.top, 0), max(len(e.rows)-b.grid, 0))
	m.hover = ""
	if r := m.pickRowOf(e.sel); r >= e.top && r < e.top+b.grid {
		return
	}
	for r := e.top; r < e.top+b.grid && r < len(e.rows); r++ {
		if !e.rows[r].title {
			e.sel = e.rows[r].from
			return
		}
	}
}

func (m *Model) pickShown() bool {
	return m.emo.open && m.mode == modeEmoji || m.running(m.emo.shutAt, dToast)
}

// drawPicker draws the picker, or its fade-out after it closed.
func (m *Model) drawPicker(c *canvas, g geo) {
	if !m.pickShown() || m.overlayShown() {
		return
	}
	e := &m.emo
	b := m.pickSize(g)
	if b.cols < 1 || b.r.y1 <= 0 {
		return
	}
	m.pickLayout(b.cols)
	m.pickShow(b.grid)
	r := m.pickPlace(g, b)
	th := m.st.th
	k := ease(m.progress(e.at, dToast))
	live := e.open // a closing picker has no actions
	if !e.open {
		k = 1 - ease(m.progress(e.shutAt, dToast))
	}
	if k < 0.6 && b.framed && r.y1 < g.inY {
		r.y0++ // it rises into place
		r.y1++
	}
	m.floatGutter(c, g, r)
	old := c.setClip(r)
	defer c.setClip(old)
	act := func(a string) string {
		if !live {
			return ""
		}
		return a
	}
	w, h := r.x1-r.x0, r.y1-r.y0
	pbg := mix(th.bg, th.panel, k)
	line := mix(th.bg, th.line, k)
	ink := func(col rgb) cstyle { return fgc(mix(pbg, col, k)) }
	showEmoji := k >= 0.4 // emoji can't fade: they appear once the panel has

	// The frame's cells take the pane's own background, never a message's
	// tint, so the rounded corners stay clean.
	for x := r.x0; x < r.x1; x++ {
		base := th.bg
		if g.split && x >= g.RX {
			base = th.panel
		}
		c.fill(x, r.y0, 1, h, base, act("noop"))
	}
	lit := e.sel
	gx := r.x0 + 3 // grid's first column
	y := r.y0
	if !b.framed {
		gx = r.x0
		c.fill(r.x0, r.y0, w, h, pbg, act("noop"))
		m.pickSearch(c, r.x0, y, w, k, pbg, act("noop"))
		y++
	} else {
		m.box(c, r.x0, r.y0, w, h, line, pbg)
		c.fill(r.x0+1, r.y0+1, w-2, h-2, pbg, act("noop"))
		m.pickTitle(c, r, k, pbg, act)
		y++
		m.pickSearch(c, r.x0+2, y, w-4, k, pbg, act("noop"))
		y++
		if b.tabs {
			m.pickTabStrip(c, r, y, gx, b.cols, k, pbg, line, act)
			y += 2
		}
	}
	gridY := y
	for i := 0; i < b.grid; i++ {
		ri := e.top + i
		if ri >= len(e.rows) {
			break
		}
		row := e.rows[ri]
		yy := gridY + i
		if row.title {
			c.put(gx, yy, truncate(m.pickTitleText(row.sec), max(r.x1-gx-2, 1)), ink(th.faint), "")
			continue
		}
		for j := row.from; j < row.to; j++ {
			x := gx + (j-row.from)*4
			on := j == lit
			bg := pbg
			if on {
				bg = mix(pbg, th.accent, k)
			}
			a := act("emo:" + strconv.Itoa(j))
			st := cstyle{fg: th.fg, hasFg: true, bg: bg, hasBg: true}
			if on {
				st.attr |= aMonoRev
			}
			c.put(x, yy, "    ", st, a)
			if showEmoji {
				c.put(x+1, yy, e.items[j].e, st, a)
			}
		}
	}
	if b.framed {
		m.pickScrollbar(c, r, gridY, b.grid, k)
	}
	if b.preview {
		py := r.y1 - 3
		c.put(r.x0, py, "├"+strings.Repeat("─", w-2)+"┤", fgc(line), "")
		m.pickPreview(c, r, py+1, lit, k, showEmoji)
	}
	if b.framed {
		m.pickHints(c, r, k)
	}
}

func (m *Model) pickTitleText(sec int) string {
	e := &m.emo
	switch {
	case sec == pickSecFreq:
		return "Frequently used"
	case sec == pickSecHits && len(e.items) == 0:
		return "no emoji match “" + e.query + "”"
	case sec == pickSecHits:
		n := len(e.items)
		return strconv.Itoa(n) + " " + plural(n, "match", "matches")
	case sec >= 0 && sec < len(emojiCats):
		return emojiCats[sec].name
	}
	return ""
}

// pickTitle draws "React to bob" or "Insert emoji" and ✕ in the top edge.
func (m *Model) pickTitle(c *canvas, r rect, k float64, pbg rgb, act func(string) string) {
	th := m.st.th
	w := r.x1 - r.x0
	head, rest := "Insert emoji", ""
	if m.emo.react {
		head, rest = "React", "to "+strings.TrimPrefix(m.atName(m.target.Author), "@")
	}
	room := w - 9 // corners, the ✕ and a gap
	if room < 4 {
		return
	}
	x := c.put(r.x0+2, r.y0, " ", fgc(th.fg), "")
	if strWidth(head) > room {
		head, rest = truncate(head, room), ""
	}
	x = c.put(x, r.y0, head, fgb(mix(pbg, th.fg, k)), "")
	if rest != "" && room-strWidth(head) > 4 {
		x = c.put(x, r.y0, " "+truncate(rest, room-strWidth(head)-1), fgc(mix(pbg, th.faint, k)), "")
	}
	c.put(x, r.y0, " ", fgc(th.fg), "")
	cx := r.x1 - 5
	c.put(cx, r.y0, " ✕ ", fgc(mix(th.bg, th.dim, k)), act("emoclose"))
	c.tag(cx, r.y0, 3, 1, act("emoclose"), true, true)
}

// pickSearch draws the search box: ⌕, the query and a cursor, or the hint.
func (m *Model) pickSearch(c *canvas, x, y, w int, k float64, pbg rgb, act string) {
	th := m.st.th
	bg := mix(pbg, th.element, k)
	c.fill(x, y, w, 1, bg, act)
	if w < 4 {
		return
	}
	c.put(x+1, y, "⌕", fgc(mix(bg, th.dim, k)).on(bg), "")
	tx, room := x+3, w-4
	q := m.emo.query
	if q == "" {
		c.put(tx, y, "▏", fgc(mix(bg, th.accent, k)).on(bg), "")
		c.put(tx+1, y, truncate("search · rocket, party, :+1:", room-1), fgc(mix(bg, th.faint, k)).on(bg), "")
		return
	}
	// The end of a long query stays in view.
	for strWidth(q) > room-1 && q != "" {
		_, n := utf8.DecodeRuneInString(q)
		q = q[n:]
	}
	tx = c.put(tx, y, q, fgc(mix(bg, th.fg, k)).on(bg), "")
	c.put(tx, y, "▏", fgc(mix(bg, th.accent, k)).on(bg), "")
}

// pickTabStrip draws the category tabs and the divider under them. When
// they don't all fit, the ones around the current section show.
func (m *Model) pickTabStrip(c *canvas, r rect, y, gx, cols int, k float64, pbg, line rgb, act func(string) string) {
	th := m.st.th
	tabs := pickTabs()
	cur := slices.Index(tabs, m.pickSection())
	start, end := window(len(tabs), max(cur, 0), min(cols, len(tabs)))
	c.put(r.x0, y+1, "├"+strings.Repeat("─", r.x1-r.x0-2)+"┤", fgc(line), "")
	for i := start; i < end; i++ {
		sec := tabs[i]
		x := gx + (i-start)*4
		a := act("emotab:" + strconv.Itoa(sec))
		icon := recentIcon
		if sec >= 0 {
			icon = emojiCats[sec].icon
		}
		bg := pbg
		if i == cur {
			bg = mix(pbg, th.element, k)
		}
		c.put(x, y, "    ", cstyle{fg: th.fg, hasFg: true, bg: bg, hasBg: true}, a)
		if k >= 0.4 {
			c.put(x+1, y, icon, cstyle{fg: th.fg, hasFg: true, bg: bg, hasBg: true}, a)
		}
		c.tag(x, y, 4, 1, a, true, true)
		if i == cur {
			c.put(x, y+1, "━━━━", fgc(mix(pbg, th.accent, k)), "")
		}
	}
}

// pickScrollbar marks the visible part of the grid on the right edge.
func (m *Model) pickScrollbar(c *canvas, r rect, y, grid int, k float64) {
	n := len(m.emo.rows)
	if n <= grid || grid <= 0 {
		return
	}
	th := m.st.th
	size := max(grid*grid/n, 1)
	pos := (grid - size) * m.emo.top / max(n-grid, 1)
	for i := range size {
		c.put(r.x1-1, y+pos+i, "┃", fgc(mix(th.bg, th.dim, k)), "")
	}
}

// pickPreview draws the lit emoji big enough to read: the emoji, its
// shortcode, its other names and keywords, and what Enter does.
func (m *Model) pickPreview(c *canvas, r rect, y, i int, k float64, showEmoji bool) {
	th := m.st.th
	e := &m.emo
	if i < 0 || i >= len(e.items) {
		return
	}
	it := e.items[i]
	verb := "⏎ insert"
	if e.react {
		verb = "⏎ react"
		if slices.Contains(m.currentTarget().Reactions[it.e], m.me.ID) {
			verb = "⏎ remove"
		}
	}
	right := r.x1 - 3
	if r.x1-r.x0 >= 30 {
		right -= strWidth(verb)
		c.put(right, y, verb, fgc(mix(th.panel, th.accent, k)), "")
		right -= 2
	}
	x := r.x0 + 3
	if showEmoji {
		c.put(x, y, it.e, fgc(th.fg), "")
	}
	x += 4
	if it.def < 0 || x >= right {
		return
	}
	d := emojiTable[it.def]
	x = c.put(x, y, truncate(":"+d.name()+":", right-x), fgb(mix(th.panel, th.fg, k)), "")
	var more []string
	for n := range strings.FieldsSeq(d.names) {
		if n != d.name() {
			more = append(more, ":"+n+":")
		}
	}
	more = append(more, strings.Fields(d.keys)...)
	if room := right - x - 2; room >= 4 && len(more) > 0 {
		c.put(x+2, y, truncate(strings.Join(more, " "), room), fgc(mix(th.panel, th.faint, k)), "")
	}
}

// pickHints draws the keys in the bottom edge.
func (m *Model) pickHints(c *canvas, r rect, k float64) {
	th := m.st.th
	verb := "insert"
	if m.emo.react {
		verb = "react"
	}
	w := r.x1 - r.x0
	for _, h := range []string{" ←↑↓→ move · ⇥ category · ⏎ " + verb + " · esc ", " ←↑↓→ ⇥ ⏎ esc "} {
		if w >= strWidth(h)+4 {
			c.put(r.x1-2-strWidth(h), r.y1-1, h, fgc(mix(th.bg, th.faint, k)), "")
			return
		}
	}
}

// drawReactTip shows who reacted while the pointer is on a reaction pill:
// "bob, carol reacted with :tada:".
func (m *Model) drawReactTip(c *canvas, g geo) {
	if !strings.HasPrefix(m.hover, "react:") || m.mode != modeNormal && m.mode != modeSelect {
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(m.hover, "react:"), ":", 3)
	if len(parts) < 3 {
		return
	}
	p := &m.main
	if parts[0] == "thr" {
		p = &m.thread
	}
	i := findRef(p.msgs, parts[1])
	if i < 0 {
		return
	}
	text := m.reactorsText(p.msgs[i].Reactions[parts[2]], parts[2])
	if text == "" {
		return
	}
	px, py := -1, -1
	for y := 0; y < g.statusY && px < 0; y++ {
		for x := 0; x < c.w; x++ {
			if c.actionAt(x, y) == m.hover {
				px, py = x, y
				break
			}
		}
	}
	if px < 0 {
		return
	}
	y := py - 1
	if y < g.msgTop {
		y = py + 1
	}
	text = " " + truncate(text, max(m.w-4, 1)) + " "
	tw := strWidth(text) + 2
	x := min(px, m.w-tw)
	if x < 0 || y < 0 || y >= g.statusY {
		return
	}
	th := m.st.th
	old := c.setClip(rect{0, 0, m.w, g.statusY})
	defer c.setClip(old)
	c.pill(x, y, text, fgc(th.bg).on(th.dim).with(aMonoRev), "")
}

// reactorsText is "you, bob and 3 others reacted with :tada:".
func (m *Model) reactorsText(us []uint64, emoji string) string {
	if len(us) == 0 {
		return ""
	}
	const show = 5
	var names []string
	for _, u := range us {
		if len(names) == show {
			break
		}
		n := "you"
		if u != m.me.ID {
			n = strings.TrimPrefix(m.atName(u), "@")
		}
		names = append(names, n)
	}
	who := strings.Join(names, ", ")
	if more := len(us) - len(names); more > 0 {
		who += " and " + strconv.Itoa(more) + " " + plural(more, "other", "others")
	} else if len(names) > 1 {
		who = strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
	what := sanitize.Line(emoji)
	if n := emojiName(emoji); n != "" {
		what = ":" + n + ":"
	}
	return who + " reacted with " + what
}
