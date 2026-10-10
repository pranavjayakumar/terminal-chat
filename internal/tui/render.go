package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// ---- width helpers (display cells, never bytes or runes) ----

func strWidth(s string) int { return ansi.StringWidth(s) }

// deletedText is what a deleted message shows (PROTOCOL §13.4): "message
// deleted" only for a delete we can attribute (plaintext channel, or one
// its author signed); one not judged yet is unverified; anything else
// wasn't (provably) the author's.
func deletedText(m client.Message) string {
	switch m.Sec {
	case client.SecPlain, client.SecE2E:
		return "message deleted"
	case client.SecWaiting:
		return "deleted (unverified)"
	}
	return "removed (not by author)"
}

// truncate cuts s (which may contain styling) to at most w cells, ending in
// "…" when something was cut.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if strWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// fit makes s exactly w cells wide: truncated, then padded with spaces.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if sw := strWidth(s); sw > w {
		s = ansi.Truncate(s, w, "")
	}
	if sw := strWidth(s); sw < w {
		s += strings.Repeat(" ", w-sw)
	}
	return s
}

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(" ", n)
}

// splitWidth splits s into a head at most w cells wide (whole grapheme
// clusters) and the rest.
func splitWidth(s string, w int) (head, rest string) {
	used, i := 0, 0
	state := -1
	for i < len(s) {
		c, _, _, st := uniseg.FirstGraphemeClusterInString(s[i:], state)
		cw := strWidth(c)
		if used+cw > w {
			break
		}
		used += cw
		i += len(c)
		state = st
	}
	return s[:i], s[i:]
}

// ---- markdown-lite ----

type flag uint8

const (
	fBold flag = 1 << iota
	fItalic
	fCode
	fURL
	fMention
)

type span struct {
	text string
	f    flag
}

// parseInline splits one line of (sanitized) text into styled spans:
// `code`, **bold**, _italic_, http(s) URLs and @me mentions.
func parseInline(s, me string) []span {
	var out []span
	parseInto(&out, s, 0, me)
	return mergeSpans(out)
}

