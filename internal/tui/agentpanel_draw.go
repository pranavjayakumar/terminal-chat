package tui

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/skailama/tc/internal/agent"
	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// panelW, panelH: the panel's size (the mockup's 84×24 screen holds it).
const (
	panelW = 84
	panelH = 30
)

// apDraw is one panel page being drawn: a cursor down the content.
type apDraw struct {
	m      *Model
	c      *canvas
	x, w   int // content column and width
	y, bot int
	labelW int
	focusY int // the focused row's y, -1 none: the view scrolls to it
}

func (d *apDraw) left() int { return d.bot - d.y }

// rule is a thin line across the content.
func (d *apDraw) rule() {
	if d.y < d.bot {
		d.c.put(d.x, d.y, strings.Repeat("─", d.w), fgc(d.m.st.th.line), "")
	}
	d.y++
}

// row draws a label and returns where its value starts.
func (d *apDraw) row(label string, focused bool, act string) int {
	th := d.m.st.th
	if d.y >= d.bot {
		return d.x + d.labelW
	}
	st := fgc(th.dim)
	if focused {
		st = fgb(th.accent)
		d.c.put(d.x-2, d.y, "›", fgc(th.accent), act)
		d.focusY = d.y
	}
	if act != "" {
		d.c.tag(d.x, d.y, d.w, 1, act, true, true)
	}
	d.c.put(d.x, d.y, truncate(label, d.labelW-1), st, act)
	return d.x + d.labelW
}

func (m *Model) drawAgentPanel(c *canvas) {
	th := m.st.th
	r := m.dialog(c, panelW, panelH, "")
	in := m.inside(r)
	old := c.setClip(in)
	defer c.setClip(old)
	x := in.x0 + 3
	if in.x1-in.x0 < 40 {
		x = in.x0 + 2
	}
	d := &apDraw{m: m, c: c, x: x, w: in.x1 - x - 2, y: in.y0 + 1, bot: in.y1 - 2, labelW: 15}
	if d.w < 52 {
		d.labelW = 12
	}
	p := &m.ag.panel
	// the title row: the agent, or the page
	a, have := m.myAgent()
	tx := d.x
	if have {
		st, since := m.agentStateOf(a)
		g, col := m.agentGlyph(a, st, since, th.panel)
		tx = c.put(tx, d.y, g, fgc(col), "") + 1
		tx = c.put(tx, d.y, agentName(a.Handle), fgb(col), "")
		tx = c.put(tx+1, d.y, agentBadge, fgc(col), "")
		sub := " · your agent"
		if t := apTitles[p.page]; t != "" {
			sub = " › " + t
		}
		c.put(tx, d.y, truncate(sub, max(d.x+d.w-tx-10, 0)), fgc(th.faint), "")
	} else {
		c.put(tx, d.y, "Set up your agent", fgb(th.fg), "")
	}
	esc := "esc close"
	if p.page != apMain && p.page != apSetup {
		esc = "esc back"
	}
	if d.w > 30 {
		c.put(d.x+d.w-strWidth(esc), d.y, esc, fgc(th.faint), "close")
	}
	d.y += 2
	if in.y1-in.y0 < 16 {
		d.y-- // a short panel: no blank row under the title
	}
	switch p.page {
	case apNote:
		m.drawNote(d)
	case apActivity:
		m.drawActivity(d)
	default:
		m.drawPanelScrolled(c, d, in)
	}
	// the bottom row: an error, a confirm, or the keys
	y := in.y1 - 2
	switch {
	case p.confirm != "":
		what := "undo every memory change made today?"
		if path, ok := strings.CutPrefix(p.confirm, "forget:"); ok {
			what = "forget " + sanitize.Line(path) + "?"
		}
		if f, ok := strings.CutPrefix(p.confirm, "folder:"); ok {
			what = "stop granting " + tildePath(f) + "?"
		}
		c.put(d.x, y, truncate(what+"  y to confirm", d.w), fgb(th.warn), "")
	case p.err != "":
		c.put(d.x, y, truncate("! "+p.err, d.w), fgc(th.err), "")
	default:
		m.drawPanelKeys(d, y)
	}
}

