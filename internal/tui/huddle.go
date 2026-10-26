package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/sanitize"
)

// Voice huddles (docs/VOICE.md "UX"). The Backend may not support them
// (older fakes); everything here checks for HuddleBackend first.

// HuddleBackend is the part of client.Client the huddle UI uses. The client
// posts Update{Huddle} when rosters, our participation or who's talking
// change, so the UI never polls.
type HuddleBackend interface {
	Huddle(ch uint64) []uint64
	InHuddle() (uint64, bool)
	JoinHuddle(ctx context.Context, ch uint64) error
	LeaveHuddle()
	SetTalking(on bool)
	Talking() bool
	Speaking() map[uint64]bool
	// Huddles counts people per channel huddle (the sidebar's ♪ marker).
	Huddles() map[uint64]int
	// HuddleWarning is a problem with our huddle worth telling the user
	// (repeated clock-skew drops), or nil; while we're in no huddle, why the
	// last one ended without us leaving (#76), which the UI shows once.
	HuddleWarning() error
	// SetOpenMic: the mic is latched open (tap, /talk), not held; only an
	// open mic is gated against echo.
	SetOpenMic(open bool)
	// VoiceAEC is our huddle's echo control: system, gate or off.
	VoiceAEC() string
	// VoiceNote is a device problem worth saying (the default input is a
	// speaker monitor, TC_VOICE_INPUT matched nothing), or "".
	VoiceNote() string
}

// Talk-key timing. Terminals report presses, not releases: a held key
// shows up as key-repeats (every ~30–80 ms after an initial delay of
// ~250–660 ms depending on the OS).
const (
	// repeatGap: a press this soon after the previous one is a key-repeat.
	repeatGap = 150 * time.Millisecond
	// firstRepeatWait: after a fresh press, how long a first repeat may take
	// before the press counts as a tap (latching the mic open). Longer than
	// X11's 660 ms default delay.
	firstRepeatWait = 800 * time.Millisecond
	// Once repeats flow, talking stops this long after the last one:
	// 3× the measured gap, clamped.
	minRelease = 120 * time.Millisecond
	maxRelease = 250 * time.Millisecond
)

type talkMode int

const (
	talkOff     talkMode = iota
	talkPending          // pressed once, waiting to see a repeat (hold) or not (tap)
	talkHolding          // repeats are arriving: push-to-talk
	talkLatched          // tapped: open mic until the next fresh press
)

type huddleUI struct {
	talk      talkMode
	talkGen   int
	lastPress time.Time

	// snapshot, refreshed on updates and our own actions; View reads only
	// this, never the backend (whose locks the audio thread shares).
	in       bool
	cur      uint64
	roster   []uint64
	speaking map[uint64]bool
	talking  bool
	here     int // people in the open channel's huddle, when we're not in it
	hereWho  []uint64
	meters   map[uint64][]uint8 // recent meter samples per person (chrome.go)
	joinedAt time.Time          // when we joined the huddle we're in
	counts   map[uint64]int
	warned   error  // the HuddleWarning already shown
	last     uint64 // the huddle we were in last
	// endedNote is the "huddle ended" notice shown; a better reason may
	// replace it (dropped, then moved).
	endedNote string
	warnNote  string // the sticky in-huddle warning shown, if any
	aec       string
	note      string
	tcp       bool // voice rides the TCP fallback (§14.4)
	// aecHinted: the headphones hint was shown (once per run).
	aecHinted bool
	// followUp is shown when the notice of generation followGen expires
	// untouched (the headphones hint after a device note).
	followUp  string
	followGen int
	// seenActive is the channel here was computed for.
	seenActive uint64
}

type (
	talkCheckMsg  struct{ gen int }
	huddleJoinMsg struct{ ch, prev uint64 }
)

func (m *Model) huddleBackend() (HuddleBackend, bool) {
	hb, ok := m.c.(HuddleBackend)
	return hb, ok
}

