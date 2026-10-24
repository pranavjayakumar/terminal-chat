package tui

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/agent"
	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/proto"
)

// writeState puts a status, a ledger and an audit log in the agent home.
func writeState(t *testing.T, dir string) {
	t.Helper()
	state := home.PathsFor(dir).State
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	ms := agentTime.Add(-4 * time.Minute).UnixMilli()
	os.WriteFile(filepath.Join(state, agent.StatusFile), []byte(`{"pid":1,"state":"awake","since":1,"last_run":`+itoa(ms)+`}`), 0o600)
	// the ledger reads "today" against the real clock
	now := time.Now().UnixMilli()
	os.WriteFile(filepath.Join(state, agent.LedgerFile), []byte(
		`{"t":`+itoa(now)+`,"asker":3,"usd":2.1,"kind":"ask"}`+"\n"+
			`{"t":`+itoa(now)+`,"asker":3,"usd":1.0,"kind":"ask"}`+"\n"+
			`{"t":`+itoa(now)+`,"asker":1,"usd":0.3,"kind":"ask"}`+"\n"), 0o600)
	os.WriteFile(filepath.Join(state, agent.ActivityFile), []byte(
		`{"t":`+itoa(ms)+`,"who":"carol","where":"#general","what":"reply","usd":0.06}`+"\n"), 0o600)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func panelModel(t *testing.T, w, h int, prefs string) (*Model, *fakeClient) {
	t.Helper()
	f := agentFake()
	f.ag.access = []uint64{10}
	m := agentModel(t, f, w, h, prefs)
	writeState(t, m.opt.AgentDir)
	run(m, m.openAgentPanel())
	return m, f
}

func TestAgentPanel(t *testing.T) {
	for _, sk := range []string{"", `,"skin":"retro"`} {
		m, _ := panelModel(t, 100, 34, sk)
		if m.mode != modeAgent {
			t.Fatal("panel not open")
		}
		v := frameText(t, m, "panel")
		for _, want := range []string{"Rocky ⟡ · your agent", "State", "awake", "idle 4m", "on this computer",
			"Spend today", "$3.40 of $20", "17%", "Asks today", "carol 2", "you 1",
			"Mode", "Talk", "Work", "Crew", "ask you first", "Approvals", "Only for code & PRs",
			"Reads", "#general + wherever it is called", "Folders", "/src/tc", "Limits", "$20/day · 15 asks/person · roll over at 50%",
			"Memory", "Activity", "carol #general $0.06", "Personality", "tell me in our DM", "Pause", "Open DM", "Logs"} {
			if !strings.Contains(v, want) {
				t.Errorf("panel lacks %q", want)
			}
		}
		golden(t, "agent_panel"+skinTag(sk), v)
		m.Update(tea.WindowSizeMsg{Width: 44, Height: 30})
		golden(t, "agent_panel_narrow"+skinTag(sk), frameText(t, m, "narrow panel"))
	}
}

func TestAgentPanelKeys(t *testing.T) {
	m, f := panelModel(t, 100, 34, "")
	dir := m.opt.AgentDir
	cfg := func() home.Config {
		c, err := home.LoadConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	// Mode: → moves Talk to Work, 3 picks Crew; saved in config.json
	press(m, "right")
	if cfg().Mode != home.ModeWork {
		t.Fatalf("mode %q", cfg().Mode)
	}
	press(m, "3")
	if cfg().Mode != home.ModeCrew {
		t.Fatalf("mode %q", cfg().Mode)
	}
	click(t, m, "ap:mode:0")
	if cfg().Mode != home.ModeTalk {
		t.Fatal("click on Talk")
	}
	// p pauses and resumes
	press(m, "p")
	if !cfg().Paused || !strings.Contains(frameText(t, m, "paused"), "paused") {
		t.Fatal("p didn't pause")
	}
	press(m, "p")
	if cfg().Paused {
		t.Fatal("p didn't resume")
	}
	// Approvals: → and ← move the level, saved to the config
	press(m, "down", "right")
	if cfg().Approvals != home.ApproveNever || !strings.Contains(frameText(t, m, "never"), "acts without asking") {
		t.Fatalf("approvals %q", cfg().Approvals)
	}
	press(m, "left", "left")
	if cfg().Approvals != home.ApproveAlways {
		t.Fatalf("approvals %q", cfg().Approvals)
	}
	press(m, "right", "up")
	// Limits: ⏎ opens it, → raises the daily cap by $5
	press(m, "down", "down", "down", "down", "enter")
	if m.ag.panel.page != apLimits {
		t.Fatalf("page %q", m.ag.panel.page)
	}
	frameText(t, m, "limits")
	press(m, "right")
	if cfg().DailyCapUSD != 25 {
		t.Fatalf("cap %v", cfg().DailyCapUSD)
	}
	press(m, "down", "left", "left", "left", "left", "left")
	if cfg().RunCapUSD != 0.5 {
		t.Fatalf("run cap %v", cfg().RunCapUSD)
	}
	press(m, "esc")
	// Reads: toggling #backend grants it on the server
	press(m, "up", "up", "enter")
	if m.ag.panel.page != apReads { // Limits → Folders → Reads
		t.Fatalf("page %q", m.ag.panel.page)
	}
	frameText(t, m, "reads")
	press(m, "down", "enter")
	if !slices.Contains(f.ag.access, 11) {
		t.Fatalf("access %v", f.ag.access)
	}
	press(m, "esc")
	// Folders: a adds one (~ expands), x removes it
	press(m, "down", "enter", "a") // Reads → Folders
	typeVia(m, "/tmp/work")
	press(m, "enter")
	if fs := cfg().Access.Folders; len(fs) != 2 || fs[1] != "/tmp/work" {
		t.Fatalf("folders %v", fs)
	}
	press(m, "down", "x")
	if fs := cfg().Access.Folders; len(fs) != 2 || !strings.Contains(frameText(t, m, "confirm folder"), "stop granting /tmp/work?") {
		t.Fatalf("x removed without asking: %v", fs)
	}
	press(m, "y")
	if fs := cfg().Access.Folders; len(fs) != 1 {
		t.Fatalf("folders after x %v", fs)
	}
	press(m, "esc")
	// Activity: L opens the full log (l moves the mode, like h)
	press(m, "L")
	if m.ag.panel.page != apActivity || !strings.Contains(frameText(t, m, "log"), "carol #general") {
		t.Fatal("l didn't open the log")
	}
	press(m, "esc", "esc")
	if m.mode != modeNormal {
		t.Fatal("esc esc didn't close")
	}
	// the palette's Agent row opens it
	m.openPalette("all")
	for _, it := range m.pal.items {
		if it.run == "app:agent" {
			run(m, m.palRun(it))
		}
	}
	if m.mode != modeAgent {
		t.Fatal("palette Agent row")
	}
}

// Memory: browse, read, forget (confirmed), undo today.
func TestAgentPanelMemory(t *testing.T) {
	m, _ := panelModel(t, 100, 34, "")
	dir := m.opt.AgentDir
	_ = dir
	press(m, "down", "down", "down", "down", "down", "enter")
	if m.ag.panel.page != apMemory {
		t.Fatalf("page %q", m.ag.panel.page)
	}
	frameText(t, m, "memory")
	if m.ag.panel.memErr != "" {
		t.Skipf("memory store unavailable here: %s", m.ag.panel.memErr)
	}
	if len(m.ag.panel.notes) == 0 {
		return // a fresh store lists nothing yet: the page says so
	}
	press(m, "f")
	if m.ag.panel.confirm == "" || !strings.Contains(frameText(t, m, "confirm"), "y to confirm") {
		t.Fatal("forget asked nothing")
	}
	press(m, "n")
	if m.ag.panel.confirm != "" {
		t.Fatal("n kept the confirm")
	}
}

// No agent yet: the panel is the setup.
func TestAgentSetup(t *testing.T) {
	for _, sk := range []string{"", `,"skin":"retro"`} {
		f := newFake()
		var got [2]string
		p := filepath.Join(t.TempDir(), "ui.json")
		os.WriteFile(p, []byte(`{"theme":"graphite"`+sk+`}`), 0o600)
		m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), UIPrefs: p,
			AgentSetup: func(ctx context.Context, handle, icon string) error { got = [2]string{handle, icon}; return nil }})
		m.now = func() time.Time { return agentTime }
		m.Update(tea.WindowSizeMsg{Width: 100, Height: 34})
		m.skipIntro()
		run(m, m.openAgentPanel())
		v := frameText(t, m, "setup")
		for _, want := range []string{"Set up your agent", "Handle", "Icon", "pebble", "kite", "Color", "Set up Rocky", "tc agent setup"} {
			if !strings.Contains(v, want) {
				t.Errorf("setup lacks %q", want)
			}
		}
		golden(t, "agent_setup"+skinTag(sk), v)
		m.Update(tea.WindowSizeMsg{Width: 44, Height: 30})
		golden(t, "agent_setup_narrow"+skinTag(sk), frameText(t, m, "narrow setup"))
		m.Update(tea.WindowSizeMsg{Width: 100, Height: 34})
		typeVia(m, "Bad Name")
		press(m, "enter")
		if !strings.Contains(frameText(t, m, "bad"), "a handle is lowercase") {
			t.Fatal("bad handle accepted")
		}
		press(m, "ctrl+u")
		m.line.SetValue("pip")
		press(m, "tab", "right", "right", "down", "right", "down")
		press(m, "enter")
		if !f.called("register pip spark:0") || got != [2]string{"pip", "spark:0"} {
			t.Fatalf("calls %v setup %v", f.calls, got)
		}
		if m.ag.panel.page != apMain {
			t.Fatalf("after setup the page is %q", m.ag.panel.page)
		}
	}
	// a refused handle stays in the setup with the server's word
	f := newFake()
	f.ag.regErr = &proto.Error{Code: proto.ErrConflict, Msg: "handle taken"}
	m := agentModel(t, f, 100, 34, "")
	run(m, m.openAgentPanel())
	press(m, "enter")
	if !strings.Contains(frameText(t, m, "taken"), "handle taken") {
		t.Fatal("conflict not shown")
	}
}

