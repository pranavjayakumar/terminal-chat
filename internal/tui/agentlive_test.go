package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// The live loop: thinking → reading → streaming → the real message.
func TestAgentLiveRow(t *testing.T) {
	for _, sk := range []string{"", `,"skin":"retro"`} {
		f := agentFake()
		m := agentModel(t, f, 120, 40, sk)
		setLive(m, f, 10, 0, 1, proto.AgentThinking, "thinking…", "")
		v := frameText(t, m, "thinking")
		row := lineWith(t, v, "Rocky is thinking…")
		if !strings.Contains(row, "▟▙") && !strings.Contains(row, "▗▖") && !strings.Contains(row, "▄▄") {
			t.Errorf("no glyph on the status row: %q", row)
		}
		// it is the last row of the log, under the ask
		lines := strings.Split(v, "\n")
		at := -1
		for i, l := range lines {
			if strings.Contains(l, "Rocky is thinking") {
				at = i
			}
		}
		if !strings.Contains(lines[at-2], "replies") {
			t.Errorf("status row not under the last message:\n%s", v)
		}
		setLive(m, f, 10, 0, 1, proto.AgentWorking, "reading the thread · 14 messages", "")
		lineWith(t, frameText(t, m, "reading"), "Rocky is reading the thread · 14 messages")
		setLive(m, f, 10, 0, 1, proto.AgentStreaming, "", "translation: no, but politely.\n\"revisit after")
		v = frameText(t, m, "streaming")
		row = lineWith(t, v, "Rocky is writing…")
		if !strings.Contains(row, "live · not saved") {
			t.Errorf("streaming row: %q", row)
		}
		pv := lineWith(t, v, "translation: no, but politely.")
		if !strings.Contains(pv, agentBar+" translation") {
			t.Errorf("preview row: %q", pv)
		}
		// the preview is dim italic
		x, y := find(t, m, "palette:")
		_ = x
		_ = y
		golden(t, "agent_live_wide"+skinTag(sk), v)
		m.Update(tea.WindowSizeMsg{Width: 44, Height: 24})
		golden(t, "agent_live_narrow"+skinTag(sk), frameText(t, m, "narrow streaming"))
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

		// the reply lands, then done: one real message, no status row
		f.msgs[10] = append(f.msgs[10], client.Message{Message: proto.Message{ID: 510, Chan: 10, Author: 1, Agent: "rocky", Body: "the real reply", Time: agentTime.UnixMilli()}})
		run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{10: true}}))
		setLive(m, f, 10, 0, 1, proto.AgentDone, "", "")
		v = frameText(t, m, "done")
		if strings.Contains(v, "Rocky is") || strings.Contains(v, "not saved") {
			t.Errorf("status row stayed after the reply:\n%s", v)
		}
		lineWith(t, v, "the real reply")
		setLive(m, f, 10, 0, 1, "", "", "")
		frameText(t, m, "gone")
	}
}

// In a thread the status row sits in the thread pane.
func TestAgentLiveInThread(t *testing.T) {
	f := agentFake()
	m := agentModel(t, f, 140, 40, "")
	run(m, m.openThread(400))
	m.layoutInput()
	setLive(m, f, 10, 400, 1, proto.AgentWorking, "editing poll.go…", "")
	v := frameText(t, m, "thread")
	row := lineWith(t, v, "Rocky is editing poll.go…")
	g := m.geo()
	if i := strings.Index(row, "Rocky is"); strWidth(row[:i]) < g.RX {
		t.Errorf("status row outside the thread pane: %q", row)
	}
	// the channel's top level doesn't show the thread's status
	if n := strings.Count(v, "Rocky is editing"); n != 1 {
		t.Errorf("status shown %d times", n)
	}
	golden(t, "agent_live_thread", v)
}