// refreshHuddle re-reads the huddle snapshot from the backend.
func (m *Model) refreshHuddle() {
	hb, ok := m.huddleBackend()
	if !ok {
		return
	}
	h := &m.hud
	if h.in {
		h.last = h.cur
	}
	was, wasIn := h.cur, h.in
	h.cur, h.in = hb.InHuddle()
	if h.in && (!wasIn || was != h.cur) {
		h.joinedAt = m.now()
	}
	h.roster, h.speaking, h.talking, h.here, h.hereWho = nil, nil, false, 0, nil
	h.counts = hb.Huddles()
	w := hb.HuddleWarning()
	if h.in {
		h.roster = hb.Huddle(h.cur)
		h.speaking = hb.Speaking()
		h.talking = hb.Talking()
		h.aec = hb.VoiceAEC()
		h.note = hb.VoiceNote()
		st, _ := huddleStats(hb)
		h.tcp = st.TCP
		if w != nil && !errors.Is(w, h.warned) {
			h.warned = w
			h.warnNote = w.Error()
			m.setNotice(h.warnNote, true, true, 0)
		}
		return
	}
	h.talk = talkOff
	h.aec, h.note, h.followUp = "", "", ""
	// A sticky in-huddle warning (clock skew) means nothing once the
	// huddle is gone, and mustn't hide why it ended.
	if h.warnNote != "" && m.notice == h.warnNote {
		m.clearNotice()
	}
	h.warnNote = ""
	h.hereWho = hb.Huddle(m.active)
	h.here = len(h.hereWho)
	// Out of a huddle the warning is why the last one ended without us
	// leaving (#76): say it once.
	switch {
	case w == nil:
		h.warned = nil
	case !errors.Is(w, h.warned):
		h.warned = w
		// Stopped (revoked, disabled): the banner says more. Never hide
		// another error notice (a failed send in the same drop) either.
		if m.status.Stopped || (m.noticeErr && m.notice != "" && m.notice != h.endedNote) {
			return
		}
		h.endedNote = m.huddleEndedText(w)
		m.setNotice(h.endedNote, !errors.Is(w, client.ErrHuddleMoved), false, endedTTL)
	}
}

// huddleEndedText names the huddle that ended and why. "/huddle" is only
// suggested when it would rejoin that huddle: we're online, viewing its
// channel and no ring would take the command.
func (m *Model) huddleEndedText(why error) string {
	where := m.chanTitle(m.hud.last)
	if errors.Is(why, client.ErrHuddleMoved) {
		return m.icon(icHuddle) + " " + where + " moved to your other device"
	}
	s := m.icon(icHuddle) + " " + where + " ended: " + why.Error()
	if m.status.State == client.Online && m.hud.last == m.active && m.al.Ring.Ch == 0 {
		s += " · /huddle to rejoin"
	}
	return s
}

// endedTTL is how long "why the huddle ended" stays up.
const endedTTL = 10 * time.Second

// micOffNote is micOff's notice; opening the mic again clears it (#77).
const micOffNote = "mic off"

// micOn clears a "mic off" notice the mic just made stale.
func (m *Model) micOn() {
	if m.notice == micOffNote {
		m.clearNotice()
	}
}

// huddlesElsewhere counts huddles in my channels other than the open one
// (and the one I'm in).
func (m *Model) huddlesElsewhere() int {
	n := 0
	for ch, k := range m.hud.counts {
		if k > 0 && ch != m.active && !(m.hud.in && ch == m.hud.cur) {
			n++
		}
	}
	return n
}

// huddleMarker is the sidebar's mark for a channel with a huddle going:
// ♪ and how many are in it ("" when none).
func (m *Model) huddleMarker(ch uint64) string {
	if n := m.hud.counts[ch]; n > 0 {
		return m.icon(icHuddle) + strconv.Itoa(n)
	}
	return ""
}

func (m *Model) huddleCommand(c command) (tea.Cmd, bool, error) {
	switch c.name {
	case "huddle", "leave-huddle", "talk", "mute":
	default:
		return nil, false, nil
	}
	hb, ok := m.huddleBackend()
	if !ok {
		return nil, true, errors.New("huddles aren't available")
	}
	cur, in := hb.InHuddle()
	switch c.name {
	case "leave-huddle":
		if !in {
			return nil, true, errors.New("not in a huddle")
		}
		hb.LeaveHuddle()
		m.hud.talk = talkOff
		m.refreshHuddle()
		m.setInfo("left the huddle")
		return nil, true, nil
	case "talk", "mute":
		// A fallback for terminals where ctrl+t never arrives (Zellij) or
		// key-repeat is off.
		if !in {
			return nil, true, errors.New("not in a huddle (/huddle to join)")
		}
		on := c.name == "talk"
		hb.SetTalking(on)
		m.hud.talk = talkOff
		if on {
			hb.SetOpenMic(true)
			m.hud.talk = talkLatched
			m.micOn()
		}
		m.refreshHuddle()
		return nil, true, nil
	}
	if len(c.args) > 0 {
		// A typo must not join a huddle (and open a live call).
		if len(c.args) > 1 || !strings.EqualFold(c.args[0], "stats") {
			return nil, true, errors.New("usage: /huddle [stats]")
		}
		if !in {
			if !client.VoiceAvailable() {
				return nil, true, errors.New("no voice: tc update --voice")
			}
			return nil, true, errors.New("not in a huddle (/huddle to join)")
		}
		st, ok := huddleStats(hb)
		if !ok {
			return nil, true, errors.New("call stats unavailable")
		}
		m.setNotice(statsLine(st), false, false, errTTL)
		return nil, true, nil
	}
	ch := m.active
	if m.al.Ring.Ch != 0 { // answering a ring
		ch = m.al.Ring.Ch
	}
	if ch == 0 {
		return nil, true, errors.New("no channel open")
	}
	if in && cur == ch {
		m.refreshHuddle()
		// What was asked (echo control) first, then the device note.
		s := aecLine(m.hud.aec)
		if m.hud.tcp { // first: narrow panes cut the end
			s = tcpNote + " · " + s
		}
		if m.hud.note != "" {
			s += " · " + m.hud.note
		}
		if m.hud.warnNote != "" {
			s += " · " + m.hud.warnNote // the report replaces it on screen
		}
		m.setNotice(s+" · /leave-huddle to leave", false, false, errTTL)
		return nil, true, nil
	}
	prev := uint64(0)
	if in {
		prev = cur
	}
	if ch == m.al.Ring.Ch {
		m.nt.answering = m.al.Ring.Gen
	}
	return m.callThen("huddle", func(ctx context.Context) (tea.Msg, error) {
		return huddleJoinMsg{ch, prev}, hb.JoinHuddle(ctx, ch)
	}), true, nil
}

