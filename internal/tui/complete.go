package tui

import (
	"slices"
	"strconv"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// The completion popup sits just above the input. It opens by itself while
// the input is a bare "/command" (no space yet) or the word at the cursor is
// an @name or #channel. While it is shown, ↑/↓ (Ctrl-P/N) move, Enter or Tab
// puts the highlighted item into the input (never sends), Esc closes it.
// Once closed it stays closed until the input changes.

// compRows is how many items the popup shows at once; more scroll.
const compRows = 4

type compItem struct {
	text string // what goes into the input: "/notify", "@bob", "#general"
	hint string // argument hint (commands only)
	desc string // sanitized one-line description
	rank int    // sort key: lower first
}

type completer struct {
	kind  byte // '/', '@' or '#'; 0 = nothing to show
	items []compItem
	sel   int
	val   string // the input value the items are for; any other value hides them
	query string // what was matched (without the sigil), to keep sel while it doesn't change

	closed    bool   // closed by Esc, a completion or a paste...
	closedVal string // ...at this input value: stays closed until it changes

	ch, root uint64 // where the input was when the items were made

	// here caches the members of channel hereCh for ranking @ candidates;
	// reloadChans drops it.
	hereCh uint64
	here   map[uint64]bool
}

// compShown reports whether the popup is on screen (and owns its keys).
func (m *Model) compShown() bool {
	p := m.focus()
	return m.comp.kind != 0 && len(m.comp.items) > 0 && m.mode == modeNormal &&
		m.comp.val == m.input.Value() && m.comp.ch == p.ch && m.comp.root == p.root
}

// hideComp takes the popup off screen without the Esc "stay closed": the
// next edit may show it again.
func (m *Model) hideComp() { m.comp.kind = 0 }

// reloadComp redoes a shown popup's items after users, groups or channels
// changed under it.
func (m *Model) reloadComp() {
	if m.compShown() {
		m.fillComp(m.input.Value())
	}
}

// closeComp hides the popup until the input changes.
func (m *Model) closeComp() {
	m.comp.kind = 0
	m.comp.closed, m.comp.closedVal = true, m.input.Value()
}

// refreshComp recomputes the popup after the input changed.
func (m *Model) refreshComp() {
	v := m.input.Value()
	if m.comp.closed && v == m.comp.closedVal {
		m.comp.kind = 0
		return
	}
	m.comp.closed = false
	m.fillComp(v)
}

// fillComp sets the popup's items for input v; kind 0 when there are none.
func (m *Model) fillComp(v string) {
	c := &m.comp
	kind, query := byte(0), ""
	switch {
	case v == "" || m.mode != modeNormal:
	case strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") && !strings.ContainsFunc(v, unicode.IsSpace):
		if m.editing == 0 { // an edit's text is never a command
			kind, query = '/', strings.ToLower(v[1:])
		}
	default:
		if w, before, _ := m.cursorWord(); before > 0 && (w[0] == '@' || w[0] == '#') {
			kind, query = w[0], strings.ToLower(string([]rune(w)[1:before]))
		}
	}
	if kind != c.kind || query != c.query {
		c.sel = 0
	}
	c.kind, c.query, c.val = kind, query, v
	c.ch, c.root = m.focus().ch, m.focus().root
	c.items = c.items[:0]
	switch kind {
	case '/':
		m.commandItems(query)
	case '@':
		m.userItems(query)
	case '#':
		m.chanItems(query)
	}
	slices.SortStableFunc(c.items, func(a, b compItem) int { return a.rank - b.rank })
	if len(c.items) == 0 {
		c.kind = 0
	}
	c.sel = min(c.sel, max(len(c.items)-1, 0))
}

// matchRank is 0 for a prefix match, 1 for a substring match, -1 for none.
func matchRank(q, s string) int {
	switch {
	case strings.HasPrefix(s, q):
		return 0
	case strings.Contains(s, q):
		return 1
	}
	return -1
}

// wordRank is matchRank for @ and # words, which turn up in ordinary text
// ("fixed in #12"): substring matches need two letters, and never come
// from digits alone, so Enter doesn't rewrite text that wasn't a mention.
func wordRank(q, s string) int {
	r := matchRank(q, s)
	if r == 1 && (len(q) < 2 || strings.Trim(q, "0123456789") == "") {
		return -1
	}
	return r
}

func (m *Model) commandItems(q string) {
	voice := client.VoiceAvailable()
	for _, c := range commandTable {
		if c.name == "" || c.more || (c.admin && !m.me.Admin) || (c.voice && !voice) {
			continue
		}
		r := matchRank(q, c.name)
		switch {
		case r < 0:
			continue
		case c.name == q:
			r = -1 // an exact name goes first, so Enter completes what was typed
		}
		m.comp.items = append(m.comp.items, compItem{text: "/" + c.name, hint: c.args, desc: c.desc, rank: r})
	}
}

// userItems lists people (this channel's members first) and then groups.
func (m *Model) userItems(q string) {
	c := &m.comp
	if ch := m.focus().ch; c.here == nil || c.hereCh != ch {
		c.hereCh, c.here = ch, map[uint64]bool{}
		if v, ok := m.chanView(ch); ok {
			for _, u := range v.Members {
				c.here[u] = true
			}
		}
	}
	for _, u := range m.userList {
		if u.ID == m.me.ID || u.Disabled {
			continue
		}
		r := wordRank(q, strings.ToLower(u.Name))
		if r < 0 && u.Display != "" && wordRank(q, strings.ToLower(u.Display)) >= 0 {
			r = 1
		}
		if r < 0 {
			continue
		}
		cls := 1
		if c.here[u.ID] {
			cls = 0
		}
		desc := ""
		if d := sanitize.Line(u.Display); d != u.Name {
			desc = d
		}
		m.comp.items = append(m.comp.items, compItem{text: "@" + sanitize.Line(u.Name), desc: desc, rank: r*3 + cls})
	}
	for _, g := range m.groups {
		if r := wordRank(q, g.Name); r >= 0 {
			m.comp.items = append(m.comp.items, compItem{
				text: "@" + sanitize.Line(g.Name),
				desc: "group · " + strconv.Itoa(len(g.Members)) + " " + plural(len(g.Members), "person", "people"),
				rank: r*3 + 2,
			})
		}
	}
}

// chanItems lists named channels, the ones I'm in first.
func (m *Model) chanItems(q string) {
	for _, v := range m.chans {
		if v.Kind == proto.ChanDM || (!v.Member && v.Kind != proto.ChanPublic) {
			continue
		}
		r := wordRank(q, v.Name)
		if r < 0 {
			continue
		}
		cls, desc := 0, sanitize.Line(v.Topic)
		if !v.Member {
			cls, desc = 1, "not joined"
		}
		m.comp.items = append(m.comp.items, compItem{text: "#" + sanitize.Line(v.Name), desc: desc, rank: r*2 + cls})
	}
}

// cursorWord is the whitespace-delimited word around the input's cursor,
// with how many of its runes are before and after the cursor.
func (m *Model) cursorWord() (word string, before, after int) {
	line, col := m.cursorLine()
	start, end := col, col
	for start > 0 && !unicode.IsSpace(line[start-1]) {
		start--
	}
	for end < len(line) && !unicode.IsSpace(line[end]) {
		end++
	}
	return string(line[start:end]), col - start, end - col
}

// cursorLine is the input's line with the cursor and the cursor's rune
// index in it.
func (m *Model) cursorLine() ([]rune, int) {
	lines := strings.Split(m.input.Value(), "\n")
	row := m.input.Line()
	if row < 0 || row >= len(lines) {
		return nil, 0
	}
	line := []rune(lines[row])
	li := m.input.LineInfo()
	return line, min(max(li.StartColumn+li.ColumnOffset, 0), len(line))
}

// compKey handles a key while the popup is shown; false passes it on.
func (m *Model) compKey(key string) (tea.Cmd, bool) {
	c := &m.comp
	switch key {
	case kUp, "ctrl+p", kShiftTab:
		c.sel = (c.sel - 1 + len(c.items)) % len(c.items)
	case kDown, "ctrl+n":
		c.sel = (c.sel + 1) % len(c.items)
	case kEnter, kTab:
		m.acceptComp()
	case kEsc:
		m.closeComp()
	default:
		return nil, false
	}
	return nil, true
}

// acceptComp puts the highlighted item into the input, followed by a space.
func (m *Model) acceptComp() {
	c := &m.comp
	if c.sel < 0 || c.sel >= len(c.items) {
		return
	}
	it := c.items[c.sel]
	if c.kind == '/' {
		m.input.SetValue(it.text + " ")
	} else {
		_, before, after := m.cursorWord()
		m.input.SetHeight(m.inputMax()) // see layoutInput
		for range before {
			m.input, _ = m.input.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		}
		for range after {
			m.input, _ = m.input.Update(tea.KeyMsg{Type: tea.KeyDelete})
		}
		m.input.InsertString(it.text)
		// Step over a space that's already there instead of doubling it.
		if line, col := m.cursorLine(); col < len(line) && unicode.IsSpace(line[col]) {
			m.input, _ = m.input.Update(tea.KeyMsg{Type: tea.KeyRight})
		} else {
			m.input.InsertString(" ")
		}
	}
	m.layoutInput()
	m.closeComp()
}

// tabComplete handles Tab while the popup is hidden: it reopens it (after
// an Esc) or, with a single candidate, completes straight away.
func (m *Model) tabComplete() bool {
	m.comp.closed = false
	m.fillComp(m.input.Value())
	switch len(m.comp.items) {
	case 0:
		m.comp.kind = 0
		return false
	case 1:
		m.acceptComp()
	}
	return true
}

// compLines renders the popup in at most h rows of width w.
func (m *Model) compLines(w, h int) []string {
	c := &m.comp
	n := min(len(c.items), compRows, h)
	if n <= 0 || w <= 0 {
		return nil
	}
	start, end := window(len(c.items), c.sel, n)
	mw := 0 // columns for the ↑/↓ scroll marks
	switch {
	case len(c.items) <= n:
	case w >= 30:
		mw = 2
	case w >= 4:
		mw = 1
	}
	bw := w - mw
	out := make([]string, 0, n)
	for i := start; i < end; i++ {
		it := c.items[i]
		rest := ""
		if it.hint != "" {
			rest = " " + it.hint
		}
		if it.desc != "" {
			rest += "  " + it.desc
		}
		// A dim bar sets the popup apart from the messages under it.
		var l string
		if i == c.sel {
			l = m.st.overlaySel.Render(fit(truncate("▌"+it.text+rest, bw), bw))
		} else {
			l = fit(truncate(m.st.dim.Render("▏")+m.st.bold.Render(it.text)+m.st.dim.Render(rest), bw), bw)
		}
		if mw > 0 {
			mark := " "
			switch {
			case i == start && start > 0:
				mark = "↑"
			case i == end-1 && end < len(c.items):
				mark = "↓"
			}
			l += spaces(mw-1) + m.st.dim.Render(mark)
		}
		out = append(out, fit(l, w))
	}
	return out
}
