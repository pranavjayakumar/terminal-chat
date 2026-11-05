package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/sanitize"
)

// Approvals. The harness (internal/agent/approvals.go) posts a card
//
//	✋ Rocky wants to post in #general  ·  #<code>
//	why: bob asked in #general: "…"
//	> exactly what it will post, every line
//
// (older harnesses: `✋ @<owner> approve: <action>? react ✅ / ❌  ·
// #<code>` and a detail) in the thread and waits for its owner to react ✅
// (yes) or ❌ (no) on it. tc draws the card as a framed prompt: who wants
// what, the preview, why, then `1 post it · 2 post, and don't ask again
// today for #general · 3 don't`. Keys 1–3 or a click answer it with
// reactions, and only the owner's answer counts (the harness reads the
// owner's reactions only; everyone else sees "waiting for <owner>").
// Choice 2 reacts ✅ and ♾️: the harness takes the ✅ as yes and the ♾️ as
// "the same kind of action on the same target is fine for today".
//
// The preview is never cut: every quoted line is drawn, wrapped to the
// width. A preview too long for one message goes on in the next messages
// ("#<code> (2/3)"), and the card's `full:` line (size and sha256) says so.
// Those continuations draw as part of the card (verbatim, never as
// markdown), and the choices wait until the last part is in.

const (
	apprYes   = "✅"
	apprNo    = "❌"
	apprToday = "♾️"
)

type approvalCard struct {
	owner  string // old cards only
	agent  string // "Rocky" (new cards)
	action string // "post in #general"
	code   string
	detail string   // old cards: free text under the card
	quote  []string // new cards: the preview
	why    string
	full   string // "N lines · N bytes · sha256 … · continued in the next N messages"
}

// apprState is a card's answer as its block shows it.
type apprState uint8

// apprInfo is what a block needs to draw a card or one of its
// continuations: the answer, and for a card in parts how many are in.
type apprInfo struct {
	st   apprState
	cont bool // a continuation ("#code (part/of)") of a card above
	part int  // continuation: which part; card: the first part still missing (0: none)
	of   int  // parts in all (0 or 1: one message)
}

const (
	apprPending apprState = iota
	apprSaidYes
	apprSaidToday
	apprSaidNo
	apprExpired
)

// parseApproval reads an agent's approval card. Only a verified message
// counts: in encrypted channels the agent's device signed it.
func parseApproval(x client.Message) (approvalCard, bool) {
	if !x.FromAgent() || x.Deleted || !x.Sec.Trusted() {
		return approvalCard{}, false
	}
	first, rest, _ := strings.Cut(x.Body, "\n")
	head, ok := strings.CutPrefix(first, "✋ ")
	if !ok {
		return approvalCard{}, false
	}
	if old, ok := strings.CutPrefix(head, "@"); ok {
		return parseOldApproval(old, rest)
	}
	// Who asks is the authenticated sender (Message.agent), never the body's
	// claim: an agent could otherwise word a card as another's.
	_, what, ok := strings.Cut(head, " wants to ")
	if !ok {
		return approvalCard{}, false
	}
	j := strings.LastIndex(what, "  ·  ")
	if j < 0 {
		return approvalCard{}, false
	}
	code := strings.TrimSpace(what[j+len("  ·  "):])
	if !strings.HasPrefix(code, "#") || strings.ContainsAny(code, " \t") {
		return approvalCard{}, false
	}
	c := approvalCard{agent: agentTitle(x.Agent), action: strings.TrimSpace(what[:j]), code: code}
	if rest != "" {
		for _, l := range strings.Split(rest, "\n") {
			switch {
			case strings.HasPrefix(l, "> "):
				c.quote = append(c.quote, l[2:])
			case strings.HasPrefix(l, "why: "):
				c.why = l[len("why: "):]
			case strings.HasPrefix(l, "full: "):
				c.full = l[len("full: "):]
			}
		}
	}
	return c, true
}

func parseOldApproval(head, detail string) (approvalCard, bool) {
	owner, rest, ok := strings.Cut(head, " approve: ")
	if !ok {
		return approvalCard{}, false
	}
	i := strings.LastIndex(rest, "? react ")
	j := strings.LastIndex(rest, "·")
	if i < 0 || j < i {
		return approvalCard{}, false
	}
	code := strings.TrimSpace(rest[j+len("·"):])
	if !strings.HasPrefix(code, "#") {
		return approvalCard{}, false
	}
	return approvalCard{owner: owner, action: strings.TrimSpace(rest[:i]), code: code, detail: detail}, true
}

