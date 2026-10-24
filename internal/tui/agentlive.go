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

// An agent's message: a dashed bar, its glyph and a ⟡ badge on the name
// row. While it works, a status row under the conversation says what it
// is doing and its reply streams in dim and italic; the real message
// replaces both when it lands.

const agentBar = "┆"

// agentHeader draws `<icon> Rocky ⟡ · Fizan's agent  hh:mm`. Narrow
// columns drop "· Fizan's agent" first.
func (m *Model) agentHeader(g *grower, x client.Message, cw int) {
	th := m.st.th
	a := m.agentByHandle(x.Agent)
	if a.Owner == 0 {
		a.Owner = x.Author
	}
	col := m.agentColor(a)
	y := g.row()
	glyph := agentFrame(parseAgentIcon(a.Icon), "", 0, false)
	e := g.c.put(2, y, glyph, fgc(col), "")
	e = g.c.put(e+1, y, agentName(a.Handle), fgb(col), "")
	e = g.c.put(e+1, y, agentBadge, fgc(col), "")
	ts := time.UnixMilli(x.ShownTime()).Local().Format("15:04")
	owner := " · " + m.ownerName(a.Owner) + "'s agent"
	if cw >= 50 && e+strWidth(owner)+2+len(ts) <= cw {
		e = g.c.put(e, y, owner, fgc(th.faint), "")
	}
	if e+2+len(ts) <= cw {
		g.c.put(e+2, y, ts, fgc(th.faint), "")
	}
}

// milestoneMark colors the ✓ a milestone reply ("✓ tests pass") starts
// with in the ok color.
func (m *Model) milestoneMark(g *grower, first int) {
	if first < g.n {
		if cl := g.c.at(2, first); cl.s == "✓" {
			cl.fg = m.st.th.ok
		}
	}
}

// ---- live status rows ----

// liveMark is an animated glyph in a live block: drawn per frame over the
// block's placeholder.
type liveMark struct {
	agent proto.Agent
	state string
	since time.Time
}

// liveShown is the live rows p shows now: a done one only while it
// flashes, and none once the agent's reply is already in the pane.
func (m *Model) liveShown(p *pane) []liveRow {
	var out []liveRow
	for _, r := range m.liveIn(p) {
		since := m.ag.since[liveKey{r.Agent.Owner, p.ch, r.thread}].at
		if r.State == proto.AgentDone && m.now().Sub(since) >= agentDoneFx {
			continue
		}
		if m.replied(p, r) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// replied: the agent's message after this run started is in the pane.
func (m *Model) replied(p *pane, r liveRow) bool {
	if r.State != proto.AgentDone && r.State != proto.AgentError {
		return false
	}
	start := r.At
	if run := m.ag.runs[liveKey{r.Agent.Owner, p.ch, r.thread}]; run != nil {
		start = run.start
	}
	for i := len(p.msgs) - 1; i >= 0 && i >= len(p.msgs)-5; i-- {
		x := p.msgs[i]
		if x.Agent != "" && strings.EqualFold(x.Agent, r.Agent.Handle) && x.Time >= start.Add(-2*time.Second).UnixMilli() {
			return true
		}
	}
	return false
}

func liveSig(rows []liveRow) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%d|%s|%s|%s|%s|%d;", r.Agent.Owner, r.Agent.Handle, r.Agent.Icon, r.State, r.Status, len(r.Preview))
		b.WriteString(r.Preview)
	}
	return b.String()
}

// liveVerb is the status row's words after the name.
func liveVerb(r liveRow) (verb, extra string) {
	st := strings.TrimSpace(sanitize.Line(r.Status))
	def := map[string]string{
		proto.AgentThinking:  "thinking…",
		proto.AgentWorking:   "working…",
		proto.AgentStreaming: "writing…",
		proto.AgentAsleep:    "asleep 💤",
		proto.AgentError:     "stuck",
		proto.AgentDone:      "done",
	}[r.State]
	switch r.State {
	case proto.AgentStreaming:
		return "is writing…", "live · not saved"
	case proto.AgentError:
		if st != "" {
			return "is stuck:", st
		}
		return "is stuck", ""
	}
	if st == "" {
		st = def
	}
	if !strings.HasPrefix(st, "is ") {
		st = "is " + st
	}
	return st, ""
}

// previewRows is how much of a streamed reply shows: its last rows.
const previewRows = 10

// liveLines adds the live rows to a pane's layout.
func (m *Model) liveLines(p *pane, live []liveRow, w int, rows []prow, owner []int) ([]prow, []int) {
	p.liveMarks = nil
	for _, r := range live {
		b := m.liveBlock(r, w)
		if p.liveMarks == nil {
			p.liveMarks = map[*canvas]liveMark{}
		}
		p.liveMarks[b] = liveMark{r.Agent, r.State, m.ag.since[liveKey{r.Agent.Owner, p.ch, r.thread}].at}
		if len(rows) > 0 && !m.ui.Compact {
			rows = append(rows, prow{})
			owner = append(owner, -1)
		}
		for y := 0; y < b.h; y++ {
			rows = append(rows, prow{b: b, y: y})
			owner = append(owner, -1)
		}
	}
	return rows, owner
}

