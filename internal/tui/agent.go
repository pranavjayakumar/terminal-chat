package tui

import (
	"hash/fnv"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Personal agents (docs/TUI.md › Agents): an agent is one more teammate in
// the room. It posts as its owner's device with Message.Agent set, shows a
// glyph that animates with what it is doing, a ⟡ badge, and a dashed bar.

// agentPreset is one of the eight animated icons: a frame list per state.
// Every frame is 1–2 cells.
type agentPreset struct {
	id                string
	idle              string
	thinking, working []string
	done              string
	asleep            []string
}

// The orb is written \u25c9: the same circle is the members icon role,
// which icons.go owns.
var agentPresets = []agentPreset{
	{"pebble", "▟▙", []string{"▟▙", "▗▖", "▄▄", "▗▖"}, []string{"▟▙", "▜▙", "▜▛", "▟▛"}, "▟▙", []string{"▄▄", "▁▁"}},
	{"orb", "\u25c9", []string{"\u25c9", "◎", "○", "◎"}, []string{"◐", "◓", "◑", "◒"}, "\u25c9", []string{"◌"}},
	{"spark", "✦", []string{"✦", "✧", "·", "✧"}, []string{"✶", "✷", "✸", "✹", "✺", "✹", "✸", "✷"}, "✦", []string{"✧", "·"}},
	{"owl", "◕◕", []string{"◕◕", "◔◔", "◑◑", "◔◔"}, []string{"◐◑", "◓◓", "◑◐", "◒◒"}, "◠◠", []string{"◡◡"}},
	{"hive", "⣿", []string{"⡀", "⡄", "⡆", "⡇", "⣇", "⣧", "⣷", "⣿"}, []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}, "⣿", []string{"⠤", "⠒"}},
	{"moon", "☾", []string{"◯", "◔", "◑", "◕", "●", "◕", "◑", "◔"}, []string{"◜", "◝", "◞", "◟"}, "●", []string{"☾"}},
	{"meter", "▃▅", []string{"▂▃", "▃▅", "▅▇", "▃▅"}, []string{"▁▇", "▇▃", "▃▅", "▅▁"}, "▅▇", []string{"▁▁"}},
	{"kite", "◆", []string{"◇", "◈", "◆", "◈"}, []string{"◢", "◣", "◤", "◥"}, "◆", []string{"◇"}},
}

// Frame times and effects per state (DECISIONS.md › Timings).
const (
	agentThinkFrame  = 280 * time.Millisecond
	agentWorkFrame   = 110 * time.Millisecond
	agentStreamFrame = 220 * time.Millisecond
	agentAsleepFrame = 1600 * time.Millisecond
	agentBreath      = 1120 * time.Millisecond // thinking: 100% → 50% → 100%
	agentFlash       = 290 * time.Millisecond  // done: two ok-colored flashes
	agentDoneFx      = 3 * agentFlash          // on, off, on
)

const agentBadge = "⟡"

// glyphMin: a faded glyph (half strength, mid-breath) still reads at this
// contrast on its background.
const glyphMin = 2.4

// agentLook is how an agent's Icon string draws: one of the presets, or a
// literal glyph someone chose elsewhere, plus an optional name color.
//
// The Icon field (proto.Agent.Icon, at most 32 bytes) holds "<preset>" or
// "<preset>:<n>", n the theme name color 0–7 the owner picked; anything
// else is shown as a static glyph (its first grapheme) in the default color.
type agentLook struct {
	preset  int    // index into agentPresets, -1: literal
	literal string // the static glyph when preset < 0
	color   int    // 0–7, -1: derived from the handle
}

