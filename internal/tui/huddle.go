package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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
	// Huddles counts people per channel huddle (the sidebar's 🎧 marker).
	Huddles() map[uint64]int
	// HuddleWarning is a problem with our huddle worth telling the user
	// (repeated clock-skew drops), or nil.
	HuddleWarning() error
	// SetOpenMic: the mic is latched open (tap, /talk), not held; only an
	// open mic is gated against echo.
	SetOpenMic(open bool)
	// VoiceAEC is our huddle's echo control: system, gate or off.
	VoiceAEC() string
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
	counts   map[uint64]int
	warned   error // the HuddleWarning already shown
	aec      string
	// aecHinted: the headphones hint was shown (once per run).
	aecHinted bool
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
	h.cur, h.in = hb.InHuddle()
	h.roster, h.speaking, h.talking, h.here = nil, nil, false, 0
	h.counts = hb.Huddles()
	if h.in {
		h.roster = hb.Huddle(h.cur)
		h.speaking = hb.Speaking()
		h.talking = hb.Talking()
		h.aec = hb.VoiceAEC()
		if w := hb.HuddleWarning(); w != nil && !errors.Is(w, h.warned) {
			h.warned = w
			m.setNotice(w.Error(), true, true, 0)
		}
	} else {
		h.talk = talkOff
		h.warned = nil
		h.here = len(hb.Huddle(m.active))
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
// 🎧 and how many are in it ("" when none).
func (m *Model) huddleMarker(ch uint64) string {
	if n := m.hud.counts[ch]; n > 0 {
		return " 🎧" + strconv.Itoa(n)
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
		}
		m.refreshHuddle()
		return nil, true, nil
	}
	ch := m.active
	if m.nt.ring.ch != 0 { // answering a ring
		ch = m.nt.ring.ch
	}
	if ch == 0 {
		return nil, true, errors.New("no channel open")
	}
	if in && cur == ch {
		m.refreshHuddle()
		m.setNotice(aecLine(m.hud.aec)+" · /leave-huddle to leave", false, false, errTTL)
		return nil, true, nil
	}
	prev := uint64(0)
	if in {
		prev = cur
	}
	if ch == m.nt.ring.ch {
		m.nt.answering = true
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
	m.setNotice("mic off", false, false, 0)
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
		m.refreshHuddle()
		const talkHelp = "hold ctrl+t to talk, tap for open mic (/talk, /mute)"
		if m.hud.in && m.hud.aec != "system" && !m.hud.aecHinted {
			// Speakers into an open mic echo back to whoever talks. The
			// hint leads so truncation eats the talk help, not the hint
			// (the 🎧 head already says where we are).
			m.hud.aecHinted = true
			note := headphonesHint + " · " + talkHelp
			if m.w < 60 {
				note = headphonesShort + " · " + talkHelp
			}
			m.setNotice(note, false, false, errTTL)
			return nil, true
		}
		note := "in the huddle — " + talkHelp
		if msg.prev != 0 {
			note = "left " + m.chanTitle(msg.prev) + " · " + note
		}
		m.setInfo(note)
		return nil, true
	}
	return nil, false
}

// huddleHead is the part of the status line that must survive truncation:
// 🎧, our mic state, and where. compact drops the roster.
func (m *Model) huddleHead(compact bool) (head, roster string) {
	h := &m.hud
	if !h.in {
		switch {
		case h.here == 0:
			return "", ""
		case compact:
			return fmt.Sprintf("🎧%d", h.here), ""
		}
		return fmt.Sprintf("🎧 %d in huddle — /huddle", h.here), ""
	}
	var b strings.Builder
	b.WriteString("🎧 ")
	if h.talking {
		b.WriteString(m.st.noticeErr.Render("●") + " ")
	}
	if v, ok := m.chanView(h.cur); ok && v.E2E {
		b.WriteString("🔒")
	}
	b.WriteString(m.chanTitle(h.cur))
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
	// headphonesShort fits after the 🎧 head in a 40-column pane.
	headphonesShort = "use headphones"
)

// aecLine describes the echo control for /huddle (docs/VOICE.md "Echo"),
// most useful words first.
func aecLine(aec string) string {
	switch aec {
	case "system":
		return "echo cancellation on (system)"
	case "gate":
		return "no echo cancellation, use headphones (gate: open mic pauses while others talk)"
	case "off":
		return "echo cancellation off, use headphones"
	}
	return "echo control: " + aec
}
