package tui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/alert"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Notification policy (docs/research/NOTIFICATIONS-2026-10.md §2): per
// channel levels, DND, quiet hours, content privacy and sound switches
// decide whether an event pops up and with which sound. The trigger rules
// (bots, trust, focus) stay in eligible.

// NotifyBackend is the part of client.Client the notification settings
// use; older fakes without it get the defaults.
type NotifyBackend interface {
	NotifyPrefs() client.NotifyPrefs
	SetChanNotify(ch uint64, lvl client.NotifyLevel, until int64) error
	SetNotifySetting(key, value string) error
	InMyThread(m proto.Message) bool
}

var _ NotifyBackend = (*client.Client)(nil)

// NotifyEnv are the env overrides (TC_SOUND, TC_NOTIFY_CONTENT, TC_QUIET):
// they win over stored settings, for scripts and SSH.
type NotifyEnv = alert.Env

type noteKind = alert.Kind

const (
	noteMessage = alert.Message // any message (level all)
	noteMention = alert.Mention // DM, mention, highlight, reminder, reply in my thread
	noteRing    = alert.Ring    // a DM huddle started
	noteHuddle  = alert.Huddle  // a channel huddle started
)

const (
	allWindow     = alert.AllWindow
	ringCooldown  = alert.RingCooldown
	ringRepeat    = alert.RingRepeat
	huddleNoteGap = alert.HuddleNoteGap
)

// decide is the shared policy (internal/alert).
func decide(p client.NotifyPrefs, now time.Time, ch uint64, kind noteKind) (bool, notify.Cue) {
	return alert.Decide(p, now, ch, kind)
}

type ringMsg struct {
	gen    int
	resend bool // else: time's up
}

// notifyUI is the model's notification state beyond the shared engine
// (m.al: coalescing, caps, rings).
type notifyUI struct {
	answering int // the ring generation /huddle was sent for
	prefsGen  int
	prefsAt   time.Time // when the scheduled re-render tick fires
	testQueue []notify.Cue
}

// tuiWorld answers the engine from the model's loaded state.
type tuiWorld struct{ m *Model }

func (w tuiWorld) Me() uint64                                    { return w.m.me.ID }
func (w tuiWorld) Chan(id uint64) (client.ChannelView, bool)     { return w.m.chanView(id) }
func (w tuiWorld) User(id uint64) proto.User                     { return w.m.users[id] }
func (w tuiWorld) MentionNames() []string                        { return w.m.mentionAs }
func (w tuiWorld) Highlights() []string                          { return w.m.highlights }
func (w tuiWorld) Current(x proto.Message) (proto.Message, bool) { return w.m.currentMessage(x) }
func (w tuiWorld) Looking(ch uint64) bool                        { return ch == w.m.active && w.m.focused }
func (w tuiWorld) InHuddle(ch uint64) bool                       { return w.m.hud.in && w.m.hud.cur == ch }

func (w tuiWorld) InMyThread(x proto.Message) bool {
	nb, ok := w.m.notifyBackend()
	return ok && nb.InMyThread(x)
}

func (w tuiWorld) Roster(ch uint64) ([]uint64, bool) {
	hb, ok := w.m.huddleBackend()
	if !ok {
		return nil, false
	}
	return hb.Huddle(ch), true
}

// tuiSender sends through whatever Notifier the options hold now.
type tuiSender struct{ m *Model }

func (s tuiSender) Send(n notify.Note) bool {
	if s.m.opt.Notifier == nil {
		return false
	}
	return s.m.opt.Notifier.Send(n)
}

// alertCmd turns the engine's timers into ticks.
func alertCmd(ts []alert.Timer) tea.Cmd {
	var cmds []tea.Cmd
	for _, t := range ts {
		ch, gen := t.Ch, t.Gen
		switch t.Kind {
		case alert.FlushTimer:
			cmds = append(cmds, tea.Tick(t.After, func(time.Time) tea.Msg { return notifyFlushMsg{ch, gen} }))
		case alert.RingResendT:
			cmds = append(cmds, tea.Tick(t.After, func(time.Time) tea.Msg { return ringMsg{gen: gen, resend: true} }))
		case alert.RingEndT:
			cmds = append(cmds, tea.Tick(t.After, func(time.Time) tea.Msg { return ringMsg{gen: gen} }))
		}
	}
	return tea.Batch(cmds...)
}