// micOff closes an open (latched) mic; Esc on an empty input calls it.
func (m *Model) micOff() bool {
	hb, ok := m.huddleBackend()
	if !ok || !m.hud.in || m.hud.talk != talkLatched {
		return false
	}
	hb.SetTalking(false)
	m.hud.talk = talkOff
	m.refreshHuddle()
	m.setNotice(micOffNote, false, false, 0)
	m.noticeDim = true
	return true
}

// talkKey handles ctrl+t; ok is false outside a huddle so the key keeps
// its usual meaning there.
func (m *Model) talkKey() (tea.Cmd, bool) {
	hb, ok := m.huddleBackend()
	if !ok {
		return nil, false
	}
	if _, in := hb.InHuddle(); !in {
		return nil, false
	}
	now := m.now()
	gap := now.Sub(m.hud.lastPress)
	m.hud.lastPress = now
	h := &m.hud
	wait := firstRepeatWait
	switch {
	case gap < repeatGap:
		// A key-repeat: the key is held, so talk — whatever we thought
		// before (a repeat right after a latch or unlatch is still a hold).
		if !hb.Talking() {
			hb.SetTalking(true)
			m.micOn()
		}
		hb.SetOpenMic(false) // held: never gated
		h.talk = talkHolding
		wait = min(maxRelease, max(minRelease, 3*gap))
	case h.talk == talkPending:
		// The first repeat after the OS's initial delay.
		h.talk = talkHolding
		wait = maxRelease
	case h.talk == talkLatched || h.talk == talkHolding:
		hb.SetTalking(false)
		h.talk = talkOff
		m.refreshHuddle()
		return nil, true
	default: // off: a fresh press
		hb.SetTalking(true)
		// Gateable until a repeat proves it's held: a tap's first 800 ms
		// would otherwise send echo.
		hb.SetOpenMic(true)
		m.micOn()
		h.talk = talkPending
	}
	m.refreshHuddle()
	h.talkGen++
	gen := h.talkGen
	return tea.Tick(wait, func(time.Time) tea.Msg { return talkCheckMsg{gen} }), true
}

// huddleMsg handles the huddle's own messages.
func (m *Model) huddleMsg(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case talkCheckMsg:
		if msg.gen != m.hud.talkGen {
			return nil, true // a later press superseded it
		}
		hb, ok := m.huddleBackend()
		if !ok {
			return nil, true
		}
		switch m.hud.talk {
		case talkPending: // no repeat came: a tap
			hb.SetOpenMic(true)
			m.hud.talk = talkLatched
		case talkHolding: // repeats stopped: released
			hb.SetTalking(false)
			m.hud.talk = talkOff
		}
		m.refreshHuddle()
		return nil, true
	case huddleJoinMsg:
		m.hud.talk = talkOff
		m.hud.warned = nil // a new participation: its own warnings
		m.hud.last = msg.ch
		m.refreshHuddle()
		if !m.hud.in {
			// It ended before the reply got here: refreshHuddle said why.
			return nil, true
		}
		// Most important first, since narrow panes truncate notices: the
		// device note (which mic, or none), the headphones hint (once per
		// run), the huddle we left, then the talk help. A device note and
		// the hint don't both fit in 80 columns, so then the hint follows
		// as its own notice when the note's expires.
		talkHelp := "hold ctrl+t to talk, tap for open mic (/talk, /mute)"
		// "" = the participation went away mid-refresh: unknown, no hint.
		hint := m.hud.aec != "" && m.hud.aec != "system" && !m.hud.aecHinted
		var parts []string
		switch {
		case m.hud.note != "":
			parts = append(parts, m.hud.note)
		case hint:
			m.hud.aecHinted = true
			if m.w < 60 {
				parts = append(parts, headphonesShort)
			} else {
				parts = append(parts, headphonesHint)
			}
		default:
			talkHelp = "in the huddle — " + talkHelp
		}
		if msg.prev != 0 {
			parts = append(parts, "left "+m.chanTitle(msg.prev))
		}
		parts = append(parts, talkHelp)
		ttl := time.Duration(0)
		if len(parts) > 1 {
			ttl = errTTL // more to read
		}
		m.setNotice(strings.Join(parts, " · "), false, false, ttl)
		m.hud.followUp, m.hud.followGen = "", 0
		if hint && !m.hud.aecHinted {
			m.hud.followUp, m.hud.followGen = headphonesHint+" · "+talkHelp, m.noticeGen
		}
		return nil, true
	}
	return nil, false
}