// agentTitle is how cards name agent handle h ("rocky" -> "Rocky").
func agentTitle(h string) string {
	if h == "" {
		return "Your agent"
	}
	r, n := utf8.DecodeRuneInString(h)
	return string(unicode.ToUpper(r)) + h[n:]
}

// title is the card's first line: who wants what.
func (c approvalCard) title() string {
	if c.agent == "" {
		return sanitize.Line(c.action) + "?"
	}
	return sanitize.Line(c.agent) + " wants to " + sanitize.Line(c.action)
}

// done is the action once approved: "posted in #general".
func (c approvalCard) done() string {
	a := sanitize.Line(c.action)
	if t, ok := strings.CutPrefix(a, "post in "); ok {
		return "posted in " + t
	}
	if t, ok := strings.CutPrefix(a, "open a "); ok {
		return t
	}
	return a
}

// verdict is the owner's answer on card x: ❌ wins over ✅, as in the
// harness (agent.Verdict).
func verdict(x client.Message) apprState {
	has := func(es ...string) bool {
		for _, e := range es {
			if slices.Contains(x.Reactions[e], x.Author) {
				return true
			}
		}
		return false
	}
	switch {
	case has(apprNo, "👎"):
		return apprSaidNo
	case has(apprYes, "👍") && has(apprToday, "♾"):
		return apprSaidToday
	case has(apprYes, "👍"):
		return apprSaidYes
	}
	return apprPending
}

// parts is how many messages the card comes in: the harness's `full:`
// line ends "continued in the next N messages".
func (c approvalCard) parts() int {
	const pre, suf = "continued in the next ", " messages"
	i := strings.LastIndex(c.full, pre)
	if i < 0 || !strings.HasSuffix(c.full, suf) {
		return 1
	}
	n, err := strconv.Atoi(c.full[i+len(pre) : len(c.full)-len(suf)])
	if err != nil || n < 1 || n > 1000 {
		return 1
	}
	return n + 1
}

// contPart is which part of card (from x) message y carries: its first line
// is "#code (i/n)", from the same agent in the same thread (0: none).
func contPart(card approvalCard, n int, x, y client.Message) int {
	if !y.FromAgent() || y.Deleted || !y.Sec.Trusted() || y.Author != x.Author || y.Agent != x.Agent || y.Thread != x.Thread {
		return 0
	}
	first, _, _ := strings.Cut(y.Body, "\n")
	rest, ok := strings.CutPrefix(first, card.code+" (")
	if !ok {
		return 0
	}
	var i, of int
	if k, err := fmt.Sscanf(rest, "%d/%d)", &i, &of); err != nil || k != 2 || of != n || i < 2 || i > n ||
		rest != fmt.Sprintf("%d/%d)", i, of) {
		return 0
	}
	return i
}

// approvalStates is every card's state in msgs (answered, or given up on:
// "⌛ no answer on #code" from the same agent) and its continuations'.
func (m *Model) approvalStates(msgs []client.Message) map[uint64]apprInfo {
	var out map[uint64]apprInfo
	for i, x := range msgs {
		card, ok := parseApproval(x)
		if !ok {
			continue
		}
		if out == nil {
			out = map[uint64]apprInfo{}
		}
		st := verdict(x)
		n := card.parts()
		var have []bool
		if n > 1 {
			have = make([]bool, n+1)
		}
		var conts []uint64
		for _, y := range msgs[i+1:] {
			if st == apprPending && y.FromAgent() && y.Author == x.Author && strings.Contains(y.Body, "no answer on "+card.code) {
				st = apprExpired
			}
			if n > 1 {
				if k := contPart(card, n, x, y); k > 0 && !have[k] {
					have[k] = true
					conts = append(conts, y.ID)
					out[y.ID] = apprInfo{cont: true, part: k, of: n}
				}
			}
		}
		for _, id := range conts {
			ci := out[id]
			ci.st = st
			out[id] = ci
		}
		info := apprInfo{st: st}
		if n > 1 {
			info.of = n
			for k := 2; k <= n; k++ {
				if !have[k] {
					info.part = k
					break
				}
			}
		}
		out[x.ID] = info
		if st != apprPending {
			delete(m.ag.pending, x.ID)
		}
	}
	return out
}