func parseAgentIcon(icon string) agentLook {
	icon = sanitize.Line(icon)
	name, col, hasCol := strings.Cut(icon, ":")
	l := agentLook{preset: -1, color: -1}
	for i, p := range agentPresets {
		if p.id == name {
			l.preset = i
		}
	}
	if l.preset >= 0 && hasCol {
		if n, err := strconv.Atoi(col); err == nil && n >= 0 && n < 8 {
			l.color = n
		}
	}
	if l.preset < 0 {
		g, _, _, _ := uniseg.FirstGraphemeClusterInString(icon, -1)
		if g == "" || strWidth(g) < 1 || strWidth(g) > 2 {
			l.preset = 0 // nothing drawable: the default
		} else {
			l.literal = g
		}
	}
	return l
}

// agentIcon is the Icon string for a preset and a name color (-1 none).
func agentIcon(preset, color int) string {
	s := agentPresets[preset].id
	if color >= 0 {
		s += ":" + strconv.Itoa(color)
	}
	return s
}

// agentName is the handle as a name: "rocky" → "Rocky".
func agentName(handle string) string {
	h := sanitize.Line(handle)
	r, n := utf8.DecodeRuneInString(h)
	if n == 0 {
		return "agent"
	}
	return string(unicode.ToUpper(r)) + h[n:]
}

// agentColorIx is the name color index an agent draws in.
func agentColorIx(a proto.Agent) int {
	if l := parseAgentIcon(a.Icon); l.color >= 0 {
		return l.color
	}
	h := fnv.New32a()
	h.Write([]byte(a.Handle))
	return int(h.Sum32() % 8)
}

func (m *Model) agentColor(a proto.Agent) rgb { return m.st.th.nicks[agentColorIx(a)] }

// agentFrame is the glyph for state at elapsed time into it.
func agentFrame(l agentLook, state string, elapsed time.Duration, motion bool) string {
	if l.preset < 0 {
		return l.literal
	}
	p := agentPresets[l.preset]
	pick := func(fs []string, every time.Duration) string {
		if !motion || len(fs) == 1 {
			return fs[0]
		}
		return fs[int(elapsed/every)%len(fs)]
	}
	switch state {
	case proto.AgentThinking:
		return pick(p.thinking, agentThinkFrame)
	case proto.AgentWorking:
		return pick(p.working, agentWorkFrame)
	case proto.AgentStreaming:
		return pick(p.working, agentStreamFrame)
	case proto.AgentDone:
		return p.done
	case proto.AgentAsleep:
		return pick(p.asleep, agentAsleepFrame)
	}
	return p.idle
}

// agentFrameEvery is how often state's glyph changes; 0: never.
func agentFrameEvery(state string) time.Duration {
	switch state {
	case proto.AgentThinking:
		return agentThinkFrame
	case proto.AgentWorking:
		return agentWorkFrame
	case proto.AgentStreaming:
		return agentStreamFrame
	case proto.AgentAsleep:
		return agentAsleepFrame
	}
	return 0
}

// agentGlyph is the agent's glyph and its color for state, which began at
// since, drawn on bg: thinking breathes, done flashes in the ok color,
// asleep sits at half strength.
func (m *Model) agentGlyph(a proto.Agent, state string, since time.Time, bg rgb) (string, rgb) {
	l := parseAgentIcon(a.Icon)
	col := m.agentColor(a)
	motion := !m.fx.off
	el := m.now().Sub(since)
	if since.IsZero() || el < 0 {
		el = 0
	}
	g := agentFrame(l, state, el, motion)
	switch state {
	case proto.AgentThinking:
		if motion {
			ph := float64(el%agentBreath) / float64(agentBreath)
			col = apart(mix(bg, col, 1-0.5*(1-absCos(ph))), bg, col, glyphMin)
		}
	case proto.AgentDone:
		if motion && el < agentDoneFx && (el/agentFlash)%2 == 0 {
			col = m.st.th.ok
		}
	case proto.AgentAsleep:
		col = apart(mix(bg, col, 0.5), bg, col, glyphMin)
	}
	return g, col
}