func parseInto(out *[]span, s string, f flag, me string) {
	var plain strings.Builder
	emit := func(sp span) {
		if plain.Len() > 0 {
			*out = append(*out, span{plain.String(), f})
			plain.Reset()
		}
		if sp.text != "" {
			*out = append(*out, sp)
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		prevWord := i > 0 && isWordByte(s[i-1])
		switch {
		case c == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j > 0 {
				emit(span{s[i+1 : i+1+j], f | fCode})
				i += j + 2
				continue
			}
		case c == '*' && strings.HasPrefix(s[i:], "**"):
			if j := strings.Index(s[i+2:], "**"); j > 0 {
				emit(span{})
				parseInto(out, s[i+2:i+2+j], f|fBold, me)
				i += j + 4
				continue
			}
		case c == '_' && !prevWord:
			if j := closingUnderscore(s[i+1:]); j > 0 {
				emit(span{})
				parseInto(out, s[i+1:i+1+j], f|fItalic, me)
				i += j + 2
				continue
			}
		case (c == 'h' || c == 'H') && !prevWord && hasURLPrefix(s[i:]):
			end := i
			for end < len(s) && s[end] != ' ' && s[end] != '\n' {
				end++
			}
			for end > i && strings.ContainsRune(".,;:!?)]}'\"", rune(s[end-1])) {
				end--
			}
			emit(span{s[i:end], f | fURL})
			i = end
			continue
		case c == '@' && !prevWord && me != "":
			if n := len(me) + 1; len(s) >= i+n && strings.EqualFold(s[i+1:i+n], me) &&
				(i+n == len(s) || !(isWordByte(s[i+n]) || s[i+n] == '-')) {
				emit(span{s[i : i+n], f | fMention})
				i += n
				continue
			}
		}
		plain.WriteByte(c)
		i++
	}
	emit(span{})
}

func hasURLPrefix(s string) bool {
	l := strings.ToLower(s[:min(len(s), 8)])
	return (strings.HasPrefix(l, "https://") && len(s) > 8) || (strings.HasPrefix(l, "http://") && len(s) > 7)
}

// closingUnderscore finds the "_" that closes an italic run in s: not
// followed by a word character, and not preceded by a space.
func closingUnderscore(s string) int {
	for j := 1; j < len(s); j++ {
		if s[j] == '_' && s[j-1] != ' ' && (j+1 == len(s) || !isWordByte(s[j+1])) {
			return j
		}
	}
	return -1
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

func mergeSpans(in []span) []span {
	var out []span
	for _, sp := range in {
		if sp.text == "" {
			continue
		}
		if n := len(out); n > 0 && out[n-1].f == sp.f {
			out[n-1].text += sp.text
			continue
		}
		out = append(out, sp)
	}
	return out
}

// mentionsMe reports whether body contains @name as a whole word,
// case-insensitively (the same rule the client uses for unread mentions).
func mentionsMe(body, name string) bool {
	if name == "" {
		return false
	}
	lb, pat := strings.ToLower(body), "@"+strings.ToLower(name)
	for i := 0; i < len(lb); {
		j := strings.Index(lb[i:], pat)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(pat)
		if (j == 0 || !isWordByte(lb[j-1])) && (end == len(lb) || !(isWordByte(lb[end]) || lb[end] == '-')) {
			return true
		}
		i = j + 1
	}
	return false
}

// ---- wrapping ----

// splitWords splits s into alternating runs of spaces and non-spaces.
func splitWords(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		j := i
		sp := s[i] == ' '
		for j < len(s) && (s[j] == ' ') == sp {
			j++
		}
		out = append(out, s[i:j])
		i = j
	}
	return out
}

// wrapSpans word-wraps spans to lines of at most w cells. Words wider than
// w are broken at grapheme boundaries. Spaces at the start of a wrapped
// continuation line and at the end of any line are dropped.
func wrapSpans(spans []span, w int) [][]span {
	if w < 1 {
		w = 1
	}
	var lines [][]span
	var cur []span
	cw := 0
	add := func(t string, f flag) {
		if n := len(cur); n > 0 && cur[n-1].f == f {
			cur[n-1].text += t
		} else {
			cur = append(cur, span{t, f})
		}
		cw += strWidth(t)
	}
	flush := func() {
		for n := len(cur); n > 0; n = len(cur) {
			t := strings.TrimRight(cur[n-1].text, " ")
			if t != "" {
				cur[n-1].text = t
				break
			}
			cur = cur[:n-1]
		}
		lines = append(lines, cur)
		cur, cw = nil, 0
	}
	for _, sp := range spans {
		for _, tok := range splitWords(sp.text) {
			tw := strWidth(tok)
			if tok[0] == ' ' {
				if cw == 0 && len(lines) > 0 {
					continue
				}
				if cw+tw > w {
					flush()
					continue
				}
				add(tok, sp.f)
				continue
			}
			if cw+tw <= w {
				add(tok, sp.f)
				continue
			}
			if tw <= w {
				flush()
				add(tok, sp.f)
				continue
			}
			for tok != "" {
				if cw >= w {
					flush()
				}
				head, rest := splitWidth(tok, w-cw)
				if head == "" {
					if cw > 0 {
						flush()
						continue
					}
					// One cluster wider than the whole line (w == 1, wide
					// rune): emit it anyway, the final fit truncates.
					head, rest, _, _ = uniseg.FirstGraphemeClusterInString(tok, -1)
				}
				add(head, sp.f)
				tok = rest
			}
		}
	}
	if len(cur) > 0 || len(lines) == 0 {
		flush()
	}
	return lines
}

// ---- messages ----

// msgLayout is the column layout for a given pane width. Below 50 columns
// the author goes on its own line so the body keeps most of the width.
type msgLayout struct {
	stacked bool
	nameW   int
	indent  int
}

func layoutFor(w int) msgLayout {
	if w < 50 {
		return msgLayout{stacked: true, indent: 2}
	}
	nameW := 8
	if w >= 80 {
		nameW = 10
	}
	return msgLayout{nameW: nameW, indent: 6 + nameW + 2}
}

// renderer turns messages into styled lines. It holds no I/O: names come
// from a lookup over data already loaded into the model.
type renderer struct {
	st   *styles
	me   proto.User
	name func(id uint64) string
	bot  func(id uint64) bool // webhook authors get a dim "bot" tag
}

func (r *renderer) userName(id uint64) string {
	n := ""
	if r.name != nil {
		n = sanitize.Line(r.name(id))
	}
	if n == "" {
		n = "user" + strconv.FormatUint(id, 10)
	}
	return n
}

// message renders m to lines at most w cells wide. header shows time and
// author; grouped follow-ups pass false.
func (r *renderer) message(m client.Message, w int, header bool) []string {
	lay := layoutFor(w)
	bw := max(w-lay.indent, 1)
	body := r.body(m, bw)
	if extra := r.footer(m, bw); extra != "" {
		body = append(body, extra)
	}
	if m.Failed != "" {
		f := r.st.failed.Render("! failed — r to retry, d to discard")
		body = append(body, truncate(f+r.st.dim.Render(" ("+sanitize.Line(m.Failed)+")"), bw))
	}

	out := make([]string, 0, len(body)+1)
	ind := spaces(lay.indent)
	if header {
		// A verified message whose server time is far from its signed
		// time shows the signed one (footer says so: driftMark).
		ts := r.st.time.Render(time.UnixMilli(m.ShownTime()).Local().Format("15:04"))
		name := r.userName(m.Author)
		as := r.st.author(m.Author)
		isBot := r.bot != nil && r.bot(m.Author)
		if lay.stacked {
			label := as.Render(name)
			if isBot {
				label += r.st.dim.Render(botTag)
			}
			out = append(out, truncate(ts+" "+label, w))
		} else {
			first := ts + " " + r.nameCell(as, name, lay.nameW, isBot) + "  "
			out = append(out, first+body[0])
			body = body[1:]
		}
	}
	for _, l := range body {
		out = append(out, ind+l)
	}
	for i, l := range out {
		// Only reachable at absurd widths (a wide rune in a 1-cell column).
		if strWidth(l) > w {
			out[i] = truncate(l, w)
		}
	}
	return out
}

// body renders the message text (or its deleted marker) to lines of at
// most bw cells.
func (r *renderer) body(m client.Message, bw int) []string {
	st := r.st
	if m.Deleted {
		return []string{st.deleted.Render(truncate(deletedText(m), bw))}
	}
	// Encrypted channels: what can't be shown, and plaintext that may not
	// be what its author wrote (PROTOCOL §13.4).
	var out []string
	switch m.Sec {
	case client.SecWaiting:
		return []string{st.dim.Render(truncate("🔒 waiting for key", bw))}
	case client.SecInvalid:
		return []string{st.failed.Render(truncate("⚠ invalid encrypted message (hidden)", bw))}
	case client.SecInserted:
		out = append(out, st.failed.Render(truncate("⚠ inserted by server", bw)))
	case client.SecLegacy:
		out = append(out, st.failed.Render(truncate("⚠ unauthenticated (legacy)", bw)))
	case client.SecLegacyClaimed:
		out = append(out, st.failed.Render(truncate("⚠ unauthenticated (legacy, server-claimed)", bw)))
	}
	text := sanitize.Text(m.Body)
	lastCode := false
	for _, sg := range splitFences(strings.Split(text, "\n")) {
		if sg.code {
			bar := st.codeBar.Render("┃") + " "
			for _, l := range r.codeLines(sg, bw-2, m.Pending) {
				out = append(out, bar+l)
			}
			if len(sg.lines) > 0 {
				lastCode = true
			}
			continue
		}
		for _, ln := range sg.lines {
			lastCode = false
			for _, l := range wrapSpans(parseInline(ln, r.me.Name), bw) {
				var b strings.Builder
				for _, sp := range l {
					b.WriteString(r.style(sp.f, m.Pending).Render(sp.text))
				}
				out = append(out, b.String())
			}
		}
	}
	if len(out) == 0 {
		out = []string{""}
	}
	suffix := func(e string) {
		if n := len(out) - 1; !lastCode && strWidth(out[n])+1+strWidth(e) <= bw {
			if out[n] != "" {
				out[n] += " "
			}
			out[n] += e
		} else {
			out = append(out, e)
		}
	}
	if m.Edited != 0 {
		suffix(st.edited.Render(truncate("(edited)", bw)))
	}
	return out
}

func (r *renderer) style(f flag, pending bool) lipgloss.Style {
	st := r.st
	s := st.base
	switch {
	case f&fMention != 0:
		s = st.mention
	case f&fURL != 0:
		s = st.url
	case f&fCode != 0:
		s = st.code
	}
	if f&fBold != 0 {
		s = s.Bold(true)
	}
	if f&fItalic != 0 {
		s = s.Italic(true)
	}
	if pending {
		s = s.Faint(true)
	}
	return s
}

// footer is the reactions + reply count line, or "".
func (r *renderer) footer(m client.Message, bw int) string {
	var parts []string
	if !m.Deleted && len(m.Reactions) > 0 {
		keys := make([]string, 0, len(m.Reactions))
		for k, users := range m.Reactions {
			if len(users) > 0 && sanitize.Line(k) != "" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var rs []string
		for _, k := range keys {
			users := m.Reactions[k]
			s := r.st.reaction
			for _, u := range users {
				if u == r.me.ID {
					s = r.st.reactionMine
					break
				}
			}
			rs = append(rs, s.Render(sanitize.Line(k)+" "+strconv.Itoa(len(users))))
		}
		if len(rs) > 0 {
			parts = append(parts, strings.Join(rs, "  "))
		}
	}
	if m.Replies > 0 {
		word := "replies"
		if m.Replies == 1 {
			word = "reply"
		}
		parts = append(parts, r.st.replies.Render(fmt.Sprintf("↳ %d %s", m.Replies, word)))
	}
	if m.Pinned && !m.Deleted {
		parts = append(parts, r.st.dim.Render("📌"))
	}
	if m.TimeDrift() && !m.Deleted {
		parts = append(parts, r.st.dim.Render(driftMark))
	}
	if len(parts) == 0 {
		return ""
	}
	return truncate(strings.Join(parts, "   "), bw)
}

// daySep is the "── Tue 9 Oct ──" line between days.
func (r *renderer) daySep(t time.Time, w int) string {
	f := "Mon 2 Jan"
	if t.Year() != time.Now().Year() {
		f = "Mon 2 Jan 2006"
	}
	return r.st.sep.Render(truncate("── "+t.Format(f)+" ──", w))
}

// sameDay reports whether two unix-ms times fall on the same local day.
func sameDay(a, b int64) bool {
	ta, tb := time.UnixMilli(a).Local(), time.UnixMilli(b).Local()
	return ta.Year() == tb.Year() && ta.YearDay() == tb.YearDay()
}

const groupWindow = 5 * 60 * 1000 // ms

// driftMark follows a verified message whose signed time (shown) differs
// from the server's by more than client.MaxTimeDrift.
const driftMark = "⚠ server time differs"

// grouped reports whether cur continues prev's group (same author, same
// day, within five minutes) and so omits its time/author.
// A message with replies or reactions ends its group so its footer clearly
// belongs to it.
func grouped(prev, cur client.Message) bool {
	if prev.Replies > 0 || prev.Pinned || hasReactions(prev) || prev.TimeDrift() || cur.TimeDrift() {
		return false // a drifting message always shows its (signed) time
	}
	return prev.Author == cur.Author && cur.Time-prev.Time < groupWindow && cur.Time >= prev.Time &&
		sameDay(prev.Time, cur.Time) && prev.Failed == ""
}

func hasReactions(x client.Message) bool {
	for _, us := range x.Reactions {
		if len(us) > 0 {
			return true
		}
	}
	return false
}

// renderKey identifies one rendered message; everything that changes its
// lines is in it.
type renderKey struct {
	id      uint64
	local   int64
	edited  int64
	deleted bool
	pending bool
	failed  string
	replies int
	pinned  bool
	reacts  string
	width   int
	header  bool
	sec     client.Sec // a waiting message changes when it decrypts
}

func keyFor(m client.Message, w int, header bool) renderKey {
	k := renderKey{id: m.ID, local: m.Local, edited: m.Edited, deleted: m.Deleted, pending: m.Pending,
		failed: m.Failed, replies: m.Replies, pinned: m.Pinned, width: w, header: header, sec: m.Sec}
	if len(m.Reactions) > 0 {
		keys := make([]string, 0, len(m.Reactions))
		for e, us := range m.Reactions {
			keys = append(keys, fmt.Sprint(e, us))
		}
		sort.Strings(keys)
		k.reacts = strings.Join(keys, ";")
	}
	return k
}

// renderCache memoizes message lines. It is cleared when names change.
type renderCache struct {
	m map[renderKey][]string
}

const maxCache = 4000

func (c *renderCache) get(r *renderer, m client.Message, w int, header bool) []string {
	k := keyFor(m, w, header)
	if l, ok := c.m[k]; ok {
		return l
	}
	if c.m == nil || len(c.m) > maxCache {
		c.m = map[renderKey][]string{}
	}
	l := r.message(m, w, header)
	c.m[k] = l
	return l
}

func (c *renderCache) reset() { c.m = nil }
