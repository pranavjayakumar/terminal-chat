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
// puts the highlighted item into the input, Esc closes it. Enter never sends
// from the / menu; on an @/# word that already equals the highlighted item it
// closes the popup and sends.
// Once closed it stays closed until the input changes.

// compRows is how many items the popup shows at once; more scroll.
const compRows = 6

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
	// Same query: keep the highlight on the same item, even if users or
	// channels changed underneath (else Enter would insert someone else).
	keep := ""
	if kind != c.kind || query != c.query {
		c.sel = 0
	} else if c.sel >= 0 && c.sel < len(c.items) {
		keep = c.items[c.sel].text
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
	if keep != "" {
		if i := slices.IndexFunc(c.items, func(it compItem) bool { return it.text == keep }); i >= 0 {
			c.sel = i
		}
	}
	c.sel = min(c.sel, max(len(c.items)-1, 0))
}

// exactRank puts a name equal to what was typed first, whatever its
// membership or kind, so it is highlighted and Enter sends instead of
// rewriting it to another name. The server keeps user and group names apart;
// were both to match, the stable sort leaves the user first.
const exactRank = -1

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

// compCommands are the commands this user can run in this build.
func (m *Model) compCommands() []cmdSpec {
	voice := client.VoiceAvailable()
	var out []cmdSpec
	for _, c := range commandTable {
		if c.name == "" || c.more || (c.admin && !m.me.Admin) || (c.voice && !voice) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (m *Model) commandItems(q string) {
	for _, c := range m.compCommands() {
		r := matchRank(q, c.name)
		switch {
		case r < 0:
			continue
		case c.name == q:
			r = exactRank // so Enter completes what was typed
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
	// After /dm, "@me" and my own name open my notes: offer them, so Enter
	// on them never completes to someone else whose name contains "me".
	dm := strings.HasPrefix(strings.ToLower(m.input.Value()), "/dm ")
	if dm {
		if strings.HasPrefix("me", q) {
			rank := 0
			if q == "me" {
				rank = exactRank
			}
			c.items = append(c.items, compItem{text: "@me", desc: "your notes", rank: rank})
		}
		if me := strings.ToLower(m.me.Name); me != "" && me != "me" && strings.HasPrefix(me, q) {
			rank := 0
			if me == q {
				rank = exactRank
			}
			c.items = append(c.items, compItem{text: "@" + sanitize.Line(m.me.Name), desc: "you · your notes", rank: rank})
		}
	}
	if me := strings.ToLower(m.me.Name); !dm && me != "" && me == q {
		// "as @alice said": my exact name is an exact name like any other,
		// so Enter sends instead of completing to a longer one.
		c.items = append(c.items, compItem{text: "@" + sanitize.Line(m.me.Name), desc: "you", rank: exactRank})
	}
	for _, u := range m.userList {
		// An account named "me" (from before the name was reserved) can't
		// be addressed: @me is always you (#113).
		if u.ID == m.me.ID || u.Disabled || u.Name == proto.NameMe {
			continue
		}
		r := wordRank(q, strings.ToLower(u.Name))
		if r < 0 && u.Display != "" && wordRank(q, strings.ToLower(u.Display)) >= 0 {
			r = 1
		}
		if r < 0 {
			continue
		}
		rank := r * 3
		switch {
		case strings.ToLower(u.Name) == q:
			rank = exactRank
		case !c.here[u.ID]:
			rank++
		}
		desc := ""
		if d := sanitize.Line(u.Display); d != u.Name {
			desc = d
		}
		m.comp.items = append(m.comp.items, compItem{text: "@" + sanitize.Line(u.Name), desc: desc, rank: rank})
	}
	for _, g := range m.groups {
		if r := wordRank(q, g.Name); r >= 0 {
			rank := r*3 + 2
			if strings.ToLower(g.Name) == q {
				rank = exactRank
			}
			m.comp.items = append(m.comp.items, compItem{
				text: "@" + sanitize.Line(g.Name),
				desc: "group · " + strconv.Itoa(len(g.Members)) + " " + plural(len(g.Members), "person", "people"),
				rank: rank,
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
		rank := r*2 + cls
		if strings.ToLower(v.Name) == q {
			rank = exactRank
		}
		m.comp.items = append(m.comp.items, compItem{text: "#" + sanitize.Line(v.Name), desc: desc, rank: rank})
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
	case kEnter:
		if m.exactComp() {
			// Nothing to complete: Enter sends as if the popup weren't there.
			m.closeComp()
			return nil, false
		}
		m.acceptComp()
	case kTab:
		m.acceptComp()
	case kEsc:
		m.closeComp()
	default:
		return nil, false
	}
	return nil, true
}

// exactComp reports whether the @/# word at the cursor already is the
// highlighted item. Commands never count: Enter on the menu always completes.
func (m *Model) exactComp() bool {
	c := &m.comp
	if c.kind == '/' || c.sel < 0 || c.sel >= len(c.items) {
		return false
	}
	w, _, _ := m.cursorWord()
	return strings.EqualFold(w, c.items[c.sel].text) // names are case-insensitive
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