func (m *Model) notifyBackend() (NotifyBackend, bool) {
	nb, ok := m.c.(NotifyBackend)
	return nb, ok
}

// refreshPrefs re-reads the stored settings and applies env overrides.
func (m *Model) refreshPrefs() {
	p := alert.DefaultPrefs()
	if nb, ok := m.notifyBackend(); ok {
		p = nb.NotifyPrefs()
	}
	m.al.SetPrefs(m.opt.NotifyEnv.Apply(p))
	if m.al.Ring.Ch == 0 {
		m.nt.answering = 0
	}
}

type prefsTickMsg struct{ gen int }

// schedulePrefsTick re-renders once a minute while something time-bound is
// showing (DND, a timed mute, quiet hours), so 🔕 marks and dimming don't
// go stale in an idle pane.
func (m *Model) schedulePrefsTick() tea.Cmd {
	p, now := m.al.Prefs, m.now()
	timed := p.DNDUntil > 0 || p.Quiet != ""
	for _, c := range p.Chans {
		timed = timed || c.Until > 0
	}
	if !timed || now.Before(m.nt.prefsAt) {
		return nil
	}
	m.nt.prefsGen++
	m.nt.prefsAt = now.Add(time.Minute)
	gen := m.nt.prefsGen
	return tea.Tick(time.Minute, func(time.Time) tea.Msg { return prefsTickMsg{gen} })
}

func (m *Model) prefsTick(msg prefsTickMsg) tea.Cmd {
	if msg.gen == m.nt.prefsGen {
		m.nt.prefsAt = time.Time{} // Update schedules the next one if still needed
	}
	return nil
}

// untilLabel is a time for "until …": 15:04 today, Mon 15:04 this week,
// Jan 2 15:04 beyond.
func (m *Model) untilLabel(ms int64) string {
	t, now := time.UnixMilli(ms), m.now().Local()
	switch {
	case t.Year() == now.Year() && t.YearDay() == now.YearDay():
		return t.Format("15:04")
	case t.Sub(now) < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	}
	return t.Format("Jan 2 15:04")
}

// muted reports whether ch is at level off now (sidebar dims it).
func (m *Model) muted(ch uint64) bool { return m.al.Prefs.Muted(ch, m.now()) }

// eligible is the trigger rule every notification passes first: never
// bots, my own messages (but my reminders), or the channel I'm looking at.
func (m *Model) eligible(x proto.Message) bool { return m.al.Eligible(x) }

// mentionClass: DMs, @mentions, highlights, my reminders, replies in my
// threads.
func (m *Model) mentionClass(x proto.Message) bool { return m.al.MentionClass(x) }

// ---- huddle starts: rings and popups ----

func (m *Model) huddleStarted(starts []client.HuddleStart) tea.Cmd {
	return alertCmd(m.al.HuddleStarts(starts))
}

func (m *Model) sendNote(n notify.Note) {
	if m.opt.Notifier != nil {
		m.opt.Notifier.Send(n)
	}
}

func (m *Model) ringMsg(msg ringMsg) tea.Cmd {
	m.al.RingTimer(msg.gen, msg.resend)
	if m.al.Ring.Ch == 0 {
		m.nt.answering = 0
	}
	return nil
}

// stopRing ends the ring; cool keeps the same DM from ringing again soon
// (dismissed or unanswered — a join/leave flapper can't spam).
func (m *Model) stopRing(cool bool) {
	m.al.StopRing(cool)
	m.nt.answering = 0
}

// checkRing stops the ring once it's answered (we joined, from any
// device), over (the huddle ended), or seen (the DM is open and focused).
func (m *Model) checkRing() {
	m.al.CheckRing()
	if m.al.Ring.Ch == 0 {
		m.nt.answering = 0
	}
}

// plainJoinHint replaces "/huddle to join" in builds without voice, where
// /huddle can only fail (#85).
const plainJoinHint = alert.PlainJoinHint

// ringLine is the status-line prompt while ringing; compact (name cut,
// just the command) when the line is narrow.
func (m *Model) ringLine(compact bool) string {
	r := m.al.Ring
	if r.Ch == 0 {
		return ""
	}
	who := m.r.userName(r.By)
	if !client.VoiceAvailable() {
		if compact {
			return "📞 " + truncate(who, 12) + " · tc update --voice"
		}
		return "📞 " + who + " started a huddle · " + plainJoinHint + " · Esc dismiss"
	}
	if compact {
		return "📞 " + truncate(who, 12) + " /huddle"
	}
	return "📞 " + who + " — /huddle to join · Esc dismiss"
}

