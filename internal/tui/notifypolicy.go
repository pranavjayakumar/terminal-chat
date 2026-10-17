package tui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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
type NotifyEnv struct {
	Sound, Content, Quiet string
}

type noteKind int

const (
	noteMessage noteKind = iota // any message (level all)
	noteMention                 // DM, mention, highlight, reminder, reply in my thread
	noteRing                    // a DM huddle started
	noteHuddle                  // a channel huddle started
)

const (
	allWindow     = 30 * time.Second // coalescing for level-all channels
	ringResend    = 10 * time.Second
	ringFor       = 30 * time.Second
	ringCooldown  = 5 * time.Minute // after a dismissed or unanswered ring
	huddleNoteGap = 5 * time.Minute // channel huddle-start popups per channel
)

// decide is the policy: whether ev pops up in ch at now, and its sound.
func decide(p client.NotifyPrefs, now time.Time, ch uint64, kind noteKind) (bool, notify.Cue) {
	lvl, _ := p.Level(ch, now)
	if lvl == client.LevelOff || p.DND(now) || p.InQuiet(now) {
		return false, notify.CueNone
	}
	var cue notify.Cue
	var sound string
	switch kind {
	case noteMessage:
		if lvl != client.LevelAll {
			return false, notify.CueNone
		}
		cue, sound = notify.CueMessage, "message"
	case noteMention:
		cue, sound = notify.CueMention, "mention"
	case noteRing:
		cue, sound = notify.CueRing, "ring"
	case noteHuddle:
		if lvl != client.LevelAll {
			return false, notify.CueNone
		}
		cue, sound = notify.CueHuddle, "huddle"
	}
	if !p.Sound(sound) {
		cue = notify.CueNone
	}
	return true, cue
}

type ringState struct {
	ch, by uint64
	gen    int
	since  time.Time
}

type ringMsg struct {
	gen    int
	resend bool // else: time's up
}

// notifyUI is the model's notification state beyond the coalescing queue.
type notifyUI struct {
	prefs     client.NotifyPrefs
	ring      ringState
	ringGen   int
	ringCool  map[uint64]time.Time
	huddleAt  map[uint64]time.Time // last channel huddle-start popup
	testQueue []notify.Cue
}

func (m *Model) notifyBackend() (NotifyBackend, bool) {
	nb, ok := m.c.(NotifyBackend)
	return nb, ok
}

// refreshPrefs re-reads the stored settings and applies env overrides.
func (m *Model) refreshPrefs() {
	p := client.NotifyPrefs{Chans: map[uint64]client.ChanNotify{}, CueOff: map[string]bool{}, Volume: client.DefaultCueVolume}
	if nb, ok := m.notifyBackend(); ok {
		p = nb.NotifyPrefs()
	}
	e := m.opt.NotifyEnv
	switch strings.ToLower(e.Sound) {
	case "off", "0", "no":
		p.SoundOff = true
	case "on", "1", "yes":
		p.SoundOff = false
	}
	switch strings.ToLower(e.Content) {
	case "full":
		p.Content = ""
	case "name", "none":
		p.Content = strings.ToLower(e.Content)
	}
	if e.Quiet != "" {
		if strings.EqualFold(e.Quiet, "off") {
			p.Quiet = ""
		} else if _, _, err := client.ParseQuiet(e.Quiet); err == nil {
			p.Quiet = e.Quiet
		}
	}
	m.nt.prefs = p
}

// muted reports whether ch is at level off now (sidebar dims it).
func (m *Model) muted(ch uint64) bool { return m.nt.prefs.Muted(ch, m.now()) }

// eligible is the trigger rule every notification passes first: never
// bots, my own messages (but my reminders), or the channel I'm looking at.
func (m *Model) eligible(x proto.Message) bool {
	if x.Deleted || m.users[x.Author].Bot {
		return false
	}
	if x.Chan == m.active && m.focused {
		return false
	}
	if x.Author == m.me.ID {
		return m.isReminder(x)
	}
	return true
}

// mentionClass: DMs, @mentions, highlights, my reminders, replies in my
// threads.
func (m *Model) mentionClass(x proto.Message) bool {
	if x.Author == m.me.ID {
		return m.isReminder(x)
	}
	if v, ok := m.chanView(x.Chan); ok && v.Kind == proto.ChanDM {
		return true
	}
	if m.concernsMe(x.Body) {
		return true
	}
	nb, ok := m.notifyBackend()
	return ok && x.Thread != 0 && nb.InMyThread(x)
}

// private applies the content setting: name hides the text, none the
// sender and channel too (popups persist in OS notification history).
func (m *Model) private(title, body string, n int) (string, string) {
	switch m.nt.prefs.Content {
	case "name":
		body = "new message"
	case "none":
		title, body = "tc", "new message"
	default:
		return title, body
	}
	if n > 1 {
		body = strconv.Itoa(n) + " new messages"
	}
	return title, body
}

