package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/agent"
	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/agent/memory"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// The agent panel (⌥K › Agent, or a click on my agent in the sidebar): one
// page, no tabs. State, spend and asks at the top, the mode, then one row
// per setting; Memory and Activity open a page of their own. Settings live
// in the agent home on its home machine (internal/agent/home); the channels
// it reads live on the server (agent.access). With no agent yet it offers
// the setup: a handle and an icon.

// agentHomeState is my agent's home as this machine sees it.
type agentHomeState struct {
	dir  string // the agent home ("" when tc doesn't know where)
	ok   bool   // its config.json is here
	here bool   // and it lives on this computer (agent.HomeHere)
	cfg  home.Config
	err  string // why the config can't be read
}

// loadAgentHome reads my agent's config.json (a small local file).
func (m *Model) loadAgentHome() {
	h := agentHomeState{dir: m.opt.AgentDir}
	if h.dir != "" {
		if _, err := os.Stat(home.PathsFor(h.dir).Config); err == nil {
			cfg, err := home.LoadConfig(h.dir)
			if err != nil {
				h.err = err.Error()
			} else {
				h.ok, h.cfg = true, cfg
				h.here = agent.HomeHere(filepath.Dir(h.dir))
			}
		}
	}
	m.ag.home = h
}

// agentPanel is the panel's state.
type agentPanel struct {
	page    string // "" the main page, or a sub-page (see agentPages)
	row     int
	scroll  int
	confirm string // a pending "y to confirm": "forget:<path>", "undo"
	editing bool   // the line input is open (setup handle, folder path)
	busy    bool

	status   agent.Status
	statusOK bool
	spent    float64
	asks     []askCount
	activity []agent.Activity
	notes    []memory.NoteInfo
	memErr   string
	note     memory.Note // the note page
	access   []uint64
	accessOK bool

	icon, color int // setup choices
	err         string
}

type askCount struct {
	name string
	n    int
}

// Sub-pages.
const (
	apMain     = ""
	apReads    = "reads"
	apFolders  = "folders"
	apLimits   = "limits"
	apMemory   = "memory"
	apNote     = "note"
	apActivity = "activity"
	apSetup    = "setup"
)

// Main page rows, in order.
var apRows = []string{"Mode", "Reads", "Folders", "Limits", "Memory", "Activity", "Personality"}

type (
	agentAccessMsg struct {
		chans []uint64
		err   error
	}
	agentSetupMsg struct {
		a   proto.Agent
		err error
	}
	agentDMMsg struct {
		ch  uint64
		err error
	}
)

// openAgentPanel opens the panel (or the setup when I have no agent).
func (m *Model) openAgentPanel() tea.Cmd {
	m.leaveSelect()
	m.mode = modeAgent
	m.fx.dialogAt = m.now()
	m.ag.panel = agentPanel{icon: 0, color: -1}
	m.loadAgentHome()
	if _, ok := m.myAgent(); !ok {
		m.ag.panel.page = apSetup
		m.line.Reset()
		m.line.Placeholder = "rocky"
		m.line.Prompt = ""
		m.line.Focus()
		m.ag.panel.editing = true
		return nil
	}
	m.refreshPanel()
	return m.fetchAccess()
}

func (m *Model) fetchAccess() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, reqTimeout)
		defer cancel()
		chans, err := m.c.AgentAccess(ctx)
		return agentAccessMsg{chans, err}
	}
}

