package tui

import (
	"context"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/sanitize"
)

// Approvals. The harness (internal/agent/approvals.go) posts a card
//
//	✋ @<owner> approve: <action>? react ✅ / ❌  ·  #<code>
//	<detail>
//
// in the thread and waits for its owner to react ✅ (yes) or ❌ (no) on it.
// tc draws the card as an inline prompt: `<action>?  1 yes · 2 yes, ok
// today · 3 no`. Keys 1–3 or a click answer it with reactions, and only
// the owner's answer counts (the harness reads the owner's reactions only;
// everyone else sees "waiting for <owner>"). Choice 2 reacts ✅ and ♾️:
// the harness takes the ✅ as yes; ♾️ says "and the same kind of thing is
// fine for today".

const (
	apprYes   = "✅"
	apprNo    = "❌"
	apprToday = "♾️"
)

type approvalCard struct {
	owner, action, code, detail string
}

// apprState is a card's answer as its block shows it.
type apprState uint8

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
	if x.Agent == "" || x.Deleted || !x.Sec.Trusted() {
		return approvalCard{}, false
	}
	first, detail, _ := strings.Cut(x.Body, "\n")
	rest, ok := strings.CutPrefix(first, "✋ @")
	if !ok {
		return approvalCard{}, false
	}
	owner, rest, ok := strings.Cut(rest, " approve: ")
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

// approvalStates is every card's state in msgs: answered, or given up on
// ("⌛ no answer on #code" from the same agent).
func (m *Model) approvalStates(msgs []client.Message) map[uint64]apprState {
	var out map[uint64]apprState
	for i, x := range msgs {
		card, ok := parseApproval(x)
		if !ok {
			continue
		}
		if out == nil {
			out = map[uint64]apprState{}
		}
		st := verdict(x)
		if st == apprPending {
			for _, y := range msgs[i+1:] {
				if y.Agent == x.Agent && strings.Contains(y.Body, "no answer on "+card.code) {
					st = apprExpired
				}
			}
		}
		out[x.ID] = st
		if st != apprPending {
			delete(m.ag.pending, x.ID)
		}
	}
	return out
}

// apprChoices are the prompt's choices: key, label, reactions.
func apprChoices(action string) [3][3]string {
	today := "yes, ok today"
	if strings.Contains(action, "PR") {
		today = "yes, PRs ok today"
	}
	return [3][3]string{{"1", "yes", apprYes}, {"2", today, apprYes + " " + apprToday}, {"3", "no", apprNo}}
}

// drawApproval draws a card: its detail, then the prompt (the owner), a
// wait line (everyone else) or the answer.
func (m *Model) drawApproval(g *grower, x client.Message, card approvalCard, st apprState, bw int, at func(int) int, textFg rgb) {
	th := m.st.th
	if d := strings.TrimSpace(card.detail); d != "" {
		y := x
		y.Body, y.Edited = d, 0
		m.drawText(g, y, bw, at, textFg)
	}
	owner := m.ownerName(x.Author)
	q := sanitize.Line(card.action) + "?"
	switch st {
	case apprPending:
		mine := x.Author == m.me.ID
		y := g.row()
		xx := g.c.put(2, y, truncate(q, bw), fgb(th.fg), "")
		if !mine {
			yy := g.row()
			g.c.put(2, yy, truncate("waiting for "+owner, bw), fgc(th.faint), "")
			return
		}
		// the three choices as pills, wrapping in a narrow column
		xx += 2
		for _, ch := range apprChoices(card.action) {
			label := " " + ch[0] + " " + ch[1] + " "
			if xx+strWidth(label)+2 > 2+bw {
				y = g.row()
				xx = 2
			}
			st := fgc(th.fg).on(th.element)
			if ch[0] == "1" {
				st = fgb(th.accentFg).on(th.accent).with(aMonoRev)
			}
			xx = g.c.pill(xx, y, label, st, "appr|"+ch[0]) + 1
		}
		yy := g.row()
		g.c.put(2, yy, truncate("only you can answer · keys 1–3", bw), fgc(th.faint), "")
	case apprSaidYes, apprSaidToday:
		said := owner + " said yes"
		if st == apprSaidToday {
			said = owner + " said " + apprChoices(card.action)[1][1]
		}
		y := g.row()
		e := g.c.put(2, y, "✓", fgb(th.ok), "")
		g.c.put(e+1, y, truncate(said+" · "+q, bw-2), fgc(th.dim), "")
	case apprSaidNo:
		y := g.row()
		e := g.c.put(2, y, "✗", fgb(th.err), "")
		g.c.put(e+1, y, truncate(owner+" said no · "+q, bw-2), fgc(th.dim), "")
	case apprExpired:
		y := g.row()
		g.c.put(2, y, truncate("⌛ no answer · "+q, bw), fgc(th.faint), "")
	}
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
	var emojis []string
	for _, ch := range apprChoices(card.action) {
		if ch[0] == choice {
			emojis = strings.Fields(ch[2])
		}
	}
	if emojis == nil {
		return nil
	}
	delete(m.ag.pending, x.ID)
	id := x.ID
	return m.call("answer", func(ctx context.Context) error {
		for _, e := range emojis {
			if err := m.c.React(ctx, id, e); err != nil {
				return err
			}
		}
		return nil
	})
}

// pendingCard is the newest card of mine still waiting in p, if it is on
// screen (the keys 1–3 answer it).
func (m *Model) pendingCard(p *pane) (client.Message, bool) {
	if !p.open() || p.scroll > 0 {
		return client.Message{}, false
	}
	st := m.approvalStates(p.msgs)
	for i := len(p.msgs) - 1; i >= 0 && i >= len(p.msgs)-8; i-- {
		x := p.msgs[i]
		if x.Author == m.me.ID && x.Agent != "" && st[x.ID] == apprPending {
			if _, ok := parseApproval(x); ok {
				return x, true
			}
		}
	}
	return client.Message{}, false
}

// approvalKey answers the card in view with 1–3 typed into an empty input.
func (m *Model) approvalKey(key string) (tea.Cmd, bool) {
	if key != "1" && key != "2" && key != "3" {
		return nil, false
	}
	if m.mode == modeSelect {
		if x, ok := m.selected(); ok {
			if _, card := parseApproval(x); card && x.Author == m.me.ID {
				return m.answerApproval(x, key), true
			}
		}
		return nil, false
	}
	if m.input.Value() != "" || m.editing != 0 {
		return nil, false
	}
	x, ok := m.pendingCard(m.focus())
	if !ok {
		return nil, false
	}
	return m.answerApproval(x, key), true
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
		if (x.Chan == m.active && m.focused) || m.opt.Notifier == nil {
			continue
		}
		a := m.agentByHandle(x.Agent)
		n := notify.Note{Title: agentName(a.Handle) + " needs your ok", Body: sanitize.Line(card.action) + "?", Tag: "tc-agent"}
		send := m.opt.Notifier
		cmd = func() tea.Msg { send.Send(n); return noopMsg{} }
	}
	return cmd
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
	cmd := m.openChannel(p.ch)
	if p.thread != 0 {
		cmd = tea.Batch(cmd, m.openThread(p.thread))
		m.layoutInput()
	}
	return cmd
}
