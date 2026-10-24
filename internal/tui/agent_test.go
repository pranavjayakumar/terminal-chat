package tui

import (
	"context"
	stdflag "flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

var updateGolden = stdflag.Bool("update", false, "rewrite testdata/*.golden")

// golden compares a frame's text with testdata/<name>.golden.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (go test -run %s -update writes it)", err, t.Name())
	}
	if string(want) != got {
		t.Errorf("%s differs from %s (-update rewrites it)\n--- got\n%s\n--- want\n%s", name, path, got, want)
	}
}

// agentTime is the fixtures' clock: a local time, so day labels and hh:mm
// don't move with the machine's time zone.
var agentTime = time.Date(2026, 10, 24, 21, 7, 30, 0, time.Local)

// agentFake is a team with agents: mine (rocky, alice's) is online; bob's
// (juno) is asleep. #general has a call and its reply; my agent DM has
// its greeting; thread 400 holds an approval card.
func agentFake() *fakeClient {
	f := newFake()
	f.ag.agents = []proto.Agent{
		{Handle: "rocky", Icon: "pebble:5", Owner: 1, Online: true, Created: 1},
		{Handle: "juno", Icon: "owl", Owner: 2, Created: 1},
	}
	at := func(min int) int64 { return agentTime.Add(time.Duration(min-30) * time.Minute).UnixMilli() }
	msg := func(id, ch, author uint64, agent string, min int, body string) client.Message {
		return client.Message{Message: proto.Message{ID: id, Chan: ch, Author: author, Agent: agent, Body: body, Time: at(min)}}
	}
	f.msgs[10] = []client.Message{
		msg(500, 10, 2, "", 0, "my manager replied to my 3-page proposal with \"let's revisit after Q4 👍\""),
		msg(501, 10, 3, "", 1, "@rocky what is he really saying?"),
		msg(502, 10, 1, "rocky", 2, "translation: no, but politely.\n\"revisit after Q4\" is where proposals go to rest."),
		msg(503, 10, 1, "", 3, "lol"),
		msg(400, 10, 2, "", 4, "can we get /poll? picking dinner took 43 messages"),
	}
	f.msgs[10][4].Replies = 2
	f.chans[1].Last = 503
	card := msg(401, 10, 1, "rocky", 5, "✋ @alice approve: open a draft PR? react ✅ / ❌  ·  #k3x9q\nplan for /poll, about 15 min and ~$2:\n· proto poll op + vote event\n· tests unit + e2e, then a draft PR")
	card.Thread = 400
	ask := msg(402, 10, 1, "", 5, "@rocky add /poll to tc")
	ask.Thread = 400
	f.threads[400] = []client.Message{f.msgs[10][4], ask, card}
	f.chans = append(f.chans, client.ChannelView{Channel: proto.Channel{ID: 90, Kind: proto.ChanAgentDM, Members: []uint64{1}}, Member: true})
	f.msgs[90] = []client.Message{
		msg(600, 90, 1, "rocky", 0, "hey Alice. I'm Rocky. I live on your laptop and wake up when someone @'s me.\nthree quick ones so I sound less generic:\n1 what should I call you?"),
		msg(601, 90, 1, "", 2, "Alice's fine. roast Bob freely."),
	}
	return f
}

// agentModel is a model over agentFake at w×h with the given ui.json
// extra (e.g. `,"skin":"retro"`), an agent home in a temp dir, and a
// fixed clock.
func agentModel(t *testing.T, f *fakeClient, w, h int, prefs string) *Model {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ui.json")
	if err := os.WriteFile(p, []byte(`{"theme":"graphite"`+prefs+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// the home folder is a temp one, so paths show as ~/… the same everywhere
	hd := t.TempDir()
	t.Setenv("HOME", hd)
	dir := filepath.Join(hd, ".config", "tc", "agent")
	cfg := home.DefaultConfig()
	// it lives here: this machine's id and its device login
	tcHome := filepath.Dir(dir)
	const mid = "0123456789abcdef0123456789abcdef"
	os.MkdirAll(home.PathsFor(dir).DeviceHome, 0o700)
	os.WriteFile(filepath.Join(home.PathsFor(dir).DeviceHome, "config.json"), []byte(`{}`), 0o600)
	os.WriteFile(filepath.Join(tcHome, "machine-id"), []byte(mid+"\n"), 0o600)
	cfg.Handle, cfg.Icon, cfg.HomeMachine = "rocky", "pebble:5", mid
	cfg.Access.Folders = []string{"/src/tc"}
	if err := home.SaveConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: p, AgentDir: dir})
	m.now = func() time.Time { return agentTime }
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.skipIntro()
	return m
}

