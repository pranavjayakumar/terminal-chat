package tui

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/skailama/tc/internal/alert"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// statusRows is the notice/typing row. It is always reserved so the log
// doesn't jump when someone starts typing or a notice appears.
const statusRows = 1

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// chanTitle is "#name" or "@names" for a DM.
func (m *Model) chanTitle(id uint64) string {
	if m.isAgentDM(id) {
		if a, ok := m.myAgent(); ok {
			return agentName(a.Handle)
		}
		return "your agent"
	}
	return alert.ChanTitle(m.chanView(id))
}

func (m *Model) statusText() string {
	if m.status.Stopped {
		return "stopped"
	}
	return m.status.State.String()
}

// statusLine is the row above the input: an error notice (if any), our
// huddle (unless the huddle bar shows it), a lasting notice, then who is
// typing. Passing notices show as a toast instead. The parts that matter
// most come first so truncation cuts the least important.
func (m *Model) statusLine(w int) string {
	var b strings.Builder
	sep := func() {
		if b.Len() > 0 {
			b.WriteString(m.st.dim.Render(" · "))
		}
	}
	head, roster := m.huddleHead(m.notice != "" || m.al.Ring.Ch != 0 || w < 40)
	if m.huddleBarShown() {
		head, roster = "", ""
	}
	// In a huddle its head (♪ ● = our mic) comes first: a sticky error
	// must never hide that we're broadcasting.
	if m.hud.in && head != "" {
		b.WriteString(head + roster)
		head = ""
	}
	if r := m.ringLine(w < 70 || m.hud.in); r != "" { // time-critical: right after our mic
		sep()
		b.WriteString(m.st.noticeInfo.Render(r))
	}
	if m.notice != "" && m.noticeErr {
		sep()
		b.WriteString(m.st.noticeErr.Render("! " + m.notice))
	}
	if head != "" {
		sep()
		b.WriteString(head + roster)
	}
	if q := m.quietMark(); q != "" {
		sep()
		b.WriteString(m.st.dim.Render(q))
	}
	if m.notice != "" && !m.noticeErr && !m.toastShown() {
		sep()
		switch {
		case m.noticeTTL == escWindow || m.noticeSticky || m.noticeDim:
			b.WriteString(m.st.dim.Render(m.notice))
		default:
			b.WriteString(m.st.noticeInfo.Render(m.notice))
		}
	}
	if t := m.typingText(); t != "" {
		if b.Len() > 0 {
			b.WriteString(m.st.dim.Render(" · "))
		}
		b.WriteString(m.st.typing.Render(strings.TrimSuffix(t, "…") + " " + m.dots()))
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
		} else if !m.muted(v.ID) {
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
	// No sidebar, so no ♪n markers: say a huddle runs elsewhere.
	if n := m.huddlesElsewhere(); n > 0 {
		if sum != "" {
			sum += " · "
		}
		sum += m.icon(icHuddle) + strconv.Itoa(n)
	}
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
	if m.isAgentDM(m.active) {
		// "<icon> Rocky · your agent · only you see this chat"
		if m.agentHeaderRoom() {
			b.WriteString("   ")
		}
		b.WriteString(m.st.header.Render(m.chanTitle(m.active)))
		b.WriteString(m.st.topic.Render(" · your agent · only you see this chat"))
		return truncate(b.String(), w)
	}
	b.WriteString(m.st.header.Render(m.chanTitle(m.active)))
	if m.dmUnverified(v) {
		b.WriteString(m.st.dim.Render("?"))
	}
	if st, ok := m.e2e[m.active]; ok {
		b.WriteString(" 🔒")
		if n := len(m.unverifiedIn(m.active)); n > 0 && v.Kind != proto.ChanDM {
			b.WriteString(m.st.dim.Render(fmt.Sprintf(" ?%d unverified", n)))
		}
		switch st.State {
		case client.E2EWaitingKey:
			b.WriteString(m.st.dim.Render(" waiting for key"))
		case client.E2EUnverified:
			b.WriteString(m.st.dim.Render(" unverified"))
		case client.E2EAlarm:
			b.WriteString(m.st.noticeErr.Render(" ⚠ alarm: " + sanitize.Line(st.Detail)))
		}
	}
	if n := len(v.Pins); n > 0 {
		b.WriteString(m.st.dim.Render(" " + m.icon(icPin) + " " + strconv.Itoa(n)))
	}
	if mm := m.muteMark(m.active); mm != "" {
		b.WriteString(m.st.dim.Render(mm))
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
	if v.Removed {
		b.WriteString(m.st.noticeErr.Render(" — you were removed · /leave to close"))
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

// ---- overlays ----

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
	out := []string{fit(dropBidi(m.line.View()), w)}
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

// dropBidi removes bidi controls from what an editor draws (it keeps its
// own cursor styling, so sanitize.Text can't be used): typed or pasted
// overrides would make the screen differ from what Enter sends. They are
// zero-width, so the cursor math is unchanged.
func dropBidi(s string) string {
	if !strings.ContainsFunc(s, isBidi) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isBidi(r) {
			return -1
		}
		return r
	}, s)
}

func isBidi(r rune) bool { return unicode.Is(unicode.Bidi_Control, r) }