// apprInfoOf is card x's info, from whichever pane holds it.
func (m *Model) apprInfoOf(x client.Message) apprInfo {
	for _, p := range []*pane{&m.thread, &m.main} {
		for _, y := range p.msgs {
			if y.ID == x.ID {
				return m.approvalStates(p.msgs)[x.ID]
			}
		}
	}
	return apprInfo{st: verdict(x)}
}

// waitingPart is "waiting for part i/n" while card info isn't all in.
func (a apprInfo) waitingPart() string {
	if a.part == 0 {
		return ""
	}
	return fmt.Sprintf("waiting for part %d/%d", a.part, a.of)
}

// apprChoice is one answer: its key, its label on the card, a short label
// for the footer, and the reactions it sends.
type apprChoice struct{ key, label, short, react string }

// apprChoices are the card's choices, worded for its kind of action.
func apprChoices(action string) [3]apprChoice {
	yes, again, no := "yes", "yes, and don't ask again today", "no"
	switch {
	case strings.HasPrefix(action, "post in "):
		yes, no = "post it", "don't"
		again = "post, and don't ask again today for " + strings.TrimPrefix(action, "post in ")
	case strings.HasPrefix(action, "open a draft PR"), strings.Contains(action, "draft PR"):
		yes, no = "open it", "don't"
		again = "open, and don't ask again today for draft PRs"
		// The harness grants a CI-changing PR on its own target ("tc
		// .github"); the title after ": " is the agent's words, not this.
		if head, _, _ := strings.Cut(action, ": "); strings.Contains(head, "CHANGES CI") {
			again = "open, and don't ask again today for PRs that change CI"
		}
	case strings.HasPrefix(action, "open a GitHub issue"):
		yes, no = "open it", "don't"
		again = "open, and don't ask again today for issues"
	}
	return [3]apprChoice{
		{"1", yes, yes, apprYes},
		{"2", again, "ok all today", apprYes + " " + apprToday},
		{"3", no, no, apprNo},
	}
}

// drawApproval draws a card as a block of its own: who wants what, the
// preview, why, then the choices (the owner), a wait line (everyone
// else); once answered it folds to one line.
func (m *Model) drawApproval(g *grower, x client.Message, card approvalCard, info apprInfo, bw int, at func(int) int, textFg rgb) {
	th := m.st.th
	st := info.st
	owner := m.ownerName(x.Author)
	mine := x.Author == m.me.ID
	who := owner
	if mine {
		who = "you"
	}
	switch st {
	case apprSaidYes, apprSaidToday:
		said := who + " approved"
		if st == apprSaidToday {
			said += " for today"
		}
		y := g.row()
		e := g.c.put(2, y, "✓", fgb(th.ok), "")
		g.c.put(e+1, y, truncate(said+" · "+card.done(), bw-2), fgc(th.dim), "")
		return
	case apprSaidNo:
		y := g.row()
		e := g.c.put(2, y, "✗", fgb(th.err), "")
		g.c.put(e+1, y, truncate(who+" said no · "+sanitize.Line(card.action), bw-2), fgc(th.dim), "")
		return
	case apprExpired:
		y := g.row()
		g.c.put(2, y, truncate("⌛ no answer · "+sanitize.Line(card.action), bw), fgc(th.faint), "")
		return
	}
	if d := strings.TrimSpace(card.detail); d != "" {
		y := x
		y.Body, y.Edited = d, 0
		m.drawText(g, y, bw, at, textFg)
	}
	m.prose(g, card.title(), bw, fgb(th.fg))
	m.drawQuote(g, card.quote, bw)
	if card.full != "" {
		for _, part := range wrapCells("⚠ long: "+sanitize.Line(card.full), bw) {
			g.c.put(2, g.row(), part, fgc(th.warn), "")
		}
	}
	if card.why != "" {
		m.prose(g, "why: "+sanitize.Line(card.why), bw, fgc(th.faint))
	}
	if !mine {
		m.prose(g, "waiting for "+owner, bw, fgc(th.faint))
		return
	}
	if w := info.waitingPart(); w != "" {
		m.prose(g, w, bw, fgc(th.faint))
		return
	}
	// the three choices as pills, wrapping in a narrow column
	y := g.row()
	xx := 2
	for i, ch := range apprChoices(card.action) {
		label := " " + ch.key + " " + ch.label + " "
		if i > 0 && xx+strWidth(label)+2 > 2+bw {
			y = g.row()
			xx = 2
		}
		st := fgc(th.fg).on(th.element)
		if ch.key == "1" {
			st = fgb(th.accentFg).on(th.accent).with(aMonoRev)
		}
		xx = g.c.pill(xx, y, truncate(label, bw-2), st, "appr|"+ch.key) + 1
	}
	m.prose(g, "only you can answer · select it, then 1–3", bw, fgc(th.faint))
}