// refreshPanel re-reads what the panel shows from the agent home.
func (m *Model) refreshPanel() {
	m.loadAgentHome()
	p := &m.ag.panel
	h := m.ag.home
	p.statusOK, p.spent, p.asks, p.activity = false, 0, nil, nil
	if !h.ok {
		return
	}
	state := home.PathsFor(h.dir).State
	if st, err := agent.ReadStatus(state); err == nil && st.State != "" {
		p.status, p.statusOK = st, true
	}
	now := time.Now() // the ledger's days are the supervisor's: wall-clock local days
	if l, err := agent.OpenLedger(state); err == nil {
		p.spent = l.Spent(now)
		for _, u := range m.userList {
			if n := l.Asks(u.ID, now); n > 0 {
				name := sanitize.Line(u.Name)
				if u.ID == m.me.ID {
					name = "you"
				}
				p.asks = append(p.asks, askCount{name, n})
			}
		}
		slices.SortStableFunc(p.asks, func(a, b askCount) int { return b.n - a.n })
	}
	if acts, err := agent.ReadActivity(state, 200); err == nil {
		slices.Reverse(acts)
		p.activity = acts
	}
	p.notes, p.memErr = nil, ""
	if st, err := memory.Open(h.dir); err != nil {
		p.memErr = err.Error()
	} else if notes, err := st.List(); err != nil {
		p.memErr = err.Error()
	} else {
		p.notes = notes
	}
}

// runCost is what my agent's run cost: the audit log's entries inside it.
func (m *Model) runCost(run *agentRun) float64 {
	if !m.ag.home.ok {
		return 0
	}
	acts, err := agent.ReadActivity(home.PathsFor(m.ag.home.dir).State, 50)
	if err != nil {
		return 0
	}
	var usd float64
	for _, a := range acts {
		t := time.UnixMilli(a.T)
		if !t.Before(run.start.Add(-time.Second)) && !t.After(run.end.Add(time.Minute)) {
			usd += a.USD
		}
	}
	return usd
}

// saveAgentConfig writes a changed config and says so.
func (m *Model) saveAgentConfig(change func(*home.Config)) {
	h := &m.ag.home
	if !h.ok || !h.here {
		m.setInfo("its settings live on its home computer · open tc there")
		return
	}
	cfg := h.cfg
	change(&cfg)
	if err := home.SaveConfig(h.dir, cfg); err != nil {
		m.setErr("agent", err)
		return
	}
	h.cfg = cfg
}

// agentMsg handles the panel's async results; false: not one of them.
func (m *Model) agentMsg(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case agentAccessMsg:
		m.ag.panel.busy = false
		if msg.err != nil {
			if m.mode == modeAgent {
				m.ag.panel.err = errText(msg.err)
			}
			return nil, true
		}
		m.ag.panel.access, m.ag.panel.accessOK = msg.chans, true
		return nil, true
	case agentSetupMsg:
		p := &m.ag.panel
		p.busy = false
		if msg.err != nil {
			p.err = errText(msg.err)
			return nil, true
		}
		m.reloadAgents()
		m.loadAgentHome()
		m.setInfo(agentName(msg.a.Handle) + " is set up · it starts with tc background")
		p.page, p.row, p.editing, p.err = apMain, 0, false, ""
		m.line.Blur()
		m.refreshPanel()
		return tea.Batch(m.fetchAccess(), m.openAgentDM()), true
	case agentDMMsg:
		if msg.err != nil {
			m.setErr("agent DM", msg.err)
			return nil, true
		}
		m.reloadChans()
		m.reloadAgents()
		if m.mode == modeAgent {
			m.mode = modeNormal
		}
		return m.openChannel(msg.ch), true
	}
	return nil, false
}

// openAgentDM opens (creating if needed) my chat with my agent.
func (m *Model) openAgentDM() tea.Cmd {
	if m.ag.dm != 0 {
		if m.mode == modeAgent {
			m.mode = modeNormal
		}
		return m.openChannel(m.ag.dm)
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, reqTimeout)
		defer cancel()
		ch, err := m.c.OpenAgentDM(ctx)
		return agentDMMsg{ch.ID, err}
	}
}

// ---- keys ----

