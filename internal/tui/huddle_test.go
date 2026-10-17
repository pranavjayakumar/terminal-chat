package tui

import (
	"context"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/voice"
)

// huddleFake adds HuddleBackend to the fake client.
type huddleFake struct {
	*fakeClient
	hmu      sync.Mutex
	rosters  map[uint64][]uint64
	in       uint64
	talking  bool
	talks    []bool
	speaking map[uint64]bool
	joinErr  error
}

func (h *huddleFake) Huddle(ch uint64) []uint64 {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	return h.rosters[ch]
}

func (h *huddleFake) Huddles() map[uint64]int {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	out := map[uint64]int{}
	for ch, r := range h.rosters {
		out[ch] = len(r)
	}
	return out
}

func (h *huddleFake) InHuddle() (uint64, bool) {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	return h.in, h.in != 0
}

func (h *huddleFake) JoinHuddle(_ context.Context, ch uint64) error {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	if h.joinErr != nil {
		return h.joinErr
	}
	h.in = ch
	h.rosters[ch] = append(h.rosters[ch], 1)
	return nil
}

func (h *huddleFake) LeaveHuddle() {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	h.rosters[h.in] = nil
	h.in, h.talking = 0, false
}

func (h *huddleFake) SetTalking(on bool) {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	h.talking = on
	h.talks = append(h.talks, on)
}

func (h *huddleFake) Talking() bool {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	return h.talking
}

func (h *huddleFake) Speaking() map[uint64]bool {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	return h.speaking
}

func newHuddleModel(t *testing.T, w, hgt int) (*Model, *huddleFake) {
	t.Helper()
	h := &huddleFake{fakeClient: newFake(), rosters: map[uint64][]uint64{}}
	m := New(context.Background(), h, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: w, Height: hgt})
	return m, h
}

func ctrlT(m *Model) tea.Cmd {
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	return cmd
}

func TestHuddleCommandWithoutSupport(t *testing.T) {
	m := newTestModel(t, 100, 24)
	runInput(t, m, "/huddle")
	if !m.noticeErr || !strings.Contains(m.notice, "aren't available") {
		t.Fatalf("notice %q", m.notice)
	}
}

func TestHuddleJoinTalkLeave(t *testing.T) {
	m, h := newHuddleModel(t, 100, 24)
	// ctrl+t outside a huddle does nothing voice-related.
	ctrlT(m)
	if len(h.talks) != 0 {
		t.Fatal("ctrl+t talked outside a huddle")
	}
	h.rosters[m.active] = []uint64{2}
	if v := checkView(t, m, "huddle elsewhere"); !strings.Contains(v, "🎧 1 in huddle — /huddle") {
		t.Fatalf("live huddle not shown:\n%s", v)
	}

	runInput(t, m, "/huddle")
	if ch, in := h.InHuddle(); !in || ch != m.active {
		t.Fatalf("not joined: %v %v (notice %q)", ch, in, m.notice)
	}
	h.speaking = map[uint64]bool{2: true}
	v := checkView(t, m, "in huddle")
	if !strings.Contains(v, "🎧 #general · bob* alice") {
		t.Fatalf("status line:\n%s", v)
	}
	runInput(t, m, "/huddle")
	if !m.noticeErr || !strings.Contains(m.notice, "already in this huddle") {
		t.Fatalf("rejoin notice %q", m.notice)
	}

	// Tap: talking starts, and with no key-repeat it latches open.
	ctrlT(m)
	if !h.Talking() {
		t.Fatal("tap didn't start talking")
	}
	m.Update(talkCheckMsg{m.hud.talkGen})
	if !h.Talking() || m.hud.talk != talkLatched {
		t.Fatalf("tap should latch: talking=%v mode=%v", h.Talking(), m.hud.talk)
	}
	if v := checkView(t, m, "talking"); !strings.Contains(v, "alice🎙") {
		t.Fatalf("own mic not shown:\n%s", v)
	}
	ctrlT(m)
	if h.Talking() {
		t.Fatal("second tap didn't mute")
	}

	// Hold: key-repeats keep talking; it stops after the last one.
	ctrlT(m)
	first := m.hud.talkGen
	ctrlT(m)
	ctrlT(m)
	m.Update(talkCheckMsg{first}) // superseded by the repeats
	if !h.Talking() {
		t.Fatal("stale check stopped talking mid-hold")
	}
	m.Update(talkCheckMsg{m.hud.talkGen})
	if h.Talking() || m.hud.talk != talkOff {
		t.Fatal("release didn't stop talking")
	}

	runInput(t, m, "/leave-huddle")
	if _, in := h.InHuddle(); in {
		t.Fatal("still in huddle")
	}
	runInput(t, m, "/leave-huddle")
	if !m.noticeErr || !strings.Contains(m.notice, "not in a huddle") {
		t.Fatalf("notice %q", m.notice)
	}
}

func TestHuddleJoinErrorShown(t *testing.T) {
	m, h := newHuddleModel(t, 100, 24)
	h.joinErr = voice.ErrNoVoice
	runInput(t, m, "/huddle")
	if !m.noticeErr || !strings.Contains(m.notice, "no voice") {
		t.Fatalf("notice %q", m.notice)
	}
}

func TestHuddleViewFits(t *testing.T) {
	for _, sz := range [][2]int{{40, 12}, {60, 15}, {120, 40}} {
		m, h := newHuddleModel(t, sz[0], sz[1])
		runInput(t, m, "/huddle")
		h.rosters[m.active] = []uint64{1, 2, 3, 4}
		h.speaking = map[uint64]bool{4: true}
		ctrlT(m)
		checkView(t, m, "huddle with long names")
	}
}