// prose draws s (already sanitized) wrapped at spaces to width bw; a word
// longer than the width is cut into pieces, never lost.
func (m *Model) prose(g *grower, s string, bw int, st cstyle) {
	bw = max(bw, 1)
	for _, row := range wrapWords(s, bw) {
		for _, part := range wrapCells(row, bw) {
			g.c.put(2, g.row(), part, st, "")
		}
	}
}

// drawQuote draws a card's preview lines verbatim: a ▏ gutter on each line,
// a ┊ on the rows a long line goes on in (so a piece starting with + or -
// doesn't read as a diff line). Warnings (⚠ CHANGES CI) stand out.
func (m *Model) drawQuote(g *grower, quote []string, bw int) {
	th := m.st.th
	for _, q := range quote {
		q = sanitize.Line(q)
		st := fgc(th.dim)
		if strings.HasPrefix(q, "⚠") {
			st = fgb(th.warn)
		}
		for i, part := range wrapCells(q, bw-2) {
			yy := g.row()
			gut := "▏"
			if i > 0 {
				gut = "┊"
			}
			e := g.c.put(2, yy, gut, fgc(th.faint), "")
			g.c.put(e+1, yy, part, st, "")
		}
	}
}

// drawApprovalPart draws a continuation of a card ("#code (2/3)" and more
// preview lines) as the card's: verbatim, no markdown. Once the card is
// answered it folds to one line, like the card.
func (m *Model) drawApprovalPart(g *grower, x client.Message, info apprInfo, bw int) {
	th := m.st.th
	first, rest, _ := strings.Cut(x.Body, "\n")
	head := sanitize.Line(first) + " · part " + strconv.Itoa(info.part) + " of " + strconv.Itoa(info.of)
	if info.st != apprPending {
		g.c.put(2, g.row(), truncate(head, bw), fgc(th.faint), "")
		return
	}
	m.prose(g, head, bw, fgc(th.faint))
	var quote []string
	for _, l := range strings.Split(rest, "\n") {
		if q, ok := strings.CutPrefix(l, "> "); ok {
			quote = append(quote, q)
		} else if l == ">" {
			quote = append(quote, "")
		}
	}
	m.drawQuote(g, quote, bw)
}

// wrapCells cuts s into pieces of at most w cells, keeping every character
// and its spacing (a diffstat or a command must read as written): joined,
// the pieces are s. A piece ends after a space when there is one in its
// last third, so words and hashes stay whole where they can.
func wrapCells(s string, w int) []string {
	w = max(w, 1)
	if s == "" {
		return []string{""}
	}
	var out []string
	for s != "" {
		head, rest := splitWidth(s, w)
		if head == "" {
			_, n := utf8.DecodeRuneInString(s)
			head, rest = s[:n], s[n:]
		}
		if rest != "" {
			if i := strings.LastIndexByte(head, ' '); i >= 0 && strWidth(head[:i+1]) > w-w/3 {
				head, rest = s[:i+1], s[i+1:]
			}
		}
		out = append(out, head)
		s = rest
	}
	return out
}

// answerApproval reacts choice ("1"–"3") on card x, if it is my agent's
// and still waits.
func (m *Model) answerApproval(x client.Message, choice string) tea.Cmd {
	card, ok := parseApproval(x)
	if !ok || x.ID == 0 {
		return nil
	}
	if x.Author != m.me.ID {
		m.setInfo("only " + m.ownerName(x.Author) + " can answer")
		return nil
	}
	if verdict(x) != apprPending {
		m.setInfo("already answered")
		return nil
	}
	if w := m.apprInfoOf(x).waitingPart(); w != "" {
		m.setInfo(w + " · read all of it first")
		return nil
	}
	var emojis []string
	said := ""
	for _, ch := range apprChoices(card.action) {
		if ch.key == choice {
			emojis = strings.Fields(ch.react)
			said = ch.label
		}
	}
	if emojis == nil {
		return nil
	}
	delete(m.ag.pending, x.ID)
	id := x.ID
	return m.callThen("answer", func(ctx context.Context) (tea.Msg, error) {
		for _, e := range emojis {
			if err := m.c.React(ctx, id, e); err != nil {
				return nil, err
			}
		}
		return infoMsg("you said " + said + " · " + card.title()), nil
	})
}