var apTitles = map[string]string{
	apReads: "Reads", apFolders: "Folders", apLimits: "Limits", apMemory: "Memory", apNote: "Memory", apActivity: "Activity",
}

// drawPanelKeys draws the key hints along the bottom.
func (m *Model) drawPanelKeys(d *apDraw, y int) {
	th := m.st.th
	c := d.c
	var pills [][2]string
	hint := "↑↓ move · ⏎ change · esc close"
	switch m.ag.panel.page {
	case apMain:
		pause := "Pause"
		if m.ag.home.cfg.Paused {
			pause = "Resume"
		}
		pills = [][2]string{{"p", pause}, {"d", "Open DM"}, {"L", "Logs"}}
	case apSetup:
		hint = "↑↓ move · ←→ pick · ⏎ set up · esc"
	case apReads:
		hint = "⏎ on/off · esc back"
	case apFolders:
		pills = [][2]string{{"a", "Add"}, {"x", "Remove"}}
		hint = "esc back"
	case apLimits:
		hint = "←→ change · esc back"
	case apMemory:
		pills = [][2]string{{"⏎", "Read"}, {"f", "Forget"}, {"u", "Undo today"}}
		hint = "esc back"
	default:
		hint = "↑↓ scroll · esc back"
	}
	x := d.x
	for _, p := range pills {
		w := strWidth(p[0]) + strWidth(p[1]) + 3
		if x+w > d.x+d.w {
			break
		}
		act := "ap:key:" + p[0]
		if p[0] == "⏎" {
			act = "ap:key:" + kEnter
		}
		e := c.put(x, y, " "+p[0]+" ", fgb(th.fg).on(th.element).with(aMonoRev), act)
		e = c.put(e, y, p[1]+" ", fgc(th.dim).on(th.element), act)
		x = e + 2
	}
	if hx := d.x + d.w - strWidth(hint); hx > x+1 {
		c.put(hx, y, hint, fgc(th.faint), "")
	}
}