// Errors and sleep say so.
func TestAgentLiveStates(t *testing.T) {
	f := agentFake()
	m := agentModel(t, f, 100, 30, "")
	setLive(m, f, 10, 0, 1, proto.AgentError, "out of juice for today", "")
	if r := lineWith(t, frameText(t, m, "error"), "Rocky is stuck"); !strings.Contains(r, "out of juice") {
		t.Errorf("error row: %q", r)
	}
	setLive(m, f, 10, 0, 2, proto.AgentAsleep, "", "")
	lineWith(t, frameText(t, m, "asleep"), "Juno is asleep 💤")
}

// The glyph animates on the frame tick while the agent works, and the
// tick stops once nothing moves.
func TestAgentGlyphTicksOnlyWhileLive(t *testing.T) {
	f := agentFake()
	m := agentModel(t, f, 120, 40, "")
	m.fx.off = false
	clk := time.Duration(0)
	m.now = func() time.Time { return agentTime.Add(clk) }
	if d := m.agentWake(); d != 0 {
		t.Fatalf("idle agents wake the screen every %v", d)
	}
	setLive(m, f, 10, 0, 1, proto.AgentWorking, "reading…", "")
	if d := m.agentWake(); d != agentWorkFrame {
		t.Fatalf("working wakes every %v", d)
	}
	if m.frameTick() == nil {
		t.Fatal("no frame tick while working")
	}
	glyphs := map[string]bool{}
	for i := 0; i < 4; i++ {
		v := frameText(t, m, "tick")
		r := lineWith(t, v, "Rocky is reading")
		before := strings.Fields(r[:strings.Index(r, "Rocky is")])
		glyphs[before[len(before)-1]] = true
		clk += agentWorkFrame
	}
	if len(glyphs) < 3 {
		t.Errorf("the glyph didn't move: %v", glyphs)
	}
	setLive(m, f, 10, 0, 1, "", "", "")
	if d := m.agentWake(); d != 0 {
		t.Fatalf("still waking every %v", d)
	}
	// Animations off: still frames, no tick.
	setLive(m, f, 10, 0, 1, proto.AgentWorking, "reading…", "")
	m.fx.off = true
	if d := m.agentWake(); d != 0 {
		t.Fatal("ticking with animations off")
	}
}

// ---- sidebar ----

func TestAgentSidebar(t *testing.T) {
	f := agentFake()
	m := agentModel(t, f, 120, 40, "")
	v := frameText(t, m, "sidebar")
	juno := lineWith(t, v, "Juno")
	if !strings.Contains(juno, "└ ◡◡ Juno 💤") {
		t.Errorf("asleep agent row: %q", juno)
	}
	lines := strings.Split(v, "\n")
	for i, l := range lines {
		if strings.Contains(l, "◐ bob") && !strings.Contains(lines[i+1], "Juno") {
			t.Error("Juno isn't under bob")
		}
		if strings.Contains(l, "alice") && strings.Contains(l, "⚙") && !strings.Contains(lines[i+1], "└ ▟▙ Rocky") {
			t.Errorf("my agent isn't under me: %q", lines[i+1])
		}
	}
	// the agent DM is not a channel row
	if strings.Contains(v, "# \n") || strings.Count(v, "Rocky") != 2 { // the sidebar row and the message
		t.Errorf("agent DM listed as a channel:\n%s", v)
	}
	// my agent's glyph follows its live state in the sidebar
	m.fx.off = false
	setLive(m, f, 10, 400, 1, proto.AgentWorking, "x", "")
	if d := m.agentWake(); d == 0 {
		t.Fatal("sidebar glyph doesn't animate")
	}
	// a click opens our DM; from there the panel
	click(t, m, "agent:1")
	if m.active != 90 {
		t.Fatalf("click opened %d", m.active)
	}
	lineWith(t, frameText(t, m, "agent dm"), "Rocky · your agent · only you see this chat")
	click(t, m, "agent:1")
	if m.mode != modeAgent {
		t.Fatal("second click didn't open the panel")
	}
	press(m, "esc")
	// someone else's agent: a click calls it
	click(t, m, "agent:2")
	if m.input.Value() != "@juno " {
		t.Fatalf("input %q", m.input.Value())
	}
	// narrow: the icons sidebar keeps the glyphs
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	frameText(t, m, "icons sidebar")
}