func noteTag(ch uint64) string { return "tc-" + strconv.FormatUint(ch, 10) }

// ---- huddle starts: rings and popups ----

func (m *Model) huddleStarted(starts []client.HuddleStart) tea.Cmd {
	var cmds []tea.Cmd
	now := m.now()
	for _, s := range starts {
		v, ok := m.chanView(s.Chan)
		if !ok || !v.Member || m.users[s.By].Bot {
			continue
		}
		who := m.r.userName(s.By)
		if v.Kind == proto.ChanDM {
			if m.nt.ring.ch != 0 || now.Before(m.nt.ringCool[s.Chan]) || (s.Chan == m.active && m.focused) {
				continue
			}
			if in, ok := m.hud.cur, m.hud.in; ok && in == s.Chan {
				continue
			}
			popup, cue := decide(m.nt.prefs, now, s.Chan, noteRing)
			if !popup {
				continue
			}
			m.nt.ringGen++
			m.nt.ring = ringState{ch: s.Chan, by: s.By, gen: m.nt.ringGen, since: now}
			m.sendRing(cue)
			gen := m.nt.ringGen
			cmds = append(cmds,
				tea.Tick(ringResend, func(time.Time) tea.Msg { return ringMsg{gen: gen, resend: true} }),
				tea.Tick(ringFor, func(time.Time) tea.Msg { return ringMsg{gen: gen} }))
			continue
		}
		if s.Chan == m.active && m.focused {
			continue
		}
		popup, cue := decide(m.nt.prefs, now, s.Chan, noteHuddle)
		if !popup || now.Sub(m.nt.huddleAt[s.Chan]) < huddleNoteGap {
			continue
		}
		if m.nt.huddleAt == nil {
			m.nt.huddleAt = map[uint64]time.Time{}
		}
		m.nt.huddleAt[s.Chan] = now
		title, body := "🎧 "+who+" started a huddle in "+m.chanTitle(s.Chan), "/huddle to join"
		if m.nt.prefs.Content == "none" {
			title, body = "tc", "huddle started"
		}
		m.sendNote(notify.Note{Title: title, Body: body, Cue: cue, Tag: noteTag(s.Chan)})
	}
	return tea.Batch(cmds...)
}

// sendRing shows the ring popup; rings skip the queue and the 2 s cap
// (the Notifier gives them their own slot).
func (m *Model) sendRing(cue notify.Cue) {
	r := m.nt.ring
	title := "📞 " + m.r.userName(r.by) + " started a huddle"
	body := "/huddle to join"
	if !client.VoiceAvailable() {
		body = "join with the voice build (install.sh --voice)"
	}
	if m.nt.prefs.Content == "none" {
		title, body = "tc", "incoming huddle"
	}
	m.sendNote(notify.Note{Title: title, Body: body, Cue: cue, Tag: noteTag(r.ch)})
}

func (m *Model) sendNote(n notify.Note) {
	if m.opt.Notifier != nil {
		m.opt.Notifier.Send(n)
	}
}

func (m *Model) ringMsg(msg ringMsg) tea.Cmd {
	if msg.gen != m.nt.ring.gen || m.nt.ring.ch == 0 {
		return nil
	}
	if msg.resend {
		if popup, cue := decide(m.nt.prefs, m.now(), m.nt.ring.ch, noteRing); popup {
			m.sendRing(cue)
		}
		return nil
	}
	m.stopRing(true)
	return nil
}

// stopRing ends the ring; cool keeps the same DM from ringing again soon
// (dismissed or unanswered — a join/leave flapper can't spam).
func (m *Model) stopRing(cool bool) {
	if m.nt.ring.ch == 0 {
		return
	}
	if cool {
		if m.nt.ringCool == nil {
			m.nt.ringCool = map[uint64]time.Time{}
		}
		m.nt.ringCool[m.nt.ring.ch] = m.now().Add(ringCooldown)
	}
	m.nt.ring = ringState{}
}

// checkRing stops the ring once it's answered (we joined, from any
// device), over (the huddle ended), or seen (the DM is open and focused).
func (m *Model) checkRing() {
	r := m.nt.ring
	if r.ch == 0 {
		return
	}
	if m.hud.in && m.hud.cur == r.ch {
		m.stopRing(false)
		return
	}
	if r.ch == m.active && m.focused {
		m.stopRing(false)
		return
	}
	hb, ok := m.huddleBackend()
	if !ok {
		return
	}
	roster := hb.Huddle(r.ch)
	if len(roster) == 0 {
		m.stopRing(false)
		return
	}
	for _, u := range roster {
		if u == m.me.ID {
			m.stopRing(false)
			return
		}
	}
}