func (m *Model) drawPanelMain(d *apDraw) {
	th := m.st.th
	c := d.c
	p := &m.ag.panel
	h := m.ag.home
	cfg := h.cfg
	a, _ := m.myAgent()
	// State
	x := d.row("State", false, "")
	state, col := "asleep", th.faint
	switch {
	case cfg.Paused && h.ok:
		state, col = "paused", th.warn
	case a.Online:
		state, col = "awake", th.ok
	}
	if p.statusOK && p.status.State == "working" {
		state, col = "working", th.ok
	}
	e := c.put(x, d.y, "● ", fgc(col), "")
	e = c.put(e, d.y, state, fgb(col), "")
	var bits []string
	if p.statusOK && p.status.LastRun > 0 && state == "awake" {
		bits = append(bits, "idle "+agoText(m.now().Sub(time.UnixMilli(p.status.LastRun))))
	}
	if h.ok {
		bits = append(bits, "sleeps after "+idleText(cfg.IdleMinutes)+" idle")
	}
	switch {
	case h.here:
		bits = append(bits, "on this computer")
	case h.ok && cfg.HomeMachine != "":
		bits = append(bits, "on another computer")
	}
	if len(bits) > 0 {
		c.put(e+2, d.y, truncate(strings.Join(bits, " · "), d.x+d.w-e-2), fgc(th.dim), "")
	}
	d.y++
	if !h.ok || !h.here {
		msg := "it lives on another computer · its settings change there (tc on that computer)"
		if h.err != "" {
			msg = "! " + h.err
		} else if h.dir == "" || !m.agentHomeExists() {
			msg = "no agent home here · tc agent setup on the computer it should live on"
		}
		c.put(d.x, d.y+1, truncate(msg, d.w), fgc(th.warn), "")
		d.y += 3
	} else {
		// Spend today
		x = d.row("Spend today", false, "")
		e = c.put(x, d.y, fmt.Sprintf("$%.2f", p.spent), fgb(th.fg), "")
		e = c.put(e+1, d.y, fmt.Sprintf("of $%g", cfg.DailyCapUSD), fgc(th.dim), "")
		pct := 0
		if cfg.DailyCapUSD > 0 {
			pct = int(min(p.spent/cfg.DailyCapUSD, 1) * 100)
		}
		bw := min(16, d.x+d.w-e-8)
		if bw >= 4 {
			fill := pct * bw / 100
			e = c.put(e+2, d.y, strings.Repeat("█", fill), fgc(th.accent), "")
			e = c.put(e, d.y, strings.Repeat("░", bw-fill), fgc(th.faint), "")
			c.put(e+2, d.y, strconv.Itoa(pct)+"%", fgc(th.dim), "")
		}
		d.y++
		// Asks today
		x = d.row("Asks today", false, "")
		total := 0
		var who []string
		for _, ak := range p.asks {
			total += ak.n
			who = append(who, ak.name+" "+strconv.Itoa(ak.n))
		}
		e = c.put(x, d.y, strconv.Itoa(total), fgb(th.fg), "")
		if len(who) > 0 {
			c.put(e+3, d.y, truncate(strings.Join(who, " · "), d.x+d.w-e-3), fgc(th.dim), "")
		}
		d.y += 2
	}
	d.rule()
	d.y++
	// Mode
	focus := p.row
	st := fgc(th.faint)
	if focus == 0 {
		st = fgb(th.accent)
		c.put(d.x-2, d.y, "›", fgc(th.accent), "")
	}
	c.put(d.x, d.y, "Mode", st, "")
	d.y++
	modes := []struct {
		id         home.Mode
		name, what string
	}{
		{home.ModeTalk, "Talk", "answers only, no shell or edits"},
		{home.ModeWork, "Work", "your asks can run shell and edit in granted folders"},
		{home.ModeCrew, "Crew", "anyone's asks can work in granted folders"},
	}
	for i, md := range modes {
		if d.y >= d.bot {
			break
		}
		on := cfg.Mode == md.id
		if on && focus == 0 {
			d.focusY = d.y + 1 // the radio and the row under it
		}
		act := "ap:mode:" + strconv.Itoa(i)
		if on {
			c.fill(d.x-1, d.y, d.w+1, 1, th.element, act)
		}
		c.tag(d.x-1, d.y, d.w+1, 1, act, !on, true)
		dot, dc := "○", th.faint
		if on {
			dot, dc = "\u25c9", th.accent
		}
		e := c.put(d.x, d.y, dot, fgc(dc), act)
		nst := fgc(th.dim)
		if on {
			nst = fgb(th.fg)
		}
		e = c.put(e+1, d.y, md.name, nst, act)
		c.put(d.x+8, d.y, truncate(md.what, d.w-8), fgc(th.faint), act)
		d.y++
	}
	if d.y < d.bot {
		hp := "~/.config/tc/agent"
		if h.dir != "" {
			hp = tildePath(h.dir)
		}
		note := "PRs, pushes and anything outside " + hp + " ask you first."
		switch cfg.Approvals {
		case home.ApproveAlways:
			note = "Posts outside the thread, PRs, pushes and anything outside " + hp + " ask you first."
		case home.ApproveNever:
			note = "Nothing asks you first."
		}
		c.put(d.x, d.y, truncate(note, d.w), fgc(th.faint).with(aItalic), "")
	}
	d.y += 2
	d.rule()
	d.y++
	// the setting rows
	vals := m.panelValues()
	for i, name := range apRows[1:] {
		if d.y >= d.bot {
			break
		}
		row := i + 1
		act := "ap:row:" + strconv.Itoa(row)
		x := d.row(name, focus == row, act)
		v := vals[name]
		hint := map[string]string{"Approvals": "←/→ change", "Reads": "⏎ edit", "Folders": "⏎ edit", "Limits": "⏎ edit",
			"Memory": "⏎ browse · forget · undo today", "Activity": "⏎ full log", "Personality": ""}[name]
		vst := fgc(th.fg)
		if name == "Personality" {
			vst = fgc(th.dim).with(aItalic)
		}
		room := d.x + d.w - x
		if hint != "" && room-strWidth(v)-3 >= strWidth(hint) {
			e := c.put(x, d.y, v, vst, act)
			c.put(e+3, d.y, hint, fgc(th.faint), act)
		} else {
			c.put(x, d.y, truncate(v, room), vst, act)
		}
		d.y++
	}
}