// quietMark is the status line's DND / quiet-hours mark.
func (m *Model) quietMark() string {
	now := m.now()
	p := m.al.Prefs
	switch {
	case p.DND(now):
		return "🔕 dnd " + m.untilLabel(p.DNDUntil)
	case p.InQuiet(now):
		return "🔕 quiet"
	}
	return ""
}

// muteMark is the header's mark for a muted open channel.
func (m *Model) muteMark(ch uint64) string {
	p := m.al.Prefs
	lvl, over := p.Level(ch, m.now())
	if lvl != client.LevelOff {
		return ""
	}
	if c := p.Chans[ch]; over && c.Until > 0 {
		return " 🔕 until " + m.untilLabel(c.Until)
	}
	return " 🔕"
}

// ---- commands: /notify, /dnd, /sounds ----

type notifyTestMsg struct{}

const notifyTestGap = 2500 * time.Millisecond

func (m *Model) notifyCommand(c command) (tea.Cmd, bool, error) {
	switch c.name {
	case "notify", "dnd", "sounds":
	default:
		return nil, false, nil
	}
	nb, ok := m.notifyBackend()
	if !ok {
		return nil, true, errors.New("notification settings aren't available")
	}
	var err error
	var cmd tea.Cmd
	switch c.name {
	case "notify":
		cmd, err = m.notifyCmd(nb, c.args)
	case "dnd":
		err = m.dndCmd(nb, c.args)
	case "sounds":
		err = m.soundsCmd(nb, c.args)
	}
	if err == nil {
		m.refreshPrefs()
	}
	return cmd, true, err
}

// parseFor reads an optional "[for] 2h" / "until HH:MM" tail into a unix
// ms expiry (0: none).
func (m *Model) parseFor(args []string) (int64, error) {
	if len(args) > 0 && (args[0] == "for" || args[0] == "until") {
		if len(args) != 2 {
			return 0, errors.New(args[0] + " what? (for 2h, until 09:00)")
		}
		if args[0] == "until" {
			return m.untilClock(args[1])
		}
		args = args[1:]
	}
	switch len(args) {
	case 0:
		return 0, nil
	case 1:
		d, err := client.ParseTTL(args[0])
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("bad duration %q (like 30m, 2h, 1d)", sanitize.Line(args[0]))
		}
		return m.now().Add(d).UnixMilli(), nil
	}
	return 0, errors.New("too many arguments")
}

// untilClock is the next local HH:MM after now.
func (m *Model) untilClock(s string) (int64, error) {
	hh, mm, ok := client.ParseClock(s)
	if !ok {
		return 0, fmt.Errorf("bad time %q (like 09:00 or 9am)", sanitize.Line(s))
	}
	now := m.now().Local() // the user's clock
	// Pick the day by wall-clock minutes, not by comparing instants: in a
	// repeated DST hour time.Date resolves to the first occurrence.
	day := now.Day()
	if hh*60+mm <= now.Hour()*60+now.Minute() {
		day++
	}
	t := time.Date(now.Year(), now.Month(), day, hh, mm, 0, 0, now.Location())
	for !t.After(now) { // the ambiguous hour, second time round
		t = t.Add(time.Hour)
	}
	return t.UnixMilli(), nil
}