func TestAgentDM(t *testing.T) {
	for _, sk := range []string{"", `,"skin":"retro"`} {
		f := agentFake()
		m := agentModel(t, f, 100, 30, sk)
		run(m, m.openChannel(90))
		v := frameText(t, m, "agent dm")
		lineWith(t, v, "Rocky · your agent · only you see this chat")
		lineWith(t, v, "Talk mode")
		lineWith(t, v, "only you")
		if m.input.Placeholder != "tell Rocky anything…" {
			t.Errorf("placeholder %q", m.input.Placeholder)
		}
		lineWith(t, v, "hey Alice. I'm Rocky.")
		golden(t, "agent_dm"+skinTag(sk), v)
		click(t, m, "app:agent")
		if m.mode != modeAgent {
			t.Fatal("the mode in the header doesn't open the panel")
		}
	}
	// Without the DM yet, d in the panel opens it through OpenAgentDM.
	f := agentFake()
	f.chans = f.chans[:len(f.chans)-1]
	m := agentModel(t, f, 100, 30, "")
	run(m, m.openAgentPanel())
	press(m, "d")
	if !f.called("agentdm") || m.active != 90 {
		t.Fatalf("d: active %d, calls %v", m.active, f.calls)
	}
}

// ---- approvals ----

func TestApprovalParse(t *testing.T) {
	x := client.Message{Message: proto.Message{Agent: "rocky", Body: "✋ @alice approve: push agent/poll? react ✅ / ❌  ·  #ab12\nthe detail"}}
	c, ok := parseApproval(x)
	if !ok || c.owner != "alice" || c.action != "push agent/poll" || c.code != "#ab12" || c.detail != "the detail" {
		t.Fatalf("%+v %v", c, ok)
	}
	x.Agent = ""
	if _, ok := parseApproval(x); ok {
		t.Fatal("a person's message parsed as a card")
	}
	for _, b := range []string{"✋ @alice approve: x", "hello", "✋ @alice approve: a? react ✅ / ❌  ·  nocode"} {
		if _, ok := parseApproval(client.Message{Message: proto.Message{Agent: "r", Body: b}}); ok {
			t.Errorf("%q parsed", b)
		}
	}
}

func TestApprovalPrompt(t *testing.T) {
	for _, sk := range []string{"", `,"skin":"retro"`} {
		f := agentFake()
		m := agentModel(t, f, 140, 40, sk)
		run(m, m.openThread(400))
		m.layoutInput()
		v := frameText(t, m, "card")
		row := lineWith(t, v, "open a draft PR?")
		for _, want := range []string{"1 yes", "2 yes, PRs ok today", "3 no"} {
			if !strings.Contains(v, want) {
				t.Errorf("no %q in %q", want, row)
			}
		}
		lineWith(t, v, "only you can answer")
		golden(t, "agent_approval"+skinTag(sk), v)
		m.Update(tea.WindowSizeMsg{Width: 44, Height: 30})
		nv := frameText(t, m, "narrow card")
		lineWith(t, nv, "3 no")
		golden(t, "agent_approval_narrow"+skinTag(sk), nv)
		m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})

		// digits in the composer stay text; selected, 2 answers "yes, ok
		// today": ✅ then ♾️ on the card
		typeVia(m, "2pm")
		if len(f.ag.reacted) != 0 || m.input.Value() != "2pm" {
			t.Fatalf("composer digits answered: %v %q", f.ag.reacted, m.input.Value())
		}
		press(m, "ctrl+u", "up")
		if ph := m.input.Placeholder; !strings.Contains(ph, "1 yes · 2 PRs ok today · 3 no") && !strings.HasPrefix(ph, "1 2 3 ") {
			t.Errorf("select footer: %q", m.input.Placeholder)
		}
		press(m, "2")
		if !strings.Contains(m.notice, "you said yes, ok today") {
			t.Errorf("no feedback: %q", m.notice)
		}
		press(m, "esc")
		if got := strings.Join(f.ag.reacted, ","); got != "401 ✅,401 "+apprToday {
			t.Fatalf("reacted %q", got)
		}
		// the answer comes back as reactions: the prompt collapses
		f.threads[400][2].Reactions = map[string][]uint64{apprYes: {1}, apprToday: {1}}
		run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{10: true}}))
		v = frameText(t, m, "answered")
		row = lineWith(t, v, "Alice said yes, PRs ok today")
		if !strings.Contains(row, "✓") || strings.Contains(v, "only you can answer") {
			t.Errorf("collapsed: %q", row)
		}
		// typing 1 now is just text
		press(m, "1")
		if m.input.Value() != "1" {
			t.Fatalf("1 after the answer: %q", m.input.Value())
		}
	}
}