func frameText(t *testing.T, m *Model, step string) string {
	t.Helper()
	return checkView(t, m, step)
}

// setLive makes agent owner's live status in (ch, thread) and tells the
// model, as Update.AgentStatus does.
func setLive(m *Model, f *fakeClient, ch, thread uint64, owner uint64, state, status, preview string) {
	f.mu.Lock()
	if f.ag.live == nil {
		f.ag.live = map[[2]uint64][]client.AgentLive{}
	}
	var a proto.Agent
	for _, x := range f.ag.agents {
		if x.Owner == owner {
			a = x
		}
	}
	k := [2]uint64{ch, thread}
	if state == "" {
		delete(f.ag.live, k)
	} else {
		f.ag.live[k] = []client.AgentLive{{Agent: a, State: state, Status: status, Preview: preview, At: m.now()}}
	}
	f.mu.Unlock()
	run(m, m.applyUpdate(client.Update{AgentStatus: map[uint64]bool{ch: true}}))
}

func lineWith(t *testing.T, view, s string) string {
	t.Helper()
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, s) {
			return l
		}
	}
	t.Fatalf("no line with %q:\n%s", s, view)
	return ""
}

// ---- identity ----

func TestAgentMessageIdentity(t *testing.T) {
	for _, sk := range []string{"", `,"skin":"retro"`} {
		m := agentModel(t, agentFake(), 120, 40, sk)
		v := frameText(t, m, "wide")
		row := lineWith(t, v, "Rocky ⟡")
		if !strings.Contains(row, "▟▙ Rocky ⟡ · Alice's agent") || !strings.Contains(row, "20:39") {
			t.Errorf("name row: %q", row)
		}
		body := lineWith(t, v, "translation: no")
		if !strings.Contains(body, agentBar+" translation") {
			t.Errorf("agent bar: %q", body)
		}
		if strings.Contains(lineWith(t, v, "lol"), agentBar) {
			t.Error("my own message got the agent bar")
		}
		// the agent's color: its name, glyph and bar
		c := m.lastCanvas
		x, y := find(t, m, "msg:main:2")
		_ = x
		want := m.agentColor(m.ag.byOwner[1])
		found := false
		for xx := 0; xx < c.w; xx++ {
			if cl := c.at(xx, y); cl.s == "R" && cl.fg == want && cl.attr&aBold != 0 {
				found = true
			}
		}
		if !found {
			t.Error("agent name not drawn in its color")
		}
		golden(t, "agent_msg_wide"+skinTag(sk), v)

		m.Update(tea.WindowSizeMsg{Width: 44, Height: 24})
		n := frameText(t, m, "narrow")
		row = lineWith(t, n, "Rocky ⟡")
		if strings.Contains(row, "Alice's agent") {
			t.Errorf("narrow keeps the owner: %q", row)
		}
		golden(t, "agent_msg_narrow"+skinTag(sk), n)
	}
}

func skinTag(prefs string) string {
	if strings.Contains(prefs, "retro") {
		return "_retro"
	}
	return ""
}

// An agent message never groups with its owner's own messages.
func TestAgentMessagesDontGroupWithOwner(t *testing.T) {
	a := client.Message{Message: proto.Message{Author: 1, Agent: "rocky", Time: 1000}}
	b := client.Message{Message: proto.Message{Author: 1, Time: 2000}}
	if grouped(a, b) || grouped(b, a) {
		t.Fatal("agent and owner grouped")
	}
}

