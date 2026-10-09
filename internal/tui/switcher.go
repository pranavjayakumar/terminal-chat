package tui

import (
	"slices"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// ---- fuzzy matching ----

// fuzzyScore matches pattern as a case-insensitive subsequence of s. Higher
// is better: consecutive runs, word starts and prefixes score extra, long
// targets a little less.
func fuzzyScore(pattern, s string) (int, bool) {
	p := []rune(strings.ToLower(pattern))
	t := []rune(strings.ToLower(s))
	if len(p) == 0 {
		return 0, true
	}
	score, pi, prev := 0, 0, -2
	for i, r := range t {
		if pi >= len(p) {
			break
		}
		if r != p[pi] {
			continue
		}
		score++
		if i == prev+1 {
			score += 5
		}
		if i == 0 || isSep(t[i-1]) {
			score += 8
		}
		prev = i
		pi++
	}
	if pi < len(p) {
		return 0, false
	}
	if strings.HasPrefix(string(t), string(p)) {
		score += 10
	}
	return score - len(t)/4, true
}

func isSep(r rune) bool {
	return r == '-' || r == '_' || r == ' ' || r == '#' || r == '@' || r == ',' || unicode.IsPunct(r)
}

// ---- quick switcher ----

type switchItem struct {
	label   string // sanitized, e.g. "# general" or "@ bob"
	match   string // what the query is matched against
	ch      uint64 // channel to open; 0 = create the DM with user
	user    uint64
	unread  int
	mention bool
	order   int
	score   int
}

type switcher struct {
	items []switchItem
	shown []switchItem
	sel   int
}

func (s *switcher) filter(q string) {
	s.shown = s.shown[:0]
	for _, it := range s.items {
		if sc, ok := fuzzyScore(q, it.match); ok {
			it.score = sc
			s.shown = append(s.shown, it)
		}
	}
	slices.SortStableFunc(s.shown, func(a, b switchItem) int {
		if a.score != b.score {
			return b.score - a.score
		}
		if ua, ub := a.unread > 0 || a.mention, b.unread > 0 || b.mention; ua != ub {
			if ua {
				return -1
			}
			return 1
		}
		return a.order - b.order
	})
	s.sel = 0
}

func (m *Model) switchItems() []switchItem {
	var items []switchItem
	dmWith := map[uint64]int{} // user → index of their 1:1 DM item
	for _, v := range m.chans {
		if v.Kind == proto.ChanDM {
			if !v.Member {
				continue
			}
			var others []uint64
			for _, u := range v.Members {
				if u != m.me.ID {
					others = append(others, u)
				}
			}
			if len(others) == 1 {
				dmWith[others[0]] = int(v.ID)
				continue
			}
			name := sanitize.Line(v.Display)
			items = append(items, switchItem{label: "@ " + name, match: name, ch: v.ID, unread: v.Unread, mention: v.Mention})
			continue
		}
		if !v.Member && v.Kind != proto.ChanPublic {
			continue
		}
		name := sanitize.Line(v.Name)
		items = append(items, switchItem{label: "# " + name, match: name, ch: v.ID, unread: v.Unread, mention: v.Mention})
	}
	for _, u := range m.userList {
		if u.ID == m.me.ID || u.Disabled {
			continue
		}
		name := sanitize.Line(u.Name)
		it := switchItem{label: "@ " + name, match: name, user: u.ID}
		if d := sanitize.Line(u.Display); d != "" && d != name {
			it.match += " " + d
			it.label += "  " + d
		}
		if id, ok := dmWith[u.ID]; ok {
			it.ch = uint64(id)
			if v, ok := m.chanView(it.ch); ok {
				it.unread, it.mention = v.Unread, v.Mention
			}
		}
		items = append(items, it)
	}
	for i := range items {
		items[i].order = i
		if items[i].ch != 0 && m.hasDraft(items[i].ch) {
			items[i].label += " ✎"
		}
	}
	return items
}

func (m *Model) openSwitcher() {
	m.leaveSelect()
	m.mode = modeSwitcher
	m.line.Reset()
	m.line.Prompt = "> "
	m.line.Placeholder = "jump to a channel or person"
	m.line.Focus()
	m.sw = switcher{items: m.switchItems()}
	m.sw.filter("")
}

func (m *Model) switcherKey(k tea.KeyMsg) tea.Cmd {
	switch keyName(k) {
	case kEsc, kSwitcher:
		m.mode = modeNormal
		return nil
	case kUp, "ctrl+p", kShiftTab:
		if m.sw.sel > 0 {
			m.sw.sel--
		}
		return nil
	case kDown, "ctrl+n", kTab:
		if m.sw.sel < len(m.sw.shown)-1 {
			m.sw.sel++
		}
		return nil
	case kEnter:
		m.mode = modeNormal
		if m.sw.sel >= len(m.sw.shown) {
			return nil
		}
		it := m.sw.shown[m.sw.sel]
		if it.ch != 0 {
			return m.openChannel(it.ch)
		}
		return m.openDM([]uint64{it.user})
	}
	before := m.line.Value()
	var cmd tea.Cmd
	m.line, cmd = m.line.Update(k)
	if m.line.Value() != before {
		m.sw.filter(m.line.Value())
	}
	return cmd
}

// ---- search results ----

type searchState struct {
	q   string
	res []proto.Message
	sel int
}

func (m *Model) searchKey(k tea.KeyMsg) tea.Cmd {
	s := &m.search
	switch keyName(k) {
	case kEsc, "q":
		m.mode = modeNormal
	case kUp, "ctrl+p":
		if s.sel > 0 {
			s.sel--
		}
	case kDown, "ctrl+n":
		if s.sel < len(s.res)-1 {
			s.sel++
		}
	case kEnter:
		m.mode = modeNormal
		if s.sel >= len(s.res) {
			return nil
		}
		return m.jumpTo(s.res[s.sel])
	}
	return nil
}

// jumpTo opens the message's channel (and thread) and selects it if it is
// loaded.
func (m *Model) jumpTo(x proto.Message) tea.Cmd {
	m.cancelEdit()
	if _, ok := m.chanView(x.Chan); !ok {
		m.setInfo("that channel isn't available here")
		return nil
	}
	cmd := m.openChannel(x.Chan)
	p := &m.main
	if x.Thread != 0 {
		cmd = tea.Batch(cmd, m.openThread(x.Thread))
		p = &m.thread
	}
	find := func() int {
		for i, y := range p.msgs {
			if y.ID == x.ID {
				return i
			}
		}
		return -1
	}
	i := find()
	// A top-level message older than the loaded page: widen the page while
	// the cache may still hold it.
	for i < 0 && p == &m.main && x.ID != 0 && loadedReal(p.msgs) >= m.limit(x.Chan) && m.limit(x.Chan) < maxJumpLimit {
		m.limits[x.Chan] = min(m.limit(x.Chan)*4, maxJumpLimit)
		m.reloadPane(p)
		i = find()
	}
	if i >= 0 {
		m.mode = modeSelect
		p.sel = i
		m.ensureVisible(p)
		m.updatePlaceholder()
		return cmd
	}
	m.setInfo("older message — scroll up to find it")
	return cmd
}

// loadedReal counts the acknowledged messages in a pane (not unsent ones).
func loadedReal(ms []client.Message) int {
	n := 0
	for _, x := range ms {
		if x.ID != 0 {
			n++
		}
	}
	return n
}

func fmtWhen(ms int64) string {
	t := time.UnixMilli(ms).Local()
	if sameDay(ms, time.Now().UnixMilli()) {
		return t.Format("15:04")
	}
	return t.Format("2 Jan")
}