func TestApprovalOthersAndKeys(t *testing.T) {
	// Bob sees the same card from alice's agent: no prompt for him.
	f := agentFake()
	f.me = proto.User{ID: 2, Name: "bob"}
	m := agentModel(t, f, 140, 40, "")
	run(m, m.openThread(400))
	m.layoutInput()
	v := frameText(t, m, "bob")
	lineWith(t, v, "waiting for Alice")
	if strings.Contains(v, "2 yes") {
		t.Fatal("bob sees the choices")
	}
	press(m, "1")
	if len(f.ag.reacted) != 0 || m.input.Value() != "1" {
		t.Fatal("bob's 1 answered alice's card")
	}

	// Alice: a click on "3 no" reacts ❌; select mode + 1 works too.
	f = agentFake()
	m = agentModel(t, f, 140, 40, "")
	run(m, m.openThread(400))
	m.layoutInput()
	click(t, m, "appr:thr:2~i401:3")
	if strings.Join(f.ag.reacted, ",") != "401 "+apprNo {
		t.Fatalf("click reacted %v", f.ag.reacted)
	}
	f.ag.reacted = nil
	press(m, "1") // not selected: just text
	if len(f.ag.reacted) != 0 {
		t.Fatal("an unselected card took 1")
	}
	press(m, "ctrl+u", "up") // select mode on the card
	press(m, "1")
	if strings.Join(f.ag.reacted, ",") != "401 "+apprYes {
		t.Fatalf("select+1 reacted %v", f.ag.reacted)
	}
	// a draft in the input keeps 1 as text
	f.ag.reacted = nil
	press(m, "esc")
	typeVia(m, "a")
	press(m, "1")
	if len(f.ag.reacted) != 0 {
		t.Fatal("1 answered over a draft")
	}
	// an expired card says so
	f.threads[400] = append(f.threads[400], client.Message{Message: proto.Message{ID: 403, Chan: 10, Thread: 400, Author: 1, Agent: "rocky", Body: "⌛ no answer on #k3x9q, so I'm not doing it", Time: agentTime.UnixMilli()}})
	run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{10: true}}))
	lineWith(t, frameText(t, m, "expired"), "⌛ no answer · open a draft PR?")
}

// A card that waits while I look elsewhere pings me and badges my agent.
func TestApprovalPing(t *testing.T) {
	f := agentFake()
	m := agentModel(t, f, 120, 40, "")
	run(m, m.openChannel(11))
	card := f.threads[400][2]
	card.Sec = client.SecPlain
	run(m, m.applyUpdate(client.Update{New: []client.Message{card}}))
	got := notes(m).got()
	if len(got) != 1 || got[0][0] != "Rocky needs your ok" {
		t.Fatalf("notes %v", got)
	}
	if r := lineWith(t, frameText(t, m, "badge"), "└ ▟▙ Rocky"); !strings.Contains(r, "✋") {
		t.Errorf("no badge: %q", r)
	}
	// the palette has a row for it, ⌥O jumps there and selects the card
	m.openPalette("all")
	found := false
	for _, it := range m.pal.items {
		found = found || it.run == "app:approval" && strings.Contains(it.label, "Approval waiting · #general")
	}
	if !found {
		t.Error("no palette row")
	}
	press(m, "esc")
	run(m, m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}, Alt: true}))
	if sel, ok := m.selected(); !ok || sel.ID != 401 || m.mode != modeSelect {
		t.Fatalf("⌥O selected %v %v", sel.ID, m.mode)
	}
	press(m, "esc")
	run(m, m.openChannel(11))
	click(t, m, "agent:1")
	if m.active != 10 || !m.thread.open() || m.thread.root != 400 {
		t.Fatalf("the badge opened %d/%d", m.active, m.thread.root)
	}
}