func TestAgentIconParsing(t *testing.T) {
	for _, tc := range []struct {
		icon  string
		glyph string
		color int
	}{
		{"orb", "\u25c9", -1}, {"kite:7", "◆", 7}, {"owl:9", "◕◕", -1}, {"🦊", "🦊", -1}, {"", "▟▙", -1}, {"x", "x", -1},
	} {
		l := parseAgentIcon(tc.icon)
		if g := agentFrame(l, "", 0, true); g != tc.glyph || l.color != tc.color {
			t.Errorf("%q: %q %d", tc.icon, g, l.color)
		}
	}
	for _, p := range agentPresets {
		for _, fs := range [][]string{{p.idle, p.done}, p.thinking, p.working, p.asleep} {
			for _, f := range fs {
				if w := strWidth(f); w < 1 || w > 2 {
					t.Errorf("%s frame %q is %d wide", p.id, f, w)
				}
			}
		}
	}
	if agentName("rocky") != "Rocky" || agentIcon(3, 2) != "owl:2" {
		t.Fatal("names")
	}
}

// ---- agents as people ----

func TestAgentsInMentionCompletion(t *testing.T) {
	m := agentModel(t, agentFake(), 100, 30, "")
	typeKeys(m, "@")
	texts := compTexts(m)
	ix := func(s string) int {
		for i, x := range texts {
			if x == s {
				return i
			}
		}
		t.Fatalf("%s not offered: %v", s, texts)
		return -1
	}
	for _, p := range []string{"@bob", "@carol"} {
		if ix(p) > ix("@rocky") || ix(p) > ix("@juno") {
			t.Errorf("agents before people: %v", texts)
		}
	}
	for _, it := range m.comp.items {
		switch it.text {
		case "@rocky":
			if it.desc != "your agent" || it.glyph != "▟▙" {
				t.Errorf("rocky: %+v", it)
			}
		case "@juno":
			if it.desc != "Bob's agent" || it.glyph != "◕◕" {
				t.Errorf("juno: %+v", it)
			}
		}
	}
	row := lineWith(t, frameText(t, m, "@"), "@juno")
	if !strings.Contains(row, "@juno ◕◕") || !strings.Contains(row, "Bob's agent") {
		t.Errorf("popup row: %q", row)
	}

	// a prefix on the handle beats people who only contain it
	typeKeys(m, "ju")
	if texts = compTexts(m); len(texts) == 0 || texts[0] != "@juno" {
		t.Fatalf("@ju: %v", texts)
	}
	send(m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.input.Value(); got != "@juno " {
		t.Errorf("completed to %q", got)
	}
}

func TestAgentsInPalettePeople(t *testing.T) {
	f := agentFake()
	m := agentModel(t, f, 100, 30, "")
	m.openPalette("people")
	var juno, mine *palItem
	for i, it := range m.pal.shown {
		switch it.label {
		case "@juno":
			juno = &m.pal.shown[i]
		case "@rocky":
			mine = &m.pal.shown[i]
		}
	}
	if juno == nil || juno.desc != "Bob's agent" || juno.icon != "◕◕" {
		t.Fatalf("juno row: %+v", juno)
	}
	if mine != nil {
		t.Error("my agent listed twice: its DM is already a row")
	}
	frameText(t, m, "palette")
	run(m, m.palRun(*juno))
	if got := m.input.Value(); got != "@juno " {
		t.Errorf("selecting juno typed %q", got)
	}

	// without its DM yet, my agent's row opens it
	f = agentFake()
	f.chans = f.chans[:len(f.chans)-1]
	m = agentModel(t, f, 100, 30, "")
	m.openPalette("people")
	for _, it := range m.pal.shown {
		if it.label == "@rocky" {
			if it.desc != "your agent" {
				t.Errorf("rocky row: %+v", it)
			}
			run(m, m.palRun(it))
			if !f.called("agentdm") {
				t.Error("my agent's row didn't open its DM")
			}
			return
		}
	}
	t.Fatal("my agent not in People")
}