func (m *Model) agentPanelKey(k tea.KeyMsg) tea.Cmd {
	key := keyName(k)
	p := &m.ag.panel
	if p.confirm != "" {
		c := p.confirm
		p.confirm = ""
		if key == kConfirmYes {
			m.panelConfirm(c)
		}
		return nil
	}
	if p.editing {
		return m.panelEditKey(k, key)
	}
	if key == kEsc || key == "q" {
		if p.page == apMain || p.page == apSetup {
			m.mode = modeNormal
			return nil
		}
		back := map[string]string{apNote: apMemory}[p.page]
		row := 0
		if back == apMain {
			row = max(slices.Index(apRows, apTitles[p.page]), 0) // back on the row it came from
		}
		p.page, p.row, p.scroll = back, row, 0
		return nil
	}
	switch p.page {
	case apSetup:
		return m.setupKey(key)
	case apMain:
		return m.panelMainKey(key)
	case apReads:
		return m.readsKey(key)
	case apFolders:
		return m.foldersKey(key)
	case apLimits:
		m.limitsKey(key)
	case apMemory:
		m.memoryKey(key)
	case apActivity, apNote:
		switch key {
		case kUp, "k":
			p.scroll = max(p.scroll-1, 0)
		case kDown, "j":
			p.scroll++
		case kPgUp:
			p.scroll = max(p.scroll-10, 0)
		case kPgDown:
			p.scroll += 10
		}
	}
	return nil
}

func (m *Model) panelMainKey(key string) tea.Cmd {
	p := &m.ag.panel
	switch key {
	case kUp, "k":
		p.row = max(p.row-1, 0)
		return nil
	case kDown, "j":
		p.row = min(p.row+1, len(apRows)-1)
		return nil
	case "p":
		paused := !m.ag.home.cfg.Paused
		m.saveAgentConfig(func(c *home.Config) { c.Paused = paused })
		if m.ag.home.ok {
			if paused {
				m.setInfo("paused · it answers nobody until you resume")
			} else {
				m.setInfo("resumed")
			}
		}
		return nil
	case "d":
		return m.openAgentDM()
	case "l":
		p.page, p.row, p.scroll = apActivity, 0, 0
		return nil
	case "r":
		m.refreshPanel()
		return m.fetchAccess()
	}
	row := apRows[min(p.row, len(apRows)-1)]
	if row == "Mode" {
		modes := []home.Mode{home.ModeTalk, home.ModeWork, home.ModeCrew}
		i := slices.Index(modes, m.ag.home.cfg.Mode)
		switch key {
		case kLeft, "h":
			i = max(i-1, 0)
		case kRight, "l", kEnter, " ":
			if key == kEnter || key == " " {
				i = (i + 1) % 3
			} else {
				i = min(i+1, 2)
			}
		case "1", "2", "3":
			i = int(key[0] - '1')
		default:
			return nil
		}
		mode := modes[max(i, 0)]
		m.saveAgentConfig(func(c *home.Config) { c.Mode = mode })
		return nil
	}
	if key != kEnter && key != " " && key != kRight {
		return nil
	}
	return m.panelOpenRow(row)
}

// panelOpenRow is ⏎ (or a click) on a main page row.
func (m *Model) panelOpenRow(row string) tea.Cmd {
	p := &m.ag.panel
	switch row {
	case "Reads":
		p.page = apReads
	case "Folders":
		p.page = apFolders
	case "Limits":
		p.page = apLimits
	case "Memory":
		p.page = apMemory
	case "Activity":
		p.page = apActivity
	case "Personality":
		return m.openAgentDM()
	default:
		return nil
	}
	p.row, p.scroll = 0, 0
	return nil
}

// readsChans are the channels the Reads page lists: the ones I'm in.
func (m *Model) readsChans() []uint64 {
	var out []uint64
	for _, v := range m.chans {
		if v.Member && !proto.IsDMKind(v.Kind) {
			out = append(out, v.ID)
		}
	}
	return out
}