// liveBlock draws one agent's status row and preview; its glyph is left
// blank for the frame to fill in (drawLiveGlyph).
func (m *Model) liveBlock(r liveRow, cw int) *canvas {
	th := m.st.th
	g := newGrower(cw, th)
	col := m.agentColor(r.Agent)
	y := g.row()
	verb, extra := liveVerb(r)
	e := g.c.put(5, y, agentName(r.Agent.Handle), fgb(col), "")
	vst := fgc(th.dim)
	if r.State == proto.AgentError {
		vst = fgc(th.err)
	}
	room := cw - e - 1
	v := truncate(verb, room)
	e = g.c.put(e+1, y, v, vst, "")
	if extra != "" && cw-e-2 > 3 {
		g.c.put(e+2, y, truncate(extra, cw-e-2), fgc(th.faint), "")
	}
	if r.State == proto.AgentStreaming && r.Preview != "" {
		bw := bodyW(cw)
		var lines [][]span
		for _, ln := range strings.Split(sanitize.Text(r.Preview), "\n") {
			lines = append(lines, wrapSpans([]span{{text: ln}}, bw)...)
		}
		if len(lines) > previewRows {
			lines = lines[len(lines)-previewRows:]
		}
		for _, l := range lines {
			yy := g.row()
			g.c.put(0, yy, agentBar, fgc(th.faint), "")
			xx := 2
			for _, sp := range l {
				xx = g.c.put(xx, yy, sp.text, fgc(th.dim).with(aItalic), "")
			}
		}
	}
	return g.done()
}

// drawLiveGlyph draws the animated glyph of a live block's first row.
func (m *Model) drawLiveGlyph(c *canvas, mk liveMark, x, y int) {
	g, col := m.agentGlyph(mk.agent, mk.state, mk.since, m.st.th.bg)
	c.put(x+2, y, "  ", fgc(col), "")
	c.put(x+2, y, g, fgc(col), "")
}

// ---- Work runs: the folded steps line ----

// agentRun is what the UI saw of one agent run: when it began and the
// distinct steps its status line went through.
type agentRun struct {
	start, end time.Time
	steps      []string
	handle     string
	ch, thread uint64
	usd        float64
	attached   bool
}

// foldMinSteps: a run with fewer steps is an answer, not work; it folds
// nothing.
const foldMinSteps = 3

func (m *Model) noteRun(k liveKey, l client.AgentLive) {
	run := m.ag.runs[k]
	if run == nil || !run.end.IsZero() && l.State != proto.AgentDone && l.State != proto.AgentError {
		run = &agentRun{start: m.now(), handle: l.Agent.Handle, ch: k.ch, thread: k.thread}
		m.ag.runs[k] = run
	}
	if l.State == proto.AgentWorking {
		st := strings.TrimSpace(sanitize.Line(l.Status))
		if st != "" && (len(run.steps) == 0 || run.steps[len(run.steps)-1] != st) {
			run.steps = append(run.steps, st)
		}
	}
	if (l.State == proto.AgentDone || l.State == proto.AgentError) && run.end.IsZero() {
		run.end = m.now()
		m.attachRun(k, run)
	}
}

// endRun is the run's status gone: fold it under its reply if that came.
func (m *Model) endRun(k liveKey) {
	run := m.ag.runs[k]
	delete(m.ag.runs, k)
	if run == nil {
		return
	}
	if run.end.IsZero() {
		run.end = m.now()
	}
	m.attachRun(k, run)
}

// attachRun puts a work run's folded line under the agent's reply: its
// newest message in the run's thread since the run began.
func (m *Model) attachRun(k liveKey, run *agentRun) {
	if run.attached || len(run.steps) < foldMinSteps {
		return
	}
	if k.owner == m.me.ID && m.ag.home.ok && m.ag.home.cfg.Mode == "talk" {
		return // Talk mode answers; it doesn't work
	}
	var msgs []client.Message
	if k.thread != 0 {
		msgs = m.c.Thread(k.thread)
	} else {
		msgs = m.c.Messages(k.ch, 20)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		x := msgs[i]
		if x.ID != 0 && strings.EqualFold(x.Agent, run.handle) && x.Time >= run.start.Add(-2*time.Second).UnixMilli() {
			if m.ag.folds == nil {
				m.ag.folds = map[uint64]*agentRun{}
			}
			if k.owner == m.me.ID {
				run.usd = m.runCost(run)
			}
			run.attached = true
			m.ag.folds[x.ID] = run
			m.main.ver++
			m.thread.ver++
			return
		}
	}
}

// foldKey is the folded line a message carries, for its block key.
func (m *Model) foldKey(x client.Message) string {
	run := m.ag.folds[x.ID]
	if run == nil {
		return ""
	}
	s := m.foldText(run)
	if m.ag.open[x.ID] {
		s = "open|" + s
	}
	return s
}

func (m *Model) foldText(run *agentRun) string {
	n := len(run.steps)
	s := strconv.Itoa(n) + " steps · " + shortDur(run.end.Sub(run.start))
	if run.usd > 0 {
		s += fmt.Sprintf(" · $%.2f", run.usd)
	}
	return s
}

// shortDur is "2m 41s", "38s".
func shortDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
	return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// drawFold draws `▸ N steps · time · $` (and the steps when unfolded).
func (m *Model) drawFold(g *grower, x client.Message, key string, bw int) {
	th := m.st.th
	open := strings.HasPrefix(key, "open|")
	text := strings.TrimPrefix(key, "open|")
	arrow := "▸ "
	if open {
		arrow = "▾ "
	}
	y := g.row()
	g.c.put(2, y, truncate(arrow+text, bw), fgc(th.faint), "fold|")
	if !open {
		return
	}
	run := m.ag.folds[x.ID]
	if run == nil {
		return
	}
	for _, s := range run.steps {
		if g.full() {
			return
		}
		yy := g.row()
		g.c.put(4, yy, truncate("· "+s, bw-2), fgc(th.dim), "fold|")
	}
}

// toggleFold opens or folds a run's steps.
func (m *Model) toggleFold(id uint64) {
	if m.ag.open == nil {
		m.ag.open = map[uint64]bool{}
	}
	m.ag.open[id] = !m.ag.open[id]
	m.main.ver++
	m.thread.ver++
}
