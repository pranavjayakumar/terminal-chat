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
			m.hud.talk = talkLatched
		}
		m.refreshHuddle()
		return nil, true, nil
	}
	if m.active == 0 {
		return nil, true, errors.New("no channel open")
	}
	ch := m.active
	if in && cur == ch {
		return nil, true, errors.New("already in this huddle (/leave-huddle to leave)")
	}
	prev := uint64(0)
	if in {
		prev = cur
	}
	return m.callThen("huddle", func(ctx context.Context) (tea.Msg, error) {
		return huddleJoinMsg{ch, prev}, hb.JoinHuddle(ctx, ch)
	}), true, nil
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
			m.hud.talk = talkLatched
		case talkHolding: // repeats stopped: released
			hb.SetTalking(false)
			m.hud.talk = talkOff
		}
		m.refreshHuddle()
		return nil, true
	case huddleJoinMsg:
		m.hud.talk = talkOff
		m.refreshHuddle()
		note := "in the huddle — hold ctrl+t to talk, tap for open mic (/talk, /mute)"
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
