package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

const sidebarW = 18

// dims is the layout for the current size and state.
type dims struct {
	side           bool
	mainW          int
	msgW, thrW     int
	split, thrFull bool
	inputH, bodyH  int
}

// statusRows is the notice/typing row. It is always reserved so the log
// doesn't jump when someone starts typing or a notice appears.
const statusRows = 1

func (m *Model) sidebarShown() bool {
	switch m.sidebar {
	case 1:
		return m.w >= minSidebarCols
	case -1:
		return false
	}
	return m.w >= 70
}

func (m *Model) dims() dims {
	var d dims
	if m.w <= 0 || m.h <= 0 {
		return d
	}
	d.mainW = m.w
	if m.sidebarShown() {
		d.side = true
		d.mainW = m.w - sidebarW - 1
	}
	d.msgW = d.mainW
	if m.thread.open() {
		if m.w >= 100 {
			d.split = true
			d.thrW = max(d.mainW*2/5, 30)
			d.msgW = d.mainW - d.thrW - 1
		} else {
			d.thrFull = true
			d.thrW = d.mainW
		}
	}
	d.inputH = 1
	if m.mode != modeReact {
		d.inputH = min(inputLines(m.input.Value(), d.mainW-2), m.inputMax())
	}
	d.bodyH = max(m.h-1-statusRows-1-d.inputH, 0)
	return d
}

// ---- panes ----

// paneLines lays out every message of p at width w (gutter included) and
// says which message owns each line (-1 for day separators). Memoized: it
// only re-runs after a reload, resize or selection change.
func (m *Model) paneLines(p *pane, w int) ([]string, []int) {
	sel := -1
	if m.mode == modeSelect || m.mode == modeConfirmDelete || m.mode == modeReact {
		sel = p.sel
	}
	if p.memoLines != nil && p.memoVer == p.ver && p.memoW == w && p.memoSel == sel {
		return p.memoLines, p.memoOwner
	}
	cw := max(w-1, 1)
	var lines []string
	var owner []int
	for i, x := range p.msgs {
		sep := i == 0 || !sameDay(p.msgs[i-1].Time, x.Time)
		if sep {
			lines = append(lines, " "+m.r.daySep(time.UnixMilli(x.Time).Local(), cw))
			owner = append(owner, -1)
		}
		header := sep || !grouped(p.msgs[i-1], x)
		gutter := " "
		switch {
		case i == sel:
			gutter = m.st.selMark.Render("▌")
		case x.Author != m.me.ID && !x.Deleted && !m.users[x.Author].Bot:
			gutter = m.concernMark(x.Body)
		}
		for _, l := range m.rc.get(&m.r, x, cw, header) {
			lines = append(lines, gutter+l)
			owner = append(owner, i)
		}
		if p.root != 0 && i == 0 && len(p.msgs) > 1 {
			lines = append(lines, " "+m.st.sep.Render(truncate("── replies ──", cw)))
			owner = append(owner, -1)
		}
	}
	if lines == nil {
		lines, owner = []string{}, []int{}
	}
	p.memoLines, p.memoOwner, p.memoVer, p.memoW, p.memoSel = lines, owner, p.ver, w, sel
	return lines, owner
}

// viewPane is the visible window of p: exactly h lines of width w,
// bottom-anchored.
func (m *Model) viewPane(p *pane, w, h int, unread int) []string {
	if h <= 0 {
		return nil
	}
	out := make([]string, 0, h)
	if !p.open() {
		msg := "no channel — Ctrl-K to open one"
		for i := 0; i < h; i++ {
			if i == h/2 {
				out = append(out, fit(m.st.dim.Render(truncate(msg, w)), w))
			} else {
				out = append(out, spaces(w))
			}
		}
		return out
	}
	lines, _ := m.paneLines(p, w)
	total := len(lines)
	scroll := min(max(p.scroll, 0), max(total-h, 0))
	end := total - scroll
	start := max(end-h, 0)
	for i := 0; i < h-(end-start); i++ {
		out = append(out, spaces(w))
	}
	for _, l := range lines[start:end] {
		out = append(out, fit(l, w))
	}
	if p == &m.main && m.histBusy[p.ch] {
		out[0] = fit(m.st.dim.Render(truncate(" loading older messages…", w)), w)
	}
	if scroll > 0 && unread > 0 {
		pill := m.st.pill.Render(truncate(fmt.Sprintf(" ↓ %d new ", unread), w))
		out[h-1] = spaces(w-strWidth(pill)) + pill
	}
	return out
}