// huddleHead is the part of the status line that must survive truncation:
// ♪, our mic state, and where. compact drops the roster.
func (m *Model) huddleHead(compact bool) (head, roster string) {
	h := &m.hud
	if !h.in {
		switch {
		case h.here == 0:
			return "", ""
		case compact:
			return fmt.Sprintf("%s%d", m.icon(icHuddle), h.here), ""
		}
		return fmt.Sprintf("%s %d in huddle — /huddle", m.icon(icHuddle), h.here), ""
	}
	var b strings.Builder
	b.WriteString(m.icon(icHuddle) + " ")
	if h.talking {
		b.WriteString(m.st.noticeErr.Render(m.icon(icMic)) + " ")
	}
	if v, ok := m.chanView(h.cur); ok && v.E2E {
		b.WriteString("🔒")
	}
	b.WriteString(m.chanTitle(h.cur))
	if h.tcp { // for the whole call: notices expire
		b.WriteString(" tcp")
	}
	if compact {
		fmt.Fprintf(&b, " (%d)", len(h.roster))
		return b.String(), ""
	}
	var names []string
	for _, u := range h.roster {
		if u == m.me.ID {
			continue // our state is the ● above
		}
		n := sanitize.Line(m.users[u].Name)
		if n == "" {
			n = fmt.Sprint(u)
		}
		if h.speaking[u] {
			n += "*"
		}
		names = append(names, n)
	}
	if len(names) == 0 {
		return b.String(), " · just you"
	}
	return b.String(), " · " + strings.Join(names, " ")
}

const (
	headphonesHint = "no echo cancellation: use headphones"
	// headphonesShort fits after the ♪ head in a 40-column pane.
	headphonesShort = "use headphones"
)

// aecLine describes the echo control for /huddle (docs/VOICE.md "Echo"),
// most useful words first.
func aecLine(aec string) string {
	switch aec {
	case "system":
		return "echo cancellation on (system)"
	case "gate":
		return "echo: gate only, use headphones"
	case "off":
		return "echo cancellation off, use headphones"
	}
	if aec == "" {
		return "in this huddle"
	}
	return "echo control: " + aec
}

// huddleFollowUp shows the notice queued behind the one of generation gen
// (now expired), if we're still in the huddle.
func (m *Model) huddleFollowUp(gen int) bool {
	h := &m.hud
	if h.followUp == "" || h.followGen != gen {
		return false
	}
	s := h.followUp
	h.followUp = ""
	if !h.in {
		return false
	}
	if m.w < 60 {
		s = headphonesShort
	}
	h.aecHinted = true
	m.setNotice(s, false, false, errTTL)
	return true
}

// tcpNote: voice rides the TCP fallback (UDP blocked), so a lost packet
// delays audio instead of dropping it.
const tcpNote = "voice via tcp"

// huddleStatser is the client's call-quality report (older fakes lack it).
type huddleStatser interface {
	HuddleStats() (client.HuddleStats, bool)
}

func huddleStats(hb HuddleBackend) (client.HuddleStats, bool) {
	if s, ok := hb.(huddleStatser); ok {
		return s.HuddleStats()
	}
	return client.HuddleStats{}, false
}

// statsLine is /huddle stats: numbers to report from a real call, the
// transport and the ones that explain bad audio first (narrow panes
// truncate notices).
func statsLine(st client.HuddleStats) string {
	parts := []string{"udp", fmt.Sprintf("loss %.1f%%", st.LossPct)}
	if st.TCP {
		parts[0] = "tcp"
	}
	if st.BufferMs > 0 {
		parts = append(parts, fmt.Sprintf("buf %dms", st.BufferMs))
	}
	if st.Late > 0 {
		parts = append(parts, fmt.Sprintf("%d late", st.Late))
	}
	if st.FEC >= 0 && !st.TCP {
		parts = append(parts, fmt.Sprintf("fec %d%%", st.FEC))
	}
	if st.Kbps > 0 {
		parts = append(parts, fmt.Sprintf("%.0f kb/s", st.Kbps))
	}
	return strings.Join(parts, " · ")
}