// absCos is |cos(πp)|: 1 at the ends of a breath, 0 halfway.
func absCos(p float64) float64 {
	return absSin(p*3.141592653589793 + 3.141592653589793/2)
}

// ---- the agents tc knows ----

// agentUI is what the model keeps about agents (refreshed on Update.Agents
// and Update.AgentStatus; drawing never asks the client).
type agentUI struct {
	byOwner map[uint64]proto.Agent
	byName  map[string]proto.Agent
	dm      uint64 // my agent DM channel, 0 none
	// live statuses of the channels we know of: ch → every thread's.
	live  map[uint64][]liveRow
	since map[liveKey]liveSince
	runs  map[liveKey]*agentRun // runs in progress, for the folded steps line
	folds map[uint64]*agentRun  // message id → the finished run it ended
	open  map[uint64]bool       // folds expanded
	// pending: my agent's approval cards still waiting (approval.go)
	pending map[uint64]pendingAppr
	home  agentHomeState        // my agent's home config (agentpanel.go)
	panel agentPanel
}

type liveRow struct {
	thread uint64
	client.AgentLive
}

type liveKey struct {
	owner, ch, thread uint64
}

type liveSince struct {
	state string
	at    time.Time
}

func (m *Model) reloadAgents() {
	list := m.c.Agents()
	m.ag.dm = 0
	if v, ok := m.c.AgentDM(); ok && v.Member {
		m.ag.dm = v.ID
	}
	same := len(list) == len(m.ag.byOwner)
	for _, a := range list {
		same = same && m.ag.byOwner[a.Owner] == a
	}
	if same && m.ag.byOwner != nil {
		return
	}
	m.ag.byOwner = map[uint64]proto.Agent{}
	m.ag.byName = map[string]proto.Agent{}
	for _, a := range list {
		m.ag.byOwner[a.Owner] = a
		m.ag.byName[strings.ToLower(a.Handle)] = a
	}
	m.blocks.reset() // names, icons and colors are drawn into blocks
	m.main.memoBlocks, m.thread.memoBlocks = nil, nil
	m.main.ver++
	m.thread.ver++
}

// agentOfMsg is the agent that sent x: its owner's agent now (current
// handle and icon), or one that has left (only the handle x carries).
func (m *Model) agentOfMsg(x client.Message) proto.Agent {
	if a, ok := m.ag.byOwner[x.Author]; ok {
		return a
	}
	return proto.Agent{Handle: x.Agent, Owner: x.Author}
}

// agentByHandle is the active agent with handle h, or one that has left
// (only its handle is known then).
func (m *Model) agentByHandle(h string) proto.Agent {
	if a, ok := m.ag.byName[strings.ToLower(h)]; ok {
		return a
	}
	return proto.Agent{Handle: h}
}

// myAgent is my active agent.
func (m *Model) myAgent() (proto.Agent, bool) {
	a, ok := m.ag.byOwner[m.me.ID]
	return a, ok
}

// isAgentDM reports whether ch is my private chat with my agent.
func (m *Model) isAgentDM(ch uint64) bool {
	if ch == 0 {
		return false
	}
	if ch == m.ag.dm {
		return true
	}
	v, ok := m.chanView(ch)
	return ok && v.Kind == proto.ChanAgentDM
}

// ownerName is how an agent's owner is called: "Fizan".
func (m *Model) ownerName(owner uint64) string {
	n := sanitize.Line(m.users[owner].Name)
	if n == "" {
		return "someone"
	}
	r, size := utf8.DecodeRuneInString(n)
	return string(unicode.ToUpper(r)) + n[size:]
}