func (m *Model) notifyCmd(nb NotifyBackend, args []string) (tea.Cmd, error) {
	if len(args) == 0 {
		m.showList("notifications — this device", m.notifySummary())
		return nil, nil
	}
	switch strings.ToLower(args[0]) {
	case "test":
		if len(m.nt.testQueue) > 0 {
			return nil, errors.New("a test is already running")
		}
		m.nt.testQueue = []notify.Cue{notify.CueMention, notify.CueMessage, notify.CueRing, notify.CueHuddle}
		m.setInfo("sending 4 test notifications (mention, message, ring, huddle) — " + m.backendName())
		return m.notifyTestNext(), nil
	case "global":
		if len(args) != 2 {
			return nil, errors.New("/notify global all|mentions|off")
		}
		lvl, err := client.ParseLevel(args[1])
		if err != nil {
			return nil, err
		}
		if err := nb.SetNotifySetting("notify.default", string(lvl)); err != nil {
			return nil, err
		}
		m.setInfo("default notification level: " + string(lvl) + " (this device)")
		return nil, nil
	case "content":
		if len(args) != 2 {
			return nil, errors.New("/notify content full|name|none")
		}
		v := strings.ToLower(args[1])
		if v != "full" && v != "name" && v != "none" {
			return nil, errors.New("/notify content full|name|none")
		}
		if v == "full" {
			v = ""
		}
		if err := nb.SetNotifySetting("notify.content", v); err != nil {
			return nil, err
		}
		m.setInfo("popups show: " + map[string]string{"": "sender and text", "name": "sender only", "none": "nothing but \"new message\""}[v])
		return nil, nil
	case "quiet":
		if len(args) != 2 {
			return nil, errors.New("/notify quiet HH:MM-HH:MM|off")
		}
		v := args[1]
		if strings.EqualFold(v, "off") {
			v = ""
		} else if _, _, err := client.ParseQuiet(v); err != nil {
			return nil, err
		}
		if err := nb.SetNotifySetting("notify.quiet", v); err != nil {
			return nil, err
		}
		if v == "" {
			m.setInfo("quiet hours off")
		} else {
			m.setInfo("quiet hours " + v + " (local time): no popups or sounds")
		}
		return nil, nil
	}
	// /notify all|mentions|off|default [#chan] [for 2h]
	word := strings.ToLower(args[0])
	lvl := client.LevelDefault
	if word != "default" {
		l, err := client.ParseLevel(word)
		if err != nil {
			return nil, errors.New("usage: /notify all|mentions|off|default [#chan] [for 2h] (more: F1)")
		}
		lvl = l
	}
	rest := args[1:]
	ch := m.active
	if len(rest) > 0 && strings.HasPrefix(rest[0], "#") {
		id, ok := m.findChan(rest[0])
		if !ok {
			return nil, fmt.Errorf("no channel %s", sanitize.Line(rest[0]))
		}
		ch, rest = id, rest[1:]
	}
	if ch == 0 {
		return nil, errors.New("no channel open")
	}
	until, err := m.parseFor(rest)
	if err != nil {
		return nil, err
	}
	if lvl == client.LevelDefault && until != 0 {
		return nil, errors.New("default takes no duration")
	}
	if err := nb.SetChanNotify(ch, lvl, until); err != nil {
		return nil, err
	}
	what := string(lvl)
	if lvl == client.LevelDefault {
		what = "the default"
	}
	msg := m.chanTitle(ch) + " notifies: " + what
	if until != 0 {
		msg += " until " + m.untilLabel(until)
	}
	m.setInfo(msg)
	return nil, nil
}

func (m *Model) backendName() string {
	type named interface {
		Backend() string
		Dialect() string
	}
	if n, ok := m.opt.Notifier.(named); ok {
		if d := n.Dialect(); n.Backend() == notify.Terminal && d != "" {
			return "terminal (" + d + ")"
		}
		return n.Backend()
	}
	return "?"
}

// notifySummary is the /notify settings, one per row: a status-line notice
// would be cut at 80 columns.
func (m *Model) notifySummary() []string {
	p, now := m.al.Prefs, m.now()
	var rows []string
	if m.active != 0 {
		lvl, over := p.Level(m.active, now)
		r := m.chanTitle(m.active) + ": " + string(lvl)
		if !over {
			r += " (default)"
		}
		rows = append(rows, r)
	}
	def := p.Default
	if def == client.LevelDefault {
		def = client.LevelMentions
	}
	content := p.Content
	if content == "" {
		content = "full"
	}
	quiet := p.Quiet
	if quiet == "" {
		quiet = "off"
	}
	dnd := "off"
	if p.DND(now) {
		dnd = "until " + m.untilLabel(p.DNDUntil)
	}
	sounds := "on"
	if p.SoundOff {
		sounds = "off"
	}
	hint := func(h string) string {
		if m.dims().mainW < 50 { // narrow split: the values matter more
			return ""
		}
		return " (" + h + ")"
	}
	rows = append(rows,
		"default: "+string(def)+hint("/notify global LEVEL"),
		"content: "+content+hint("/notify content full|name|none"),
		"quiet hours: "+quiet+hint("/notify quiet 22:00-08:00|off"),
		"do not disturb: "+dnd+hint("/dnd"),
		"via: "+m.backendName()+hint("/notify test"),
		"sounds: "+sounds+hint("/sounds"))
	if m.opt.Background != nil {
		// The one place tc mentions it: popups while this window is closed.
		// It's a shell command, not a slash command: say so.
		if on, known := m.opt.Background(); known && on {
			rows = append(rows, "when tc is closed: on"+hint("shell: tc background off"))
		} else if known {
			rows = append(rows, "when tc is closed: off"+hint("shell: tc background on"))
		}
	}
	return rows
}

