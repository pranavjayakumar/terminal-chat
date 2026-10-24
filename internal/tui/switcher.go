package tui

import (
	"context"
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
	haveNotes := false
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
			if len(others) == 0 {
				haveNotes = true
			}
			name := sanitize.Line(v.Display)
			match := name
			if len(others) == 0 {
				match += " me" // my notes
			}
			items = append(items, switchItem{label: "@ " + name, match: match, ch: v.ID, unread: v.Unread, mention: v.Mention})
			continue
		}
		if v.Kind == proto.ChanAgentDM {
			if a, ok := m.myAgent(); ok && v.Member {
				name := agentName(a.Handle) + " (your agent)"
				items = append(items, switchItem{label: "@ " + name, match: name + " " + a.Handle, ch: v.ID, unread: v.Unread, mention: v.Mention})
			}
			continue
		}
		if !v.Member && v.Kind != proto.ChanPublic {
			continue
		}
		name := sanitize.Line(v.Name)
		items = append(items, switchItem{label: "# " + name, match: name, ch: v.ID, unread: v.Unread, mention: v.Mention})
	}
	for _, u := range m.userList {
		if u.ID == m.me.ID {
			if !haveNotes { // opened on demand, like a DM
				name := sanitize.Line(u.Name) + " (notes)"
				items = append(items, switchItem{label: "@ " + name, match: name + " me", user: u.ID})
			}
			continue
		}
		if u.Disabled {
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

// ---- search results ----

type searchState struct {
	q    string
	pins uint64 // nonzero: the /pins list of this channel, not search results
	res  []proto.Message
	sel  int
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

// showPins loads ch's pinned messages. The list is refreshed from the server
// when online; offline it shows whatever the cache has.
func (m *Model) showPins(ch uint64) tea.Cmd {
	be := m.c
	ctx := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, reqTimeout)
		defer cancel()
		_ = be.FetchPins(ctx, ch)
		var res []proto.Message
		for _, x := range be.Pins(ch) {
			res = append(res, x.Message)
		}
		return pinsMsg{ch: ch, res: res}
	}
}

// jumpState is a jump's lasting effects: the channel whose page it widened
// and a thread jump still waiting for the thread's history.
type jumpState struct {
	wide    uint64 // channel whose page limit the jump raised, 0 if none
	prev    int    // that channel's limits entry before (0: the default)
	waiting bool   // want is a thread reply that isn't loaded yet
	want    proto.Message
}

// endWiden puts back the page limit a jump raised, so one jump to an old
// message doesn't leave a channel at 2000 messages for the whole session.
// reload says whether to re-read the pane (not needed when it is about to
// be replaced).
func (m *Model) endWiden(reload bool) {
	j := &m.jump
	if j.wide == 0 {
		return
	}
	ch := j.wide
	j.wide = 0
	if j.prev > 0 {
		m.limits[ch] = j.prev
	} else {
		delete(m.limits, ch)
	}
	if reload && m.main.ch == ch {
		m.reloadPane(&m.main)
		m.clampScroll(&m.main)
	}
}

// trimWiden ends a widening once the selection is back among the messages
// of a normal page (it moved on, towards the newest ones).
func (m *Model) trimWiden() {
	p := &m.main
	if m.jump.wide == 0 || m.jump.wide != p.ch || p.sel < 0 || p.sel >= len(p.msgs) {
		return
	}
	normal := m.jump.prev
	if normal <= 0 {
		normal = pageLimit
	}
	if loadedReal(p.msgs[p.sel:]) <= normal {
		m.endWiden(true)
		m.ensureVisible(p)
	}
}

// threadLoadedMsg says the request for a thread's replies finished.
type threadLoadedMsg struct {
	ch, root uint64
	err      error
}

// jumpTo opens the message's channel (and thread) and selects it if it is
// loaded. An old top-level message widens the channel's page for as long as
// the selection needs it; a thread reply that isn't cached yet is selected
// when the thread's history arrives.
func (m *Model) jumpTo(x proto.Message) tea.Cmd {
	m.cancelEdit()
	m.jump.waiting = false
	if _, ok := m.chanView(x.Chan); !ok {
		m.setInfo("that channel isn't available here")
		return nil
	}
	m.endWiden(true)
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
		if m.jump.wide != x.Chan {
			m.jump.wide, m.jump.prev = x.Chan, m.limits[x.Chan]
		}
		m.limits[x.Chan] = min(m.limit(x.Chan)*4, maxJumpLimit)
		m.reloadPane(p)
		i = find()
	}
	switch {
	case i >= 0:
		m.selectJumped(p, i)
	case p == &m.thread && x.ID != 0:
		// The replies are being fetched: look again when they arrive.
		m.jump.waiting, m.jump.want = true, x
		m.setInfo("loading the thread…")
	default:
		m.endWiden(true)
		m.setInfo("older message — scroll up to find it")
	}
	return cmd
}

func (m *Model) selectJumped(p *pane, i int) {
	m.mode = modeSelect
	p.sel = i
	m.ensureVisible(p)
	m.updatePlaceholder()
}

// finishJump retries a waiting thread jump after the thread's replies were
// reloaded; final says no more replies are coming.
func (m *Model) finishJump(final bool) {
	j := &m.jump
	if !j.waiting {
		return
	}
	if !m.thread.open() || m.thread.root != j.want.Thread || (m.mode != modeNormal && m.mode != modeSelect) {
		j.waiting = false // the user went elsewhere
		return
	}
	for i, y := range m.thread.msgs {
		if y.ID == j.want.ID {
			j.waiting = false
			m.clearNotice()
			m.selectJumped(&m.thread, i)
			return
		}
	}
	if final {
		j.waiting = false
		m.setInfo("older message — scroll up to find it")
	}
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