// drawPanelScrolled draws a page offscreen at full length, then shows the
// part that keeps its focused row in view: a short terminal still reaches
// every row.
func (m *Model) drawPanelScrolled(c *canvas, d *apDraw, in rect) {
	th := m.st.th
	p := &m.ag.panel
	avail := d.bot - d.y - 1 // a blank row above the keys
	if avail < 1 {
		return
	}
	full := max(200, len(p.notes)+len(m.chans)+40)
	sc := newCanvas(in.x1-in.x0, full, th)
	sc.fill(0, 0, sc.w, sc.h, th.panel, "")
	sd := &apDraw{m: m, c: sc, x: d.x - in.x0, w: d.w, y: 0, bot: full, labelW: d.labelW, focusY: -1}
	switch p.page {
	case apSetup:
		m.drawSetup(sd)
	case apMain:
		m.drawPanelMain(sd)
	case apReads:
		m.drawReads(sd)
	case apFolders:
		m.drawFolders(sd)
	case apLimits:
		m.drawLimits(sd)
	case apMemory:
		m.drawMemory(sd)
	}
	end := sd.y
	off := 0
	if sd.focusY >= 0 {
		off = min(max(sd.focusY-avail+2, 0), max(end-avail, 0))
	}
	for y := 0; y < avail && off+y < sc.h; y++ {
		for x := 0; x < sc.w; x++ {
			src := sc.at(x, off+y)
			if !c.in(in.x0+x, d.y+y) {
				continue
			}
			dst := c.at(in.x0+x, d.y+y)
			*dst = *src
			dst.act = 0
			if a := sc.acts[src.act]; src.act != 0 && a != "" {
				dst.act = c.act(a)
			}
		}
	}
	if off > 0 {
		c.put(d.x+d.w-1, d.y, "↑", fgc(th.faint), "")
	}
	if end-off > avail {
		c.put(d.x+d.w-1, d.y+avail-1, "↓", fgc(th.faint), "")
	}
}

// panelValues is each setting row's one-line value.
func (m *Model) panelValues() map[string]string {
	p := &m.ag.panel
	h := m.ag.home
	cfg := h.cfg
	v := map[string]string{"Personality": "tell me in our DM", "Approvals": "Only for code & PRs"}
	for _, a := range apApprovals {
		if a.id == cfg.Approvals {
			v["Approvals"] = a.name
		}
	}
	if cfg.Approvals == home.ApproveNever {
		name := "it"
		if a, ok := m.myAgent(); ok {
			name = agentName(a.Handle)
		}
		v["Approvals"] = "Never · " + name + " acts without asking"
	}
	var reads []string
	if p.accessOK {
		for _, ch := range p.access {
			if cv, ok := m.chanView(ch); ok && cv.Name != "" {
				reads = append(reads, "#"+sanitize.Line(cv.Name))
			}
		}
	}
	reads = append(reads, "+ wherever it is called")
	if cfg.Access.DMs {
		reads = append(reads, "+ your DMs when you ask")
	}
	v["Reads"] = strings.Join(reads, " ")
	if !h.ok {
		for _, k := range []string{"Folders", "Limits", "Memory", "Activity"} {
			v[k] = "—"
		}
		return v
	}
	if len(cfg.Access.Folders) == 0 {
		v["Folders"] = "none yet (Work and Crew only)"
	} else {
		var fs []string
		for _, f := range cfg.Access.Folders {
			fs = append(fs, tildePath(f))
		}
		v["Folders"] = strings.Join(fs, " ") + "  (Work and Crew only)"
	}
	v["Limits"] = fmt.Sprintf("$%g/day · %d asks/person · roll over at %d%%", cfg.DailyCapUSD, cfg.AsksPerPerson, cfg.RolloverPct)
	switch {
	case p.memErr != "":
		v["Memory"] = "can't read: " + p.memErr
	default:
		n := 0
		for _, nt := range p.notes {
			if nt.Kind != "index" && nt.Kind != "daily" && nt.Kind != "handoff" {
				n++
			}
		}
		v["Memory"] = strconv.Itoa(n) + " " + plural(n, "note", "notes")
	}
	if len(p.activity) == 0 {
		v["Activity"] = "nothing yet"
	} else {
		v["Activity"] = activityLine(p.activity[0])
	}
	return v
}