func (m *Model) readsKey(key string) tea.Cmd {
	p := &m.ag.panel
	chans := m.readsChans()
	n := len(chans) + 1 // the DMs row first
	switch key {
	case kUp, "k":
		p.row = max(p.row-1, 0)
	case kDown, "j":
		p.row = min(p.row+1, n-1)
	case kEnter, " ":
		if p.row == 0 {
			dms := !m.ag.home.cfg.Access.DMs
			m.saveAgentConfig(func(c *home.Config) { c.Access.DMs = dms })
			return nil
		}
		return m.toggleAccess(chans[p.row-1])
	}
	return nil
}

// toggleAccess lets my agent read ch for anyone's asks, or stops it.
func (m *Model) toggleAccess(ch uint64) tea.Cmd {
	p := &m.ag.panel
	if p.busy || !p.accessOK {
		return nil
	}
	on := slices.Contains(p.access, ch)
	var add, rm []uint64
	if on {
		rm = []uint64{ch}
	} else {
		add = []uint64{ch}
	}
	m.saveAgentConfig(func(c *home.Config) {
		c.Access.Chans = slices.DeleteFunc(c.Access.Chans, func(x uint64) bool { return x == ch })
		if !on {
			c.Access.Chans = append(c.Access.Chans, ch)
		}
	})
	p.busy = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, reqTimeout)
		defer cancel()
		chans, err := m.c.SetAgentAccess(ctx, add, rm)
		return agentAccessMsg{chans, err}
	}
}

func (m *Model) foldersKey(key string) tea.Cmd {
	p := &m.ag.panel
	folders := m.ag.home.cfg.Access.Folders
	switch key {
	case kUp, "k":
		p.row = max(p.row-1, 0)
	case kDown, "j":
		p.row = min(p.row+1, max(len(folders)-1, 0))
	case "a", kEnter:
		if !m.ag.home.ok {
			m.saveAgentConfig(func(*home.Config) {}) // says where settings live
			return nil
		}
		p.editing = true
		m.line.Reset()
		m.line.Prompt = ""
		m.line.Placeholder = "~/Projects/something"
		m.line.Focus()
	case "x", "backspace", "delete":
		if p.row < len(folders) {
			f := folders[p.row]
			m.saveAgentConfig(func(c *home.Config) {
				c.Access.Folders = slices.DeleteFunc(slices.Clone(c.Access.Folders), func(x string) bool { return x == f })
			})
			p.row = max(min(p.row, len(m.ag.home.cfg.Access.Folders)-1), 0)
		}
	}
	return nil
}

// limitSteps: the Limits page's rows, each nudged by ←/→.
var limitSteps = []struct {
	label string
	get   func(home.Config) float64
	set   func(*home.Config, float64)
	step  float64
	lo    float64
	hi    float64
	shown func(float64) string
}{
	{"Daily cap", func(c home.Config) float64 { return c.DailyCapUSD }, func(c *home.Config, v float64) { c.DailyCapUSD = v }, 5, 5, 500,
		func(v float64) string { return fmt.Sprintf("$%g a day", v) }},
	{"Per run", func(c home.Config) float64 { return c.RunCapUSD }, func(c *home.Config, v float64) { c.RunCapUSD = v }, 0.5, 0.5, 50,
		func(v float64) string { return fmt.Sprintf("$%g at most", v) }},
	{"Asks per person", func(c home.Config) float64 { return float64(c.AsksPerPerson) }, func(c *home.Config, v float64) { c.AsksPerPerson = int(v) }, 5, 5, 500,
		func(v float64) string { return fmt.Sprintf("%g a day", v) }},
	{"Roll over at", func(c home.Config) float64 { return float64(c.RolloverPct) }, func(c *home.Config, v float64) { c.RolloverPct = int(v) }, 10, 10, 90,
		func(v float64) string { return fmt.Sprintf("%g%% of its context", v) }},
	{"Sleep after", func(c home.Config) float64 { return float64(c.IdleMinutes) }, func(c *home.Config, v float64) { c.IdleMinutes = int(v) }, 15, 15, 24 * 60,
		func(v float64) string { return idleText(int(v)) + " idle" }},
}

func idleText(min int) string {
	if min%60 == 0 {
		return strconv.Itoa(min/60) + "h"
	}
	return strconv.Itoa(min) + "m"
}