// ---- View ----

// View renders the screen: exactly h lines, none wider than w.
func (m *Model) View() string {
	if m.w <= 0 || m.h <= 0 {
		return ""
	}
	d := m.dims()
	main := m.mainColumn(d)
	if !d.side {
		return strings.Join(main, "\n")
	}
	side := m.sidebarLines(sidebarW, m.h)
	bar := m.st.border.Render("│")
	var b strings.Builder
	for i := 0; i < m.h; i++ {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(fit(side[i], sidebarW))
		b.WriteString(bar)
		b.WriteString(main[i])
	}
	return b.String()
}

func (m *Model) mainColumn(d dims) []string {
	w := d.mainW
	out := make([]string, 0, m.h)
	out = append(out, fit(m.headerLine(w), w))
	out = append(out, m.bodyLines(d)...)
	out = append(out, fit(m.statusLine(w), w))
	out = append(out, m.st.border.Render(strings.Repeat("─", w)))
	var in []string
	if m.mode == modeReact {
		in = []string{m.line.View()}
	} else {
		in = strings.Split(m.input.View(), "\n")
	}
	for i := 0; i < d.inputH; i++ {
		l := ""
		if i < len(in) {
			l = in[i]
		}
		out = append(out, fit(l, w))
	}
	// Tiny terminals: keep the bottom (input) and lose the header first.
	if len(out) > m.h {
		out = out[len(out)-m.h:]
	}
	for len(out) < m.h {
		out = append([]string{spaces(w)}, out...)
	}
	return out
}

func (m *Model) bodyLines(d dims) []string {
	w, h := d.mainW, d.bodyH
	if h <= 0 {
		return nil
	}
	switch m.mode {
	case modeHelp:
		return m.helpLines(w, h)
	case modeSwitcher:
		return m.switcherLines(w, h)
	case modeSearch:
		return m.searchLines(w, h)
	case modeActivity:
		return m.activityLines(w, h)
	case modePanePick:
		return m.panePickLines(w, h)
	case modeSharePreview:
		return m.sharePreviewLines(w, h)
	}
	unread := 0
	if v, ok := m.chanView(m.active); ok {
		unread = v.Unread
	}
	switch {
	case d.thrFull:
		return m.threadPane(w, h)
	case d.split:
		left := m.viewPane(&m.main, d.msgW, h, unread)
		right := m.threadPane(d.thrW, h)
		bar := m.st.border.Render("│")
		out := make([]string, h)
		for i := range out {
			out[i] = left[i] + bar + right[i]
		}
		return out
	}
	return m.viewPane(&m.main, w, h, unread)
}