// Work runs fold their steps under the reply.
func TestAgentRunFold(t *testing.T) {
	f := agentFake()
	m := agentModel(t, f, 120, 40, "")
	m.ag.home.cfg.Mode = "work"
	start := agentTime
	clk := time.Duration(0)
	m.now = func() time.Time { return start.Add(clk) }
	setLive(m, f, 10, 0, 1, proto.AgentThinking, "", "")
	for _, s := range []string{"reading poll.go…", "editing poll.go…", "running go test ./…"} {
		clk += 20 * time.Second
		setLive(m, f, 10, 0, 1, proto.AgentWorking, s, "")
	}
	clk += 100 * time.Second
	f.msgs[10] = append(f.msgs[10], client.Message{Message: proto.Message{ID: 520, Chan: 10, Author: 1, Agent: "rocky", Body: "done: /poll works", Time: start.Add(clk).UnixMilli()}})
	run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{10: true}}))
	setLive(m, f, 10, 0, 1, proto.AgentDone, "", "")
	v := frameText(t, m, "fold")
	row := lineWith(t, v, "▸ 3 steps · 2m 40s")
	_ = row
	click(t, m, "fold:main:")
	v = frameText(t, m, "unfolded")
	lineWith(t, v, "▾ 3 steps")
	lineWith(t, v, "· editing poll.go…")
	// the keyboard folds it too: select the reply, o
	press(m, "up")
	if !strings.Contains(m.input.Placeholder, "o steps") {
		t.Errorf("footer: %q", m.input.Placeholder)
	}
	press(m, "o")
	lineWith(t, frameText(t, m, "folded"), "▸ 3 steps")
	press(m, "enter")
	lineWith(t, frameText(t, m, "enter unfolds"), "▾ 3 steps")
}

// A visitor post (an outside Claude session, tc mcp) shows ⇢ by the name.
func TestViaMark(t *testing.T) {
	f := agentFake()
	f.msgs[10] = append(f.msgs[10], client.Message{Message: proto.Message{ID: 530, Chan: 10, Author: 2, Via: proto.ViaMCP, Body: "posted from claude", Time: agentTime.UnixMilli()}})
	m := agentModel(t, f, 120, 40, "")
	v := frameText(t, m, "via")
	if r := lineWith(t, v, "bob  21:07"); !strings.Contains(r, viaMark+" bob") {
		t.Errorf("no via mark: %q", r)
	}
	if grouped(f.msgs[10][4], f.msgs[10][5]) {
		t.Error("a visitor post grouped with its author's own")
	}
}

// Identity follows the agent's current handle and icon (AgentOf the
// author), not the handle the message was sent under.
func TestAgentIdentityFollowsRename(t *testing.T) {
	f := agentFake()
	f.ag.agents[0].Handle, f.ag.agents[0].Icon = "boulder", "kite:2"
	m := agentModel(t, f, 120, 40, "")
	r := lineWith(t, frameText(t, m, "renamed"), "Boulder ⟡")
	if !strings.Contains(r, "◆ Boulder ⟡ · Alice's agent") {
		t.Errorf("name row: %q", r)
	}
	// an agent that has left keeps the handle its message carries
	f.ag.agents = f.ag.agents[1:]
	run(m, m.applyUpdate(client.Update{Agents: true}))
	lineWith(t, frameText(t, m, "retired"), "Rocky ⟡")
}

// Review follow-ups.

// Typing a time into the composer under a waiting card never answers it.
func TestApprovalComposerDigits(t *testing.T) {
	f := agentFake()
	m := agentModel(t, f, 140, 40, "")
	run(m, m.openThread(400))
	m.layoutInput()
	typeVia(m, "1pm works")
	if len(f.ag.reacted) != 0 || m.input.Value() != "1pm works" {
		t.Fatalf("input %q reacted %v", m.input.Value(), f.ag.reacted)
	}
}