// activityLine is one audit entry in a line: "21:07 priya #general $0.06".
func activityLine(a agent.Activity) string {
	parts := []string{time.UnixMilli(a.T).Local().Format("15:04")}
	for _, s := range []string{a.Who, a.Where} {
		if s = sanitize.Line(s); s != "" {
			parts = append(parts, s)
		}
	}
	if a.What != "ask" && a.What != "reply" {
		parts = append(parts, sanitize.Line(a.What))
	}
	if a.USD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", a.USD))
	}
	return strings.Join(parts, " ")
}

// agentHomeExists: the agent home folder is on this machine.
func (m *Model) agentHomeExists() bool {
	if m.ag.home.dir == "" {
		return false
	}
	_, err := os.Stat(m.ag.home.dir)
	return err == nil
}

// tildePath shortens a path under the home folder to ~/….
func tildePath(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		if rest, ok := strings.CutPrefix(p, h); ok && (rest == "" || rest[0] == '/') {
			return "~" + rest
		}
	}
	return sanitize.Line(p)
}

// agoText is "4m", "2h", "3d".
func agoText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d"
}

func (m *Model) drawReads(d *apDraw) {
	th := m.st.th
	c := d.c
	p := &m.ag.panel
	c.put(d.x, d.y, truncate("Channels it may read for anyone's asks. It always reads where it is called.", d.w), fgc(th.faint), "")
	d.y += 2
	chans := m.readsChans()
	rows := len(chans) + 1
	start := scrollStart(p.row, rows, d.left())
	for i := start; i < rows && d.y < d.bot; i++ {
		act := "ap:row:" + strconv.Itoa(i)
		on, label := false, ""
		if i == 0 {
			on, label = m.ag.home.cfg.Access.DMs, "your DMs, when you ask"
		} else {
			on = slices.Contains(p.access, chans[i-1])
			v, _ := m.chanView(chans[i-1])
			label = m.chanTitle(v.ID)
		}
		x := d.row("", p.row == i, act)
		box, bc := "[ ]", th.faint
		if on {
			box, bc = "[✓]", th.accent
		}
		e := c.put(d.x, d.y, box, fgc(bc), act)
		_ = x
		c.put(e+1, d.y, truncate(label, d.w-5), fgc(th.fg), act)
		d.y++
		if i == 0 {
			d.y++
		}
	}
	if !p.accessOK {
		c.put(d.x, d.bot-1, "loading…", fgc(th.faint), "")
	}
}

// scrollStart is the first of n rows to show so sel fits in room.
func scrollStart(sel, n, room int) int {
	if room <= 0 || n <= room {
		return 0
	}
	return min(max(sel-room+1, 0), n-room)
}

func (m *Model) drawFolders(d *apDraw) {
	th := m.st.th
	c := d.c
	p := &m.ag.panel
	c.put(d.x, d.y, truncate("Folders it may edit in Work and Crew mode, besides its own home.", d.w), fgc(th.faint), "")
	d.y += 2
	fs := m.ag.home.cfg.Access.Folders
	if len(fs) == 0 && !p.editing {
		c.put(d.x, d.y, "none yet · a adds one", fgc(th.dim), "")
		d.y++
	}
	for i, f := range fs {
		if d.y >= d.bot {
			break
		}
		x := d.row("", p.row == i && !p.editing, "ap:row:"+strconv.Itoa(i))
		_ = x
		c.put(d.x, d.y, truncate(tildePath(f), d.w), fgc(th.fg), "ap:row:"+strconv.Itoa(i))
		d.y++
	}
	if p.editing {
		d.y++
		c.put(d.x, d.y, "add ", fgc(th.dim), "")
		c.putANSI(d.x+4, d.y, dropBidi(m.line.View()), d.w-4, fgc(th.fg), "")
	}
}