// showList opens the read-only list overlay.
func (m *Model) showList(title string, rows []string) {
	m.list = listState{title: title, rows: rows}
	m.mode = modeList
}

func (m *Model) notifyTestNext() tea.Cmd {
	if len(m.nt.testQueue) == 0 {
		return nil
	}
	cue := m.nt.testQueue[0]
	m.nt.testQueue = m.nt.testQueue[1:]
	name := map[notify.Cue]string{notify.CueMention: "mention", notify.CueMessage: "message", notify.CueRing: "ring", notify.CueHuddle: "huddle"}[cue]
	sound := map[notify.Cue]string{notify.CueMention: "mention", notify.CueMessage: "message", notify.CueRing: "ring", notify.CueHuddle: "huddle"}[cue]
	if !m.al.Prefs.Sound(sound) {
		cue = notify.CueNone
	}
	m.sendNote(notify.Note{Title: "tc test: " + name, Body: "this is how a " + name + " notification looks", Cue: cue})
	if len(m.nt.testQueue) == 0 {
		return nil
	}
	return tea.Tick(notifyTestGap, func(time.Time) tea.Msg { return notifyTestMsg{} })
}

func (m *Model) dndCmd(nb NotifyBackend, args []string) error {
	now := m.now()
	var until int64
	switch {
	case len(args) == 0:
		if m.al.Prefs.DND(now) {
			until = 0 // toggle off
		} else {
			until = now.Add(time.Hour).UnixMilli()
		}
	case len(args) == 1 && strings.EqualFold(args[0], "off"):
	default:
		u, err := m.parseFor(args)
		if err != nil {
			return errors.New("/dnd [30m|2h|until 09:00|off]")
		}
		until = u
	}
	v := ""
	if until != 0 {
		v = strconv.FormatInt(until, 10)
	}
	if err := nb.SetNotifySetting("notify.dnd_until", v); err != nil {
		return err
	}
	if until == 0 {
		m.setInfo("do not disturb off")
	} else {
		m.setInfo("do not disturb until " + m.untilLabel(until) + ": no popups, sounds or rings")
	}
	return nil
}

func (m *Model) soundsCmd(nb NotifyBackend, args []string) error {
	usage := errors.New("usage: /sounds [CUE] on|off, /sounds volume N (more: F1)")
	for i := range args {
		args[i] = strings.ToLower(args[i])
	}
	set := func(k, v string) error { return nb.SetNotifySetting(k, v) }
	switch {
	case len(args) == 0:
		p := m.al.Prefs
		master := "on"
		if p.SoundOff {
			master = "off"
		}
		rows := []string{"sounds: " + master + " (/sounds on|off)"}
		for _, c := range client.SoundCues {
			st := "on"
			if !p.Sound(c) {
				st = "off"
			}
			rows = append(rows, c+": "+st)
		}
		rows = append(rows, fmt.Sprintf("huddle cue volume: %d (/sounds volume 0-100)", p.Volume))
		m.showList("sounds — /sounds CUE on|off", rows)
		return nil
	case len(args) == 1 && (args[0] == "on" || args[0] == "off"):
		v := ""
		if args[0] == "off" {
			v = "off"
		}
		if err := set("sound.all", v); err != nil {
			return err
		}
		if args[0] == "off" {
			m.setInfo("sounds off (popups still show)")
		} else {
			m.setInfo("sounds on")
		}
		return nil
	case len(args) == 2 && args[0] == "volume":
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 0 || n > 100 {
			return usage
		}
		if err := set("sound.volume", strconv.Itoa(n)); err != nil {
			return err
		}
		m.setInfo("huddle cue volume " + strconv.Itoa(n))
		return nil
	case len(args) == 2 && (args[1] == "on" || args[1] == "off"):
		cue := strings.ToLower(args[0])
		known := false
		for _, c := range client.SoundCues {
			known = known || c == cue
		}
		if !known {
			return usage
		}
		v := ""
		if args[1] == "off" {
			v = "off"
		}
		if err := set("sound."+cue, v); err != nil {
			return err
		}
		m.setInfo(cue + " sound " + args[1])
		return nil
	}
	return usage
}