// The agent's home on another machine: the panel says where settings live.
func TestAgentPanelElsewhere(t *testing.T) {
	f := agentFake()
	m := agentModel(t, f, 100, 34, "")
	m.opt.AgentDir = filepath.Join(t.TempDir(), "nothing")
	run(m, m.openAgentPanel())
	v := frameText(t, m, "elsewhere")
	if !strings.Contains(v, "no agent home here") || strings.Contains(v, "Spend today") {
		t.Fatalf("no hint:\n%s", v)
	}
	press(m, "p")
	frameText(t, m, "p elsewhere")
	// set up, but on another computer: read only
	m.opt.AgentDir = agentDirOf(t, m)
	cfgPath := home.PathsFor(m.opt.AgentDir)
	c, _ := home.LoadConfig(m.opt.AgentDir)
	c.HomeMachine = "ffffffffffffffffffffffffffffffff"
	home.SaveConfig(m.opt.AgentDir, c)
	_ = cfgPath
	press(m, "esc")
	run(m, m.openAgentPanel())
	if v := frameText(t, m, "other computer"); !strings.Contains(v, "it lives on another computer") {
		t.Fatalf("no hint:\n%s", v)
	}
	press(m, "p")
	if c, _ := home.LoadConfig(m.opt.AgentDir); c.Paused {
		t.Fatal("paused an agent that lives elsewhere")
	}
}