func (m *Model) drawLimits(d *apDraw) {
	th := m.st.th
	c := d.c
	p := &m.ag.panel
	for i, ls := range limitSteps {
		if d.y >= d.bot {
			break
		}
		act := "ap:row:" + strconv.Itoa(i)
		x := d.row(ls.label, p.row == i, act)
		v := ls.shown(ls.get(m.ag.home.cfg))
		e := c.put(x, d.y, "‹ ", fgc(th.faint), "ap:key:h")
		e = c.put(e, d.y, v, fgb(th.fg), act)
		c.put(e, d.y, " ›", fgc(th.faint), "ap:key:l")
		d.y += 2
	}
	c.put(d.x, d.y, truncate("At the cap it says it's out of juice for today and runs nothing.", d.w), fgc(th.faint), "")
}

func (m *Model) drawMemory(d *apDraw) {
	th := m.st.th
	c := d.c
	p := &m.ag.panel
	if p.memErr != "" {
		c.put(d.x, d.y, truncate("! "+p.memErr, d.w), fgc(th.err), "")
		return
	}
	if len(p.notes) == 0 {
		c.put(d.x, d.y, "nothing remembered yet", fgc(th.dim), "")
		return
	}
	start := scrollStart(p.row, len(p.notes), d.left())
	for i := start; i < len(p.notes) && d.y < d.bot; i++ {
		n := p.notes[i]
		act := "ap:row:" + strconv.Itoa(i)
		d.row("", p.row == i, act)
		kind := string(n.Kind)
		e := c.put(d.x, d.y, fit(kind, 8), fgc(th.faint), act)
		name := sanitize.Line(n.Path)
		e = c.put(e+1, d.y, truncate(name, 28), fgc(th.fg), act)
		if n.Private {
			e = c.put(e+1, d.y, "private", fgc(th.info), act)
		}
		if s := sanitize.Line(n.Summary); s != "" && d.x+d.w-e-2 > 6 {
			c.put(e+2, d.y, truncate(s, d.x+d.w-e-2), fgc(th.dim), act)
		}
		d.y++
	}
}

func (m *Model) drawNote(d *apDraw) {
	th := m.st.th
	c := d.c
	p := &m.ag.panel
	n := p.note
	head := sanitize.Line(n.Path)
	if n.Source != "" {
		head += " · from " + sanitize.Line(n.Source)
	}
	if !n.Modified.IsZero() {
		head += " · " + n.Modified.Local().Format("2 Jan 15:04")
	}
	c.put(d.x, d.y, truncate(head, d.w), fgc(th.faint), "")
	d.y += 2
	var lines []string
	for _, ln := range strings.Split(sanitize.Text(n.Body), "\n") {
		for _, l := range wrapSpans([]span{{text: ln}}, d.w) {
			s := ""
			for _, sp := range l {
				s += sp.text
			}
			lines = append(lines, s)
		}
	}
	p.scroll = min(p.scroll, max(len(lines)-d.left(), 0))
	for _, l := range lines[min(p.scroll, len(lines)):] {
		if d.y >= d.bot {
			break
		}
		c.put(d.x, d.y, l, fgc(th.fg), "")
		d.y++
	}
}

func (m *Model) drawActivity(d *apDraw) {
	th := m.st.th
	c := d.c
	p := &m.ag.panel
	if len(p.activity) == 0 {
		c.put(d.x, d.y, "nothing yet", fgc(th.dim), "")
		return
	}
	p.scroll = min(p.scroll, max(len(p.activity)-d.left(), 0))
	for _, a := range p.activity[p.scroll:] {
		if d.y >= d.bot {
			break
		}
		t := time.UnixMilli(a.T).Local()
		ts := t.Format("15:04")
		if !sameDay(a.T, m.now().UnixMilli()) {
			ts = t.Format("2 Jan")
		}
		e := c.put(d.x, d.y, fit(ts, 6), fgc(th.faint), "")
		whatCol := th.dim
		switch a.What {
		case "error", "refused":
			whatCol = th.err
		case "pr", "issue", "approval":
			whatCol = th.info
		}
		e = c.put(e+1, d.y, fit(sanitize.Line(a.What), 9), fgc(whatCol), "")
		rest := strings.TrimSpace(strings.Join([]string{sanitize.Line(a.Who), sanitize.Line(a.Where), sanitize.Line(a.Detail)}, " "))
		if a.USD > 0 {
			rest += fmt.Sprintf("  $%.2f", a.USD)
		}
		c.put(e+1, d.y, truncate(rest, d.x+d.w-e-1), fgc(th.fg), "")
		d.y++
	}
}