func (m *Model) limitsKey(key string) {
	p := &m.ag.panel
	switch key {
	case kUp, "k":
		p.row = max(p.row-1, 0)
		return
	case kDown, "j":
		p.row = min(p.row+1, len(limitSteps)-1)
		return
	}
	d := 0.0
	switch key {
	case kLeft, "h", "-":
		d = -1
	case kRight, "l", "+", kEnter, " ":
		d = 1
	default:
		return
	}
	ls := limitSteps[p.row]
	v := min(max(ls.get(m.ag.home.cfg)+d*ls.step, ls.lo), ls.hi)
	m.saveAgentConfig(func(c *home.Config) {
		ls.set(c, v)
		if c.RunCapUSD > c.DailyCapUSD {
			c.RunCapUSD = c.DailyCapUSD
		}
	})
}

func (m *Model) memoryKey(key string) {
	p := &m.ag.panel
	switch key {
	case kUp, "k":
		p.row = max(p.row-1, 0)
	case kDown, "j":
		p.row = min(p.row+1, max(len(p.notes)-1, 0))
	case kEnter, " ":
		if p.row < len(p.notes) {
			st, err := memory.Open(m.ag.home.dir)
			if err == nil {
				p.note, err = st.Read(p.notes[p.row].Path)
			}
			if err != nil {
				m.setErr("memory", err)
				return
			}
			p.page, p.scroll = apNote, 0
		}
	case "f", "x":
		if p.row < len(p.notes) {
			p.confirm = "forget:" + p.notes[p.row].Path
		}
	case "u":
		p.confirm = "undo"
	}
}

// panelConfirm runs a confirmed forget or undo.
func (m *Model) panelConfirm(c string) {
	st, err := memory.Open(m.ag.home.dir)
	if err != nil {
		m.setErr("memory", err)
		return
	}
	if path, ok := strings.CutPrefix(c, "forget:"); ok {
		if err := st.Forget(path); err != nil {
			m.setErr("forget", err)
			return
		}
		m.setInfo("forgot " + path)
	} else if c == "undo" {
		n, err := st.UndoToday()
		if err != nil {
			m.setErr("undo", err)
			return
		}
		m.setInfo("undid today's memory: " + strconv.Itoa(n) + " " + plural(n, "change", "changes"))
	}
	m.refreshPanel()
	m.ag.panel.row = min(m.ag.panel.row, max(len(m.ag.panel.notes)-1, 0))
}

// panelEditKey types into the line input (setup handle, a folder).
func (m *Model) panelEditKey(k tea.KeyMsg, key string) tea.Cmd {
	p := &m.ag.panel
	switch key {
	case kEsc:
		if p.page == apSetup {
			m.mode = modeNormal
			return nil
		}
		p.editing = false
		m.line.Blur()
		return nil
	case kEnter:
		if p.page == apSetup {
			return m.submitSetup()
		}
		path := expandHome(strings.TrimSpace(m.line.Value()))
		if path == "" {
			return nil
		}
		if !filepath.IsAbs(path) {
			m.setInfo("a folder is a full path, like ~/Projects/tc")
			return nil
		}
		m.saveAgentConfig(func(c *home.Config) {
			if !slices.Contains(c.Access.Folders, path) {
				c.Access.Folders = append(slices.Clone(c.Access.Folders), filepath.Clean(path))
			}
		})
		p.editing = false
		m.line.Blur()
		return nil
	case kTab, kDown, kUp:
		if p.page == apSetup {
			p.editing = false
			m.line.Blur()
			p.row = 1
			return nil
		}
	}
	var cmd tea.Cmd
	m.line, cmd = m.line.Update(k)
	return cmd
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~"); ok && (rest == "" || rest[0] == '/') {
		if h, err := os.UserHomeDir(); err == nil {
			return h + rest
		}
	}
	return p
}

// ---- setup ----