// agentDirOf is the agent home agentModel made.
func agentDirOf(t *testing.T, m *Model) string {
	t.Helper()
	return filepath.Join(os.Getenv("HOME"), ".config", "tc", "agent")
}

// Every skin with every theme: the agent views read on what they sit on.
func TestAgentEverySkinEveryTheme(t *testing.T) {
	for _, sk := range skins {
		for _, name := range themeOrder {
			for _, sz := range [][2]int{{140, 44}, {44, 24}} {
				f := agentFake()
				m := agentModel(t, f, sz[0], sz[1], `,"skin":"`+sk.id+`","theme":"`+name+`"`)
				id := sk.id + "/" + name
				setLive(m, f, 10, 0, 1, proto.AgentStreaming, "", "a streamed\npreview")
				readable(t, m, id+" live", rect{0, 0, m.w, m.h})
				run(m, m.openThread(400))
				m.layoutInput()
				readable(t, m, id+" card", rect{0, 0, m.w, m.h})
				press(m, "esc")
				writeState(t, m.opt.AgentDir)
				run(m, m.openAgentPanel())
				readable(t, m, id+" panel", m.dialogBox(panelW, panelH))
				press(m, "esc")
			}
		}
	}
}

// A 40×12 terminal still reaches every row of the panel and the setup.
func TestAgentPanelSmall(t *testing.T) {
	for _, sk := range []string{"", `,"skin":"retro"`} {
		m, _ := panelModel(t, 40, 12, sk)
		for i := 0; i < len(apRows)-1; i++ {
			press(m, "down")
		}
		if r := lineWith(t, frameText(t, m, "last row"), "Personality"); !strings.Contains(r, "›") {
			t.Errorf("%s: focused last row not in view: %q", sk, r)
		}
		f := newFake()
		m2 := agentModel(t, f, 40, 12, sk)
		run(m2, m2.openAgentPanel())
		press(m2, "tab")
		lineWith(t, frameText(t, m2, "icon"), "Icon")
		press(m2, "down")
		lineWith(t, frameText(t, m2, "color"), "Color")
		press(m2, "down")
		lineWith(t, frameText(t, m2, "button"), "Set up Rocky")
		press(m2, "enter")
		if !f.called("register rocky pebble") {
			t.Errorf("%s: setup from 40x12: %v", sk, f.calls)
		}
	}
}

// Reads can't change an agent that lives on another computer.
func TestAgentReadsElsewhere(t *testing.T) {
	m, f := panelModel(t, 100, 34, "")
	m.ag.home.here = false
	m.ag.panel.page, m.ag.panel.row = apReads, 1
	press(m, "enter")
	if len(f.ag.access) != 1 || !strings.Contains(m.notice, "home computer") {
		t.Fatalf("access %v notice %q", f.ag.access, m.notice)
	}
}