func (m *Model) threadPane(w, h int) []string {
	title := "thread"
	if n := len(m.thread.msgs); n > 0 {
		title = fmt.Sprintf("thread · %d %s", n-1, plural(n-1, "reply", "replies"))
	}
	title += " — Esc to close"
	out := []string{fit(m.st.header.Render(truncate(title, w)), w)}
	return append(out, m.viewPane(&m.thread, w, h-1, 0)...)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// chanTitle is "#name" or "@names" for a DM.
func (m *Model) chanTitle(id uint64) string {
	v, ok := m.chanView(id)
	if !ok {
		return "#?"
	}
	if v.Kind == proto.ChanDM {
		return "@" + sanitize.Line(v.Display)
	}
	return "#" + sanitize.Line(v.Name)
}

func (m *Model) statusText() string {
	if m.status.Stopped {
		return "stopped"
	}
	return m.status.State.String()
}

// statusLine is the notice (if any), then who is typing.
func (m *Model) statusLine(w int) string {
	var b strings.Builder
	if m.notice != "" {
		switch {
		case m.noticeErr:
			b.WriteString(m.st.noticeErr.Render("! " + m.notice))
		case m.noticeTTL == escWindow || m.noticeSticky:
			b.WriteString(m.st.dim.Render(m.notice))
		default:
			b.WriteString(m.st.noticeInfo.Render(m.notice))
		}
	}
	if t := m.typingText(); t != "" {
		if b.Len() > 0 {
			b.WriteString(m.st.dim.Render(" · "))
		}
		b.WriteString(m.st.typing.Render(t))
	}
	return truncate(b.String(), w)
}

// unreadSummary counts unread activity in channels other than the active
// one, for the header when the sidebar is hidden: "3 unread · 1@".
func (m *Model) unreadSummary() string {
	unread, mentions := 0, 0
	for _, v := range m.chans {
		if !v.Member || v.ID == m.active {
			continue
		}
		if v.Mention {
			mentions++
		} else {
			unread += v.Unread
		}
	}
	var parts []string
	if unread > 0 {
		parts = append(parts, strconv.Itoa(unread)+" unread")
	}
	if mentions > 0 {
		parts = append(parts, strconv.Itoa(mentions)+"@")
	}
	return strings.Join(parts, " · ")
}

func (m *Model) headerLine(w int) string {
	left := m.headerLeft(w)
	if m.sidebarShown() {
		return left
	}
	sum := m.unreadSummary()
	if sum == "" {
		return left
	}
	sum = " · " + sum
	sw := strWidth(sum)
	const minLeft = 8 // keep at least the start of the channel name
	if w-sw < minLeft {
		return left
	}
	left = truncate(left, w-sw)
	return left + spaces(w-sw-strWidth(left)) + m.st.dim.Render(sum)
}

func (m *Model) headerLeft(w int) string {
	var b strings.Builder
	if !m.sidebarShown() && m.status.State != client.Online {
		b.WriteString(m.st.status.Render(m.statusText() + " · "))
	}
	if m.active == 0 {
		b.WriteString(m.st.header.Render("tc"))
		return truncate(b.String(), w)
	}
	v, _ := m.chanView(m.active)
	b.WriteString(m.st.header.Render(m.chanTitle(m.active)))
	if n := len(v.Pins); n > 0 {
		b.WriteString(m.st.dim.Render(" 📌 " + strconv.Itoa(n)))
	}
	if v.Kind == proto.ChanDM {
		if others := m.dmOthers(v); len(others) == 1 {
			if p := m.presence[others[0]]; p.State != "" {
				s := p.State
				if p.Status != "" {
					s += ": " + sanitize.Line(p.Status)
				}
				b.WriteString(m.st.topic.Render(" · " + s))
			}
		}
	} else if t := sanitize.Line(v.Topic); t != "" {
		b.WriteString(m.st.topic.Render(" — " + t))
	}
	if !v.Member && v.Kind == proto.ChanPublic {
		b.WriteString(m.st.dim.Render(" (not a member)"))
	}
	return truncate(b.String(), w)
}

func (m *Model) dmOthers(v client.ChannelView) []uint64 {
	var out []uint64
	for _, u := range v.Members {
		if u != m.me.ID {
			out = append(out, u)
		}
	}
	return out
}

func (m *Model) typingText() string {
	var names []string
	for _, u := range m.typing {
		n := sanitize.Line(m.users[u].Name)
		if n == "" {
			n = "someone"
		}
		names = append(names, n)
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0] + " is typing…"
	case 2:
		return names[0] + " and " + names[1] + " are typing…"
	}
	return strconv.Itoa(len(names)) + " people are typing…"
}

// ---- sidebar ----