// agentStateOf is the agent's most recent live state anywhere and when it
// began; "" (idle) when nothing is live, asleep when it is offline.
func (m *Model) agentStateOf(a proto.Agent) (string, time.Time) {
	var best liveRow
	var bestCh uint64
	for ch, rows := range m.ag.live {
		for _, r := range rows {
			if r.Agent.Owner == a.Owner && r.At.After(best.At) {
				best, bestCh = r, ch
			}
		}
	}
	if best.State != "" && !(best.State == proto.AgentDone && m.now().Sub(best.At) > agentDoneFx) {
		return best.State, m.ag.since[liveKey{a.Owner, bestCh, best.thread}].at
	}
	if !a.Online {
		return proto.AgentAsleep, time.Time{}
	}
	return "", time.Time{}
}

// reloadLive re-reads the live statuses of ch.
func (m *Model) reloadLive(ch uint64) {
	if m.ag.live == nil {
		m.ag.live = map[uint64][]liveRow{}
		m.ag.since = map[liveKey]liveSince{}
		m.ag.runs = map[liveKey]*agentRun{}
	}
	var rows []liveRow
	seen := map[liveKey]bool{}
	for _, th := range m.c.AgentThreads(ch) {
		for _, l := range m.c.AgentStatus(ch, th) {
			rows = append(rows, liveRow{th, l})
			k := liveKey{l.Agent.Owner, ch, th}
			seen[k] = true
			if s, ok := m.ag.since[k]; !ok || s.state != l.State {
				m.ag.since[k] = liveSince{l.State, m.now()}
			}
			m.noteRun(k, l)
		}
	}
	for k := range m.ag.since {
		if k.ch == ch && !seen[k] {
			delete(m.ag.since, k)
			m.endRun(k)
		}
	}
	if len(rows) == 0 {
		delete(m.ag.live, ch)
	} else {
		m.ag.live[ch] = rows
	}
	// paneLines notices the change itself (its live signature): the
	// messages' layout stays memoized while a reply streams.
}

// liveIn is the live statuses shown at the bottom of a pane: the channel's
// top level, or the open thread.
func (m *Model) liveIn(p *pane) []liveRow {
	var out []liveRow
	for _, r := range m.ag.live[p.ch] {
		if r.thread == p.root {
			out = append(out, r)
		}
	}
	return out
}

// agentWake is how soon an agent glyph on screen changes; 0: none moves.
func (m *Model) agentWake() time.Duration {
	if m.fx.off {
		return 0
	}
	d := time.Duration(0)
	want := func(e time.Duration) {
		if e > 0 && (d == 0 || e < d) {
			d = e
		}
	}
	consider := func(state string, since time.Time) {
		switch state {
		case proto.AgentThinking:
			want(fastFrame * 2) // the breath is a smooth fade
		case proto.AgentDone:
			if m.now().Sub(since) < agentDoneFx {
				want(agentFlash / 2)
			}
		default:
			want(agentFrameEvery(state))
		}
	}
	panes := []*pane{&m.main}
	if m.thread.open() {
		panes = append(panes, &m.thread)
	}
	for _, p := range panes {
		for _, r := range m.liveIn(p) {
			consider(r.State, m.ag.since[liveKey{r.Agent.Owner, p.ch, r.thread}].at)
		}
	}
	if a, ok := m.myAgent(); ok && m.agentHeaderRoom() {
		if st, since := m.agentStateOf(a); st != proto.AgentAsleep {
			consider(st, since)
		}
	}
	if m.sidebarShown() {
		for _, a := range m.sidebarAgents() {
			if st, since := m.agentStateOf(a); st != proto.AgentAsleep {
				consider(st, since)
			}
		}
	}
	return d
}

// sidebarAgents are the agents the sidebar lists: mine and those of the
// people I have a direct message with.
func (m *Model) sidebarAgents() []proto.Agent {
	var out []proto.Agent
	if a, ok := m.myAgent(); ok {
		out = append(out, a)
	}
	for _, v := range m.chans {
		if v.Kind != proto.ChanDM || !v.Member {
			continue
		}
		if others := m.dmOthers(v); len(others) == 1 {
			if a, ok := m.ag.byOwner[others[0]]; ok {
				out = append(out, a)
			}
		}
	}
	return out
}
