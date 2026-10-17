package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/sanitize"
)

// Voice huddles (docs/VOICE.md "UX"). The Backend may not support them
// (older fakes); everything here checks for HuddleBackend first.

// HuddleBackend is the part of client.Client the huddle UI uses.
type HuddleBackend interface {
	Huddle(ch uint64) []uint64
	Huddles() map[uint64]int
	InHuddle() (uint64, bool)
	JoinHuddle(ctx context.Context, ch uint64) error
	LeaveHuddle()
	SetTalking(on bool)
	Talking() bool
	Speaking() map[uint64]bool
}

const (
	// talkHold: a ctrl+t with no key-repeat within this is a tap (latch the
	// mic open); with repeats, talking stops this long after the last one.
	// Longer than common OS initial repeat delays (macOS ≈ 375–500 ms).
	talkHold      = 600 * time.Millisecond
	huddleRefresh = 250 * time.Millisecond
)

type talkMode int

const (
	talkOff talkMode = iota
	talkPending
	talkHolding
	talkLatched
)

type huddleUI struct {
	talk    talkMode
	talkGen int
	ticking bool
}

type (
	talkCheckMsg  struct{ gen int }
	huddleTickMsg struct{}
	huddleJoinMsg struct{ ch uint64 }
)

func (m *Model) huddleBackend() (HuddleBackend, bool) {
	hb, ok := m.c.(HuddleBackend)
	return hb, ok
}

func (m *Model) huddleCommand(c command) (tea.Cmd, bool, error) {
	switch c.name {
	case "huddle", "leave-huddle":
	default:
		return nil, false, nil
	}
	hb, ok := m.huddleBackend()
	if !ok {
		return nil, true, errors.New("huddles aren't available")
	}
	if c.name == "leave-huddle" {
		if _, in := hb.InHuddle(); !in {
			return nil, true, errors.New("not in a huddle")
		}
		hb.LeaveHuddle()
		m.hud.talk = talkOff
		m.setInfo("left the huddle")
		return nil, true, nil
	}
	if m.active == 0 {
		return nil, true, errors.New("no channel open")
	}
	ch := m.active
	if cur, in := hb.InHuddle(); in && cur == ch {
		return nil, true, errors.New("already in this huddle (/leave-huddle to leave)")
	}
	return m.callThen("huddle", func(ctx context.Context) (tea.Msg, error) {
		return huddleJoinMsg{ch}, hb.JoinHuddle(ctx, ch)
	}), true, nil
}

// talkKey handles ctrl+t; ok is false outside a huddle so the key does
// nothing special there.
func (m *Model) talkKey() (tea.Cmd, bool) {
	hb, ok := m.huddleBackend()
	if !ok {
		return nil, false
	}
	if _, in := hb.InHuddle(); !in {
		return nil, false
	}
	switch m.hud.talk {
	case talkOff:
		hb.SetTalking(true)
		m.hud.talk = talkPending
	case talkPending, talkHolding:
		m.hud.talk = talkHolding // a key-repeat: the key is held
	case talkLatched:
		hb.SetTalking(false)
		m.hud.talk = talkOff
		return nil, true
	}
	m.hud.talkGen++
	gen := m.hud.talkGen
	return tea.Tick(talkHold, func(time.Time) tea.Msg { return talkCheckMsg{gen} }), true
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
		case talkPending:
			m.hud.talk = talkLatched
			m.setInfo("mic open — ctrl+t again to mute")
		case talkHolding:
			hb.SetTalking(false)
			m.hud.talk = talkOff
		}
		return nil, true
	case huddleJoinMsg:
		m.hud.talk = talkOff
		m.setInfo("in the huddle — hold ctrl+t to talk, tap to keep the mic open")
		return m.huddleTick(), true
	case huddleTickMsg:
		m.hud.ticking = false
		return m.huddleTick(), true
	}
	return nil, false
}

// huddleTick redraws the talking indicators while we're in a huddle.
func (m *Model) huddleTick() tea.Cmd {
	hb, ok := m.huddleBackend()
	if !ok {
		return nil
	}
	if _, in := hb.InHuddle(); !in {
		m.hud.talk = talkOff
		return nil
	}
	if m.hud.ticking {
		return nil
	}
	m.hud.ticking = true
	return tea.Tick(huddleRefresh, func(time.Time) tea.Msg { return huddleTickMsg{} })
}

// huddleText is the status-line part: our huddle with who's talking, or
// the size of a huddle in the open channel we're not in.
func (m *Model) huddleText() string {
	hb, ok := m.huddleBackend()
	if !ok {
		return ""
	}
	cur, in := hb.InHuddle()
	if !in {
		if n := len(hb.Huddle(m.active)); n > 0 {
			return fmt.Sprintf("🎧 %d in huddle — /huddle", n)
		}
		return ""
	}
	speaking := hb.Speaking()
	var names []string
	for _, u := range hb.Huddle(cur) {
		n := sanitize.Line(m.users[u].Name)
		if n == "" {
			n = fmt.Sprint(u)
		}
		if u == m.me.ID {
			if hb.Talking() {
				n += "🎙"
			}
		} else if speaking[u] {
			n += "*"
		}
		names = append(names, n)
	}
	where := m.chanTitle(cur)
	if v, ok := m.chanView(cur); ok && v.E2E {
		where = "🔒" + where
	}
	return "🎧 " + where + " · " + strings.Join(names, " ")
}