// parseApprovalAction is the action of card x ("" when it isn't one).
func parseApprovalAction(x client.Message) string {
	c, _ := parseApproval(x)
	return c.action
}

// approvalKey answers the selected card with 1–3. Only a card selected on
// purpose takes digits: typing "1pm works" in the composer never answers.
func (m *Model) approvalKey(key string) (tea.Cmd, bool) {
	if (key != "1" && key != "2" && key != "3") || m.mode != modeSelect {
		return nil, false
	}
	x, ok := m.selected()
	if !ok {
		return nil, false
	}
	if _, card := parseApproval(x); !card || x.Author != m.me.ID {
		return nil, false
	}
	return m.answerApproval(x, key), true
}

// jumpToApproval (⌥O, the palette's "Approval waiting" row) opens the
// newest card waiting for me and selects it, so 1–3 answer it.
func (m *Model) jumpToApproval() tea.Cmd {
	p, id, ok := m.waitingApproval()
	if !ok {
		m.setInfo("no approval waiting")
		return nil
	}
	cmd := m.openApproval()
	pn := &m.main
	if p.thread != 0 {
		pn = &m.thread
	}
	for i, x := range pn.msgs {
		if x.ID == id {
			m.mode = modeSelect
			pn.sel = i
			m.ensureVisible(pn)
			m.updatePlaceholder()
		}
	}
	return cmd
}

// ---- the ping: an approval waits somewhere I'm not looking ----

type pendingAppr struct {
	ch, thread uint64
	at         time.Time
	action     string
}

// apprTimeout matches the harness's wait: after it the card is moot.
const apprTimeout = 30 * time.Minute

// noteApprovals remembers my agent's new cards and pings me about the
// ones outside the channel I'm looking at: my own agent posts as me, so
// the usual mention notification never fires for it.
func (m *Model) noteApprovals(news []client.Message) tea.Cmd {
	var cmd tea.Cmd
	for _, x := range news {
		card, ok := parseApproval(x)
		if !ok || x.Author != m.me.ID || !x.Sec.Trusted() || verdict(x) != apprPending {
			continue
		}
		if m.ag.pending == nil {
			m.ag.pending = map[uint64]pendingAppr{}
		}
		m.ag.pending[x.ID] = pendingAppr{x.Chan, x.Thread, m.now(), card.action}
		looking := x.Chan == m.active && m.focused && (x.Thread == 0 || x.Thread == m.thread.root)
		if looking || m.opt.Notifier == nil {
			continue
		}
		a := m.agentOfMsg(x)
		n := notify.Note{Title: agentName(a.Handle) + " needs your ok", Body: card.title(), Tag: "tc-agent"}
		send := m.opt.Notifier
		cmd = func() tea.Msg { send.Send(n); return noopMsg{} }
	}
	return cmd
}

// myApprovalCard: x is a verified approval card from my own agent.
func (m *Model) myApprovalCard(x client.Message) bool {
	_, ok := parseApproval(x)
	return ok && x.Author == m.me.ID
}

// waitingApproval is the newest card still waiting for me.
func (m *Model) waitingApproval() (pendingAppr, uint64, bool) {
	var best pendingAppr
	var id uint64
	for k, p := range m.ag.pending {
		if m.now().Sub(p.at) > apprTimeout {
			delete(m.ag.pending, k)
			continue
		}
		if p.at.After(best.at) || id == 0 {
			best, id = p, k
		}
	}
	return best, id, id != 0
}

// openApproval opens the newest waiting card's channel (and thread).
func (m *Model) openApproval() tea.Cmd {
	p, _, ok := m.waitingApproval()
	if !ok {
		return nil
	}
	m.jumping = true
	cmd := m.openChannel(p.ch)
	m.jumping = false
	if p.thread != 0 {
		cmd = tea.Batch(cmd, m.openThread(p.thread))
		m.layoutInput()
	}
	return cmd
}