func (m *Model) sidebarLines(w, h int) []string {
	title := m.st.sideTitle.Render("tc")
	if m.status.State != client.Online || m.status.Stopped {
		title += m.st.status.Render(" · " + m.statusText())
	}
	head := []string{title, ""}
	var lines []string
	active := -1
	section := func(name string, dm bool) {
		first := true
		for _, v := range m.sidebarOrder() {
			if (v.Kind == proto.ChanDM) != dm {
				continue
			}
			if first {
				if len(lines) > 0 {
					lines = append(lines, "")
				}
				lines = append(lines, m.st.sideSection.Render(name))
				first = false
			}
			if v.ID == m.active {
				active = len(lines)
			}
			lines = append(lines, m.sideEntry(v, w))
		}
	}
	section("CHANNELS", false)
	section("DIRECT", true)

	room := h - len(head)
	if room < 0 {
		head = head[:h]
		room = 0
	}
	start := 0
	if len(lines) > room && active >= room {
		start = min(active-room/2, len(lines)-room)
	}
	out := append([]string{}, head...)
	for i := start; i < len(lines) && len(out) < h; i++ {
		out = append(out, lines[i])
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out
}

func (m *Model) sideEntry(v client.ChannelView, w int) string {
	var prefix, name string
	if v.Kind == proto.ChanDM {
		prefix = "○ "
		if others := m.dmOthers(v); len(others) == 1 {
			switch m.presence[others[0]].State {
			case proto.PresOnline:
				prefix = "● "
			case proto.PresAway:
				prefix = "◐ "
			}
		} else {
			prefix = "◆ "
		}
		name = sanitize.Line(v.Display)
	} else {
		prefix = "# "
		name = sanitize.Line(v.Name)
	}
	badge := ""
	switch {
	case v.Mention:
		badge = "@"
	case v.Unread > 99:
		badge = "99+"
	case v.Unread > 0:
		badge = strconv.Itoa(v.Unread)
	}
	// A dim pencil marks a channel (or one of its threads) with an unsent
	// draft that the input isn't showing.
	draft := ""
	if m.hasDraft(v.ID) {
		draft = " ✎"
	}
	labelW := w - 1 - strWidth(draft)
	if badge != "" {
		labelW = w - strWidth(badge) - 2 - strWidth(draft)
	}
	label := fit(truncate(prefix+name, labelW), labelW)
	if v.ID == m.active {
		return m.st.sideActive.Render(fit(label+draft+" "+badge, w-1))
	}
	st := m.st.sideItem
	if v.Unread > 0 || v.Mention {
		st = m.st.sideUnrd
	}
	out := st.Render(label)
	if draft != "" {
		out += m.st.dim.Render(draft)
	}
	if badge != "" {
		bs := m.st.badge
		if v.Mention {
			bs = m.st.mentionBadge
		}
		out += " " + bs.Render(badge)
	}
	return out
}

// ---- overlays ----

func (m *Model) helpMaxScroll() int {
	return max(len(helpLines)-(m.dims().bodyH-1), 0)
}

// helpLines is the scrollable help overlay; a "↓ more" row marks a cut.
func (m *Model) helpLines(w, h int) []string {
	out := []string{fit(m.st.overlayTitle.Render(truncate("help — ↑/↓ PgUp/PgDn scroll, Esc closes", w)), w)}
	kw := 24
	if w < 60 {
		kw = 0
	}
	rows := h - 1
	scroll := min(max(m.helpScroll, 0), max(len(helpLines)-rows, 0))
	items := helpLines[scroll:]
	more := len(items) > rows
	if more {
		items = items[:max(rows-1, 0)]
	}
	for _, kv := range items {
		var l string
		switch {
		case kv[1] == "":
			l = kv[0]
		case kw == 0:
			l = kv[0] + ": " + m.st.dim.Render(kv[1])
		default:
			l = fit(kv[0], kw) + m.st.dim.Render(kv[1])
		}
		out = append(out, fit(l, w))
	}
	if more && rows > 0 {
		out = append(out, fit(m.st.dim.Render(" ↓ more"), w))
	}
	return padLines(out, w, h)
}

func padLines(out []string, w, h int) []string {
	if len(out) > h {
		out = out[:h]
	}
	for len(out) < h {
		out = append(out, spaces(w))
	}
	return out
}

// window returns the [start, end) of n rows that keeps sel visible in h.
func window(n, sel, h int) (int, int) {
	if h <= 0 {
		return 0, 0
	}
	start := 0
	if sel >= h {
		start = sel - h + 1
	}
	return start, min(start+h, n)
}

func (m *Model) switcherLines(w, h int) []string {
	out := []string{fit(m.line.View(), w)}
	if len(m.sw.shown) == 0 && h > 1 {
		out = append(out, fit(m.st.dim.Render(" no matches"), w))
	}
	start, end := window(len(m.sw.shown), m.sw.sel, h-1)
	for i := start; i < end; i++ {
		it := m.sw.shown[i]
		badge := ""
		switch {
		case it.mention:
			badge = "@"
		case it.unread > 0:
			badge = strconv.Itoa(it.unread)
		case it.ch == 0:
			badge = "new"
		}
		l := " " + fit(truncate(it.label, w-strWidth(badge)-3), w-strWidth(badge)-3) + " " + badge + " "
		if i == m.sw.sel {
			out = append(out, fit(m.st.overlaySel.Render(fit(l, w)), w))
		} else {
			out = append(out, fit(l, w))
		}
	}
	return padLines(out, w, h)
}

func (m *Model) searchLines(w, h int) []string {
	s := &m.search
	title := fmt.Sprintf("search “%s” — %d %s · Enter jumps, Esc closes", sanitize.Line(s.q), len(s.res), plural(len(s.res), "result", "results"))
	if s.pins != 0 {
		title = fmt.Sprintf("pinned in %s — %d · Enter jumps, Esc closes", m.chanTitle(s.pins), len(s.res))
		if len(s.res) == 0 {
			title = fmt.Sprintf("nothing pinned in %s · p pins the selected message · Esc closes", m.chanTitle(s.pins))
		}
	}
	out := []string{fit(m.st.overlayTitle.Render(truncate(title, w)), w)}
	start, end := window(len(s.res), s.sel, h-1)
	for i := start; i < end; i++ {
		x := s.res[i]
		body := sanitize.Line(x.Body)
		if x.Deleted {
			body = "message deleted"
		}
		l := fmt.Sprintf(" %s %s %s: %s", m.chanTitle(x.Chan), fmtWhen(x.Time), m.r.userName(x.Author), body)
		if s.pins != 0 {
			l = fmt.Sprintf(" %s · %s", m.r.userName(x.Author), body)
		}
		if i == s.sel {
			out = append(out, fit(m.st.overlaySel.Render(fit(truncate(l, w), w)), w))
		} else {
			out = append(out, fit(truncate(l, w), w))
		}
	}
	return padLines(out, w, h)
}

func (m *Model) panePickLines(w, h int) []string {
	s := &m.share
	out := []string{fit(m.st.overlayTitle.Render(truncate("share a pane — ↑/↓ pick, Enter previews, Esc cancels", w)), w)}
	start, end := window(len(s.panes), s.sel, h-1)
	for i := start; i < end; i++ {
		p := s.panes[i]
		l := " " + p.Name() + m.st.dim.Render("  "+sanitize.Line(p.ID))
		if i == s.sel {
			out = append(out, fit(m.st.overlaySel.Render(fit(truncate(" "+p.Name()+"  "+sanitize.Line(p.ID), w), w)), w))
		} else {
			out = append(out, fit(truncate(l, w), w))
		}
	}
	return padLines(out, w, h)
}

// sharePreviewLines shows exactly the message Enter will send, bottom
// first.
func (m *Model) sharePreviewLines(w, h int) []string {
	s := &m.share
	title := fmt.Sprintf("send to %s? Enter sends · Esc cancels · ↑/↓ scroll", m.chanTitle(s.ch))
	out := []string{fit(m.st.overlayTitle.Render(truncate(title, w)), w)}
	rows := h - 1
	if rows <= 0 {
		return padLines(out, w, h)
	}
	end := len(s.lines) - min(max(s.scroll, 0), max(len(s.lines)-rows, 0))
	start := max(end-rows, 0)
	for _, l := range s.lines[start:end] {
		out = append(out, fit(truncate(sanitize.Line(l), w), w))
	}
	return padLines(out, w, h)
}