// Scrolled up while a reply streams: the view stays put.
func TestLiveRowsKeepScrolledView(t *testing.T) {
	f := agentFake()
	for i := 0; i < 40; i++ {
		f.msgs[10] = append([]client.Message{{Message: proto.Message{ID: uint64(100 + i), Chan: 10, Author: 2, Body: "history line " + itoa(int64(i)), Time: agentTime.Add(-2 * time.Hour).UnixMilli()}}}, f.msgs[10]...)
	}
	m := agentModel(t, f, 80, 24, "")
	run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{10: true}}))
	m.main.scroll = 10
	top := func() string { return strings.Split(frameText(t, m, "scrolled"), "\n")[4] }
	before := top()
	for _, st := range []struct{ state, preview string }{{proto.AgentThinking, ""}, {proto.AgentStreaming, "one\ntwo\nthree\nfour"}, {proto.AgentStreaming, "one"}, {"", ""}} {
		setLive(m, f, 10, 0, 1, st.state, "", st.preview)
		if got := top(); got != before {
			t.Fatalf("%s moved the view: %q, was %q", st.state, got, before)
		}
	}
}

// The reply landed but "done" hasn't: the stale preview goes at once.
func TestLiveRowGoesWhenReplyLands(t *testing.T) {
	f := agentFake()
	m := agentModel(t, f, 120, 40, "")
	setLive(m, f, 10, 0, 1, proto.AgentStreaming, "", "the preview")
	lineWith(t, frameText(t, m, "streaming"), "the preview")
	f.msgs[10] = append(f.msgs[10], client.Message{Message: proto.Message{ID: 540, Chan: 10, Author: 1, Agent: "rocky", Body: "the reply", Time: agentTime.Add(time.Second).UnixMilli()}})
	run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{10: true}}))
	if v := frameText(t, m, "landed"); strings.Contains(v, "the preview") || strings.Contains(v, "Rocky is writing") {
		t.Fatalf("stale preview with the reply:\n%s", v)
	}
}

// No wakes for asleep agents or an unfocused terminal; thinking breathes
// in 140 ms steps.
func TestAgentWakeRules(t *testing.T) {
	f := agentFake()
	m := agentModel(t, f, 120, 40, "")
	m.fx.off = false
	setLive(m, f, 10, 0, 2, proto.AgentAsleep, "", "")
	if d := m.agentWake(); d != 0 {
		t.Fatalf("asleep wakes every %v", d)
	}
	setLive(m, f, 10, 0, 2, "", "", "")
	setLive(m, f, 10, 0, 1, proto.AgentThinking, "", "")
	if d := m.agentWake(); d != agentBreathStep {
		t.Fatalf("thinking wakes every %v", d)
	}
	m.Update(tea.BlurMsg{})
	if d := m.agentWake(); d != 0 {
		t.Fatalf("unfocused wakes every %v", d)
	}
}

// A card in a thread of the channel I'm in, but not open, still pings.
func TestApprovalPingInUnopenedThread(t *testing.T) {
	f := agentFake()
	m := agentModel(t, f, 120, 40, "")
	card := f.threads[400][2]
	run(m, m.applyUpdate(client.Update{New: []client.Message{card}}))
	if got := notes(m).got(); len(got) != 1 {
		t.Fatalf("notes %v", got)
	}
}

// The keys sheet lists the agent keys.
func TestKeySheetAgentKeys(t *testing.T) {
	var all []string
	for _, r := range keySheetRows(120) {
		for _, c := range r {
			all = append(all, c.text)
		}
	}
	s := strings.Join(all, " ")
	for _, want := range []string{"Agent", "⌥K Agent", "⌥O", "1 2 3", "p d L r"} {
		if !strings.Contains(s, want) {
			t.Errorf("keys sheet lacks %q", want)
		}
	}
}