// setupRows: handle, icon, color, the button.
const setupRows = 4

func (m *Model) setupKey(key string) tea.Cmd {
	p := &m.ag.panel
	switch key {
	case kUp, "k", kShiftTab:
		if p.row == 1 {
			p.row = 0
			p.editing = true
			m.line.Focus()
			return nil
		}
		p.row = max(p.row-1, 0)
	case kDown, "j", kTab:
		p.row = min(p.row+1, setupRows-1)
	case kLeft, "h":
		m.setupNudge(-1)
	case kRight, "l":
		m.setupNudge(1)
	case kEnter, " ":
		if p.row == 0 {
			p.editing = true
			m.line.Focus()
			return nil
		}
		if p.row == setupRows-1 {
			return m.submitSetup()
		}
		m.setupNudge(1)
	}
	return nil
}

func (m *Model) setupNudge(d int) {
	p := &m.ag.panel
	switch p.row {
	case 1:
		p.icon = (p.icon + d + len(agentPresets)) % len(agentPresets)
	case 2:
		p.color = (p.color+d+1+9)%9 - 1 // -1 (from the handle), 0–7
	}
}

// setupHandle is the handle typed, or the placeholder's.
func (m *Model) setupHandle() string {
	h := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(m.line.Value()), "@"))
	if h == "" {
		h = "rocky"
	}
	return h
}

// submitSetup registers my agent and sets up its home here.
func (m *Model) submitSetup() tea.Cmd {
	p := &m.ag.panel
	h := m.setupHandle()
	if !proto.ValidAgentHandle(h) {
		p.err = "a handle is lowercase letters, digits, - and _ (and not a reserved name)"
		return nil
	}
	if p.busy {
		return nil
	}
	p.busy, p.err = true, ""
	icon := agentIcon(p.icon, p.color)
	setup := m.opt.AgentSetup
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, reqTimeout)
		defer cancel()
		a, err := m.c.RegisterAgent(ctx, h, icon)
		if err != nil {
			return agentSetupMsg{err: err}
		}
		if setup != nil {
			if err := setup(ctx, a.Handle, a.Icon); err != nil {
				return agentSetupMsg{a: a, err: fmt.Errorf("%s is registered, but its home: %w · run tc agent setup", a.Handle, err)}
			}
		}
		return agentSetupMsg{a: a}
	}
}

// ---- mouse ----

// agentPanelClick handles "ap:<what>[:<arg>]".
func (m *Model) agentPanelClick(rest string) tea.Cmd {
	p := &m.ag.panel
	what, arg, _ := strings.Cut(rest, ":")
	n, _ := strconv.Atoi(arg)
	switch what {
	case "row":
		p.row = n
		if p.page == apMain && n < len(apRows) {
			if apRows[n] == "Mode" {
				return nil
			}
			return m.panelOpenRow(apRows[n])
		}
		if p.page == apReads || p.page == apMemory || p.page == apLimits {
			return m.agentPanelKey(tea.KeyMsg{Type: tea.KeyEnter})
		}
	case "mode":
		modes := []home.Mode{home.ModeTalk, home.ModeWork, home.ModeCrew}
		p.row = 0
		m.saveAgentConfig(func(c *home.Config) { c.Mode = modes[min(max(n, 0), 2)] })
	case "key":
		return m.agentPanelKey(keyMsg1(arg))
	case "icon":
		p.icon = min(max(n, 0), len(agentPresets)-1)
		p.row = 1
	case "color":
		p.color = min(max(n, -1), 7)
		p.row = 2
	case "setup":
		return m.submitSetup()
	case "handle":
		p.row, p.editing = 0, true
		m.line.Focus()
	}
	return nil
}

// keyMsg1 is a single-key message for a clicked key hint.
func keyMsg1(k string) tea.KeyMsg {
	switch k {
	case kEsc:
		return tea.KeyMsg{Type: tea.KeyEscape}
	case kEnter:
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return keyMsgRune([]rune(k)[0])
}