// ringLine is the status-line prompt while ringing.
func (m *Model) ringLine() string {
	r := m.nt.ring
	if r.ch == 0 {
		return ""
	}
	return "📞 " + m.r.userName(r.by) + " — /huddle to join · Esc dismiss"
}

// quietMark is the status line's DND / quiet-hours mark.
func (m *Model) quietMark() string {
	now := m.now()
	p := m.nt.prefs
	switch {
	case p.DND(now):
		return "🔕 " + time.UnixMilli(p.DNDUntil).Format("15:04")
	case p.InQuiet(now):
		return "🔕 quiet"
	}
	return ""
}

// muteMark is the header's mark for a muted open channel.
func (m *Model) muteMark(ch uint64) string {
	p := m.nt.prefs
	lvl, over := p.Level(ch, m.now())
	if lvl != client.LevelOff {
		return ""
	}
	if c := p.Chans[ch]; over && c.Until > 0 {
		return " 🔕 until " + time.UnixMilli(c.Until).Format("15:04")
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
		if args[0] == "until" {
			if len(args) != 2 {
				return 0, errors.New("until HH:MM")
			}
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
	hm, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("bad time %q (HH:MM)", sanitize.Line(s))
	}
	now := m.now().Local() // the user's clock
	t := time.Date(now.Year(), now.Month(), now.Day(), hm.Hour(), hm.Minute(), 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t.UnixMilli(), nil
}

func (m *Model) notifyCmd(nb NotifyBackend, args []string) (tea.Cmd, error) {
	if len(args) == 0 {
		m.setNotice(m.notifySummary(), false, true, 0)
		return nil, nil
	}
	switch strings.ToLower(args[0]) {
	case "test":
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
			return nil, errors.New("/notify all|mentions|off|default [#chan] [for 2h] · global · content · quiet · test")
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
		msg += " until " + time.UnixMilli(until).Format("15:04")
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

func (m *Model) notifySummary() string {
	p, now := m.nt.prefs, m.now()
	var b strings.Builder
	if m.active != 0 {
		lvl, over := p.Level(m.active, now)
		fmt.Fprintf(&b, "%s: %s", m.chanTitle(m.active), lvl)
		if !over {
			b.WriteString(" (default)")
		}
		b.WriteString(" · ")
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
		dnd = "until " + time.UnixMilli(p.DNDUntil).Format("15:04")
	}
	sounds := "on"
	if p.SoundOff {
		sounds = "off"
	}
	fmt.Fprintf(&b, "default %s · content %s · quiet %s · dnd %s · via %s · sounds %s — this device", def, content, quiet, dnd, m.backendName(), sounds)
	return b.String()
}

func (m *Model) notifyTestNext() tea.Cmd {
	if len(m.nt.testQueue) == 0 {
		return nil
	}
	cue := m.nt.testQueue[0]
	m.nt.testQueue = m.nt.testQueue[1:]
	name := map[notify.Cue]string{notify.CueMention: "mention", notify.CueMessage: "message", notify.CueRing: "ring", notify.CueHuddle: "huddle"}[cue]
	sound := map[notify.Cue]string{notify.CueMention: "mention", notify.CueMessage: "message", notify.CueRing: "ring", notify.CueHuddle: "huddle"}[cue]
	if !m.nt.prefs.Sound(sound) {
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
		if m.nt.prefs.DND(now) {
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
		m.setInfo("do not disturb until " + time.UnixMilli(until).Format("15:04") + ": no popups, sounds or rings")
	}
	return nil
}

func (m *Model) soundsCmd(nb NotifyBackend, args []string) error {
	usage := errors.New("/sounds [on|off] · /sounds mention|message|ring|huddle|join|mic on|off · /sounds volume 0-100")
	set := func(k, v string) error { return nb.SetNotifySetting(k, v) }
	switch {
	case len(args) == 0:
		p := m.nt.prefs
		var parts []string
		for _, c := range client.SoundCues {
			st := "on"
			if !p.Sound(c) {
				st = "off"
			}
			parts = append(parts, c+" "+st)
		}
		master := "on"
		if p.SoundOff {
			master = "off"
		}
		m.setNotice(fmt.Sprintf("sounds %s · %s · huddle cue volume %d", master, strings.Join(parts, " · "), p.Volume), false, true, 0)
		return nil
	case len(args) == 1 && (args[0] == "on" || args[0] == "off"):
		v := ""
		if args[0] == "off" {
			v = "off"
		}
		if err := set("sound.all", v); err != nil {
			return err
		}
		m.setInfo("sounds " + args[0] + " (popups still show)")
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