func (m *Model) drawSetup(d *apDraw) {
	th := m.st.th
	c := d.c
	p := &m.ag.panel
	for _, l := range []string{
		"A teammate that lives on this computer and answers when someone @-mentions it.",
		"It speaks first in your private chat with it, to learn how you like it.",
	} {
		c.put(d.x, d.y, truncate(l, d.w), fgc(th.dim), "")
		d.y++
	}
	d.y++
	// handle
	x := d.row("Handle", p.row == 0, "ap:handle")
	c.fill(x, d.y, min(24, d.x+d.w-x), 1, th.element, "ap:handle")
	c.put(x, d.y, "@", fgc(th.faint), "ap:handle")
	if p.editing {
		c.putANSI(x+1, d.y, dropBidi(m.line.View()), min(23, d.x+d.w-x-1), fgc(th.fg).on(th.element), "ap:handle")
	} else {
		v := m.line.Value()
		st := fgc(th.fg).on(th.element)
		if v == "" {
			v, st = "rocky", fgc(th.faint).on(th.element)
		}
		c.put(x+1, d.y, truncate(sanitize.Line(v), 22), st, "ap:handle")
	}
	d.y += 2
	// icon
	x = d.row("Icon", p.row == 1, "")
	col := th.nicks[0]
	if p.color >= 0 {
		col = th.nicks[p.color]
	} else {
		col = m.agentColor(proto.Agent{Handle: m.setupHandle()})
	}
	xx := x
	for i, pr := range agentPresets {
		cell := pr.idle + " " + pr.id
		w := strWidth(cell) + 2
		if xx+w > d.x+d.w {
			d.y++
			xx = x
		}
		act := "ap:icon:" + strconv.Itoa(i)
		st := fgc(th.dim)
		gc := th.faint
		if i == p.icon {
			c.fill(xx, d.y, w, 1, th.element, act)
			st, gc = fgb(th.fg).on(th.element), col
		}
		e := c.put(xx+1, d.y, pr.idle, fgc(gc), act)
		c.put(e+1, d.y, pr.id, st, act)
		xx += w + 1
	}
	d.y += 2
	// color
	x = d.row("Color", p.row == 2, "")
	xx = c.put(x, d.y, " auto ", map[bool]cstyle{true: fgb(th.fg).on(th.element), false: fgc(th.dim)}[p.color < 0], "ap:color:-1") + 1
	for i := 0; i < 8; i++ {
		g := " ● "
		st := fgc(th.nicks[i])
		if i == p.color {
			g, st = "[●]", fgb(th.nicks[i]).on(th.element)
		}
		xx = c.put(xx, d.y, g, st, "ap:color:"+strconv.Itoa(i))
	}
	d.y += 2
	// preview of its name row
	h := m.setupHandle()
	pv := proto.Agent{Handle: h, Icon: agentIcon(p.icon, p.color), Owner: m.me.ID}
	g, gcol := m.agentGlyph(pv, "", time.Time{}, th.panel)
	e := c.put(d.x, d.y, g, fgc(gcol), "")
	e = c.put(e+1, d.y, agentName(h), fgb(m.agentColor(pv)), "")
	e = c.put(e+1, d.y, agentBadge, fgc(m.agentColor(pv)), "")
	c.put(e, d.y, truncate(" · "+m.ownerName(m.me.ID)+"'s agent", d.x+d.w-e), fgc(th.faint), "")
	d.y += 2
	// the button
	label := " Set up " + agentName(h) + " "
	if p.busy {
		label = " setting up… "
	}
	st := fgc(th.info).on(th.element)
	if p.row == 3 {
		st = fgb(th.accentFg).on(th.accent).with(aMonoRev)
		c.put(d.x-2, d.y, "›", fgc(th.accent), "")
		d.focusY = d.y + 2 // and the shell hint under it
	}
	c.pill(d.x, d.y, label, st, "ap:setup")
	d.y += 2
	c.put(d.x, d.y, truncate("or from a shell: tc agent setup", d.w), fgc(th.faint), "")
}
