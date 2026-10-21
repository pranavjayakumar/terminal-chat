package tui

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
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
	warn     error
	openMic  bool
	aec      string // "" = system
	note     string
}

func (h *huddleFake) SetOpenMic(open bool) {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	h.openMic = open
}

func (h *huddleFake) VoiceNote() string {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	if h.in == 0 {
		return ""
	}
	return h.note
}

func (h *huddleFake) VoiceAEC() string {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	if h.in == 0 {
		return ""
	}
	if h.aec == "" {
		return "system"
	}
	return h.aec
}

func (h *huddleFake) OpenMic() bool {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	return h.openMic
}

func (h *huddleFake) Huddles() map[uint64]int {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	out := map[uint64]int{}
	for ch, r := range h.rosters {
		if len(r) > 0 {
			out[ch] = len(r)
		}
	}
	return out
}

func (h *huddleFake) HuddleWarning() error {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	return h.warn
}

func (h *huddleFake) Huddle(ch uint64) []uint64 {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	return h.rosters[ch]
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
	if !on {
		h.openMic = false // as the engine does
	}
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

// update tells the model the backend changed, as the client would.
func (h *huddleFake) update(m *Model) {
	m.Update(updateMsg(client.Update{Huddle: true}))
}

func TestHuddleJoinTalkLeave(t *testing.T) {
	m, h := newHuddleModel(t, 100, 24)
	clk := newClock(m)
	// ctrl+t outside a huddle does nothing voice-related.
	ctrlT(m)
	if len(h.talks) != 0 {
		t.Fatal("ctrl+t talked outside a huddle")
	}
	h.rosters[m.active] = []uint64{2}
	h.update(m)
	if v := checkView(t, m, "huddle elsewhere"); !strings.Contains(v, "♪ huddle") || !strings.Contains(v, "bob ⣀⣀") || !strings.Contains(v, "join") {
		t.Fatalf("live huddle not shown:\n%s", v)
	}

	runInput(t, m, "/huddle")
	if ch, in := h.InHuddle(); !in || ch != m.active {
		t.Fatalf("not joined: %v %v (notice %q)", ch, in, m.notice)
	}
	h.speaking = map[uint64]bool{2: true}
	m.clearNotice()
	h.update(m)
	if v := checkView(t, m, "in huddle"); !strings.Contains(v, "♪ huddle") || !strings.Contains(v, "bob ") || !strings.Contains(v, "leave") {
		t.Fatalf("status line:\n%s", v)
	}
	runInput(t, m, "/huddle")
	if m.noticeErr || !strings.Contains(m.notice, "echo cancellation on (system) · /leave-huddle") {
		t.Fatalf("rejoin notice %q", m.notice)
	}

	// Tap: talking starts; no repeat within firstRepeatWait latches it.
	clk.add(time.Second)
	ctrlT(m)
	if !h.Talking() {
		t.Fatal("tap didn't start talking")
	}
	m.Update(talkCheckMsg{m.hud.talkGen})
	if !h.Talking() || m.hud.talk != talkLatched {
		t.Fatalf("tap should latch: talking=%v mode=%v", h.Talking(), m.hud.talk)
	}
	m.clearNotice()
	if v := checkView(t, m, "talking"); !strings.Contains(v, "● live") {
		t.Fatalf("own mic not shown:\n%s", v)
	}
	clk.add(time.Second)
	ctrlT(m)
	if h.Talking() {
		t.Fatal("second tap didn't mute")
	}

	// Hold with a slow (X11, 660 ms) initial repeat delay, then fast repeats.
	clk.add(time.Second)
	ctrlT(m)
	clk.add(660 * time.Millisecond)
	ctrlT(m) // first repeat: still talking, now holding
	if !h.Talking() || m.hud.talk != talkHolding {
		t.Fatalf("slow first repeat: talking=%v mode=%v", h.Talking(), m.hud.talk)
	}
	for range 5 {
		clk.add(33 * time.Millisecond)
		first := m.hud.talkGen
		ctrlT(m)
		m.Update(talkCheckMsg{first}) // superseded by the newer press
	}
	if !h.Talking() {
		t.Fatal("stale check stopped talking mid-hold")
	}
	m.Update(talkCheckMsg{m.hud.talkGen}) // released
	if h.Talking() || m.hud.talk != talkOff {
		t.Fatal("release didn't stop talking")
	}

	// A repeat that lands on a latched mic (hold detected late) keeps talking.
	clk.add(time.Second)
	ctrlT(m)
	m.Update(talkCheckMsg{m.hud.talkGen}) // latched
	clk.add(30 * time.Millisecond)
	ctrlT(m)
	if !h.Talking() || m.hud.talk != talkHolding {
		t.Fatalf("repeat after latch: talking=%v mode=%v", h.Talking(), m.hud.talk)
	}
	m.Update(talkCheckMsg{m.hud.talkGen})

	// /talk and /mute work where ctrl+t can't.
	runInput(t, m, "/talk")
	if !h.Talking() {
		t.Fatal("/talk")
	}
	runInput(t, m, "/mute")
	if h.Talking() {
		t.Fatal("/mute")
	}

	runInput(t, m, "/leave-huddle")
	if _, in := h.InHuddle(); in {
		t.Fatal("still in huddle")
	}
	runInput(t, m, "/leave-huddle")
	if !m.noticeErr || !strings.Contains(m.notice, "not in a huddle") {
		t.Fatalf("notice %q", m.notice)
	}
	runInput(t, m, "/talk")
	if !m.noticeErr {
		t.Fatal("/talk outside a huddle should fail")
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

// At 40 columns, joined last with the mic latched: the mic marker and an
// error notice must both survive truncation.
func TestHuddleNarrowKeepsMicAndErrors(t *testing.T) {
	m, h := newHuddleModel(t, 40, 12)
	h.rosters[m.active] = []uint64{2, 3, 4}
	runInput(t, m, "/huddle")
	runInput(t, m, "/talk")
	m.clearNotice()
	h.update(m)
	if v := checkView(t, m, "narrow talking"); !strings.Contains(v, "● live") {
		t.Fatalf("mic marker lost at 40 cols:\n%s", v)
	}
	m.setErr("send", errors.New("offline"))
	v := checkView(t, m, "narrow error")
	if !strings.Contains(v, "! send") || !strings.Contains(v, "● live") {
		t.Fatalf("error or mic lost at 40 cols:\n%s", v)
	}
}

func TestHuddleViewFits(t *testing.T) {
	for _, sz := range [][2]int{{40, 12}, {60, 15}, {120, 40}} {
		m, h := newHuddleModel(t, sz[0], sz[1])
		runInput(t, m, "/huddle")
		h.rosters[m.active] = []uint64{1, 2, 3, 4}
		h.speaking = map[uint64]bool{4: true}
		ctrlT(m)
		h.update(m)
		checkView(t, m, "huddle with long names")
	}
}

var _ HuddleBackend = (*client.Client)(nil)

// The sidebar marks channels with a huddle going (♪ + count) and drops
// the mark before squeezing the name; every frame still fits.
func TestHuddleSidebarMarker(t *testing.T) {
	for _, sz := range [][2]int{{120, 30}, {100, 24}, {60, 15}, {40, 12}} {
		m, h := newHuddleModel(t, sz[0], sz[1])
		h.rosters[11] = []uint64{2, 3}
		h.rosters[20] = []uint64{2}
		h.update(m)
		v := checkView(t, m, "sidebar huddle marker")
		if !m.sidebarShown() && !strings.Contains(ansi.Strip(v), "♪2") {
			t.Fatalf("%dx%d: no sidebar and no header hint of the 2 huddles:\n%s", sz[0], sz[1], v)
		}
		if m.sidebarShown() && sz[0] >= 100 {
			if v := ansi.Strip(v); !regexp.MustCompile(`backend +♪2`).MatchString(v) || !regexp.MustCompile(`bob +♪1`).MatchString(v) {
				t.Fatalf("%dx%d: no marker:\n%s", sz[0], sz[1], v)
			}
		}
		h.rosters[11] = nil
		h.update(m)
		if v := checkView(t, m, "huddle ended"); regexp.MustCompile(`backend +♪`).MatchString(ansi.Strip(v)) {
			t.Fatalf("marker after the huddle ended:\n%s", v)
		}
	}
}

// herdr's pane badge shows 🎧 while we're in a huddle, unread or not.
func TestHuddleHerdrBadge(t *testing.T) {
	m, h := newHuddleModel(t, 100, 24)
	before := m.badgeText()
	runInput(t, m, "/huddle")
	h.update(m)
	got := m.badgeText()
	if !strings.HasPrefix(got, "tc · 🎧") || (before != "" && got != "tc · 🎧 · "+strings.TrimPrefix(before, "tc · ")) {
		t.Fatalf("badge %q (before %q)", got, before)
	}
	runInput(t, m, "/leave-huddle")
	if got := m.badgeText(); got != before {
		t.Fatalf("badge after leaving %q, want %q", got, before)
	}
}

// Repeated clock-skew drops show one sticky warning per participation.
func TestHuddleClockSkewWarning(t *testing.T) {
	m, h := newHuddleModel(t, 60, 15)
	runInput(t, m, "/huddle")
	m.clearNotice()
	h.warn = client.ErrClockSkew
	h.update(m)
	v := checkView(t, m, "skew warning")
	if !m.noticeErr || !strings.Contains(m.notice, "clock") || !strings.Contains(v, "clock") {
		t.Fatalf("notice %q\n%s", m.notice, v)
	}
	m.clearNotice()
	h.update(m)
	if m.notice != "" {
		t.Fatalf("warning repeated: %q", m.notice)
	}
	// Hopping straight to another huddle: its warning shows too.
	m.active = 11
	h.warn = nil
	runInput(t, m, "/huddle")
	h.warn = client.ErrClockSkew
	m.clearNotice()
	h.update(m)
	if !strings.Contains(m.notice, "clock") {
		t.Fatal("warning swallowed after hopping huddles")
	}
	runInput(t, m, "/leave-huddle")
	h.warn = nil // a new participation starts clean
	runInput(t, m, "/huddle")
	h.warn = client.ErrClockSkew
	m.clearNotice()
	h.update(m)
	if !strings.Contains(m.notice, "clock") {
		t.Fatal("no warning in the next huddle")
	}
}

// In a huddle the ● live mark stays visible even behind a sticky error, Esc
// closes an open mic, and herdr's title shows the open mic.
func TestHuddleOpenMicVisible(t *testing.T) {
	for _, w := range []int{20, 40, 80} {
		m, h := newHuddleModel(t, w, 12)
		runInput(t, m, "/huddle")
		runInput(t, m, "/talk")
		h.warn = client.ErrClockSkew
		h.update(m)
		if v := ansi.Strip(checkView(t, m, "open mic + error")); !strings.Contains(v, "● live") {
			t.Fatalf("%d cols: open mic hidden:\n%s", w, v)
		}
		if got := m.badgeText(); !strings.HasPrefix(got, "tc · 🎧●") {
			t.Fatalf("badge %q", got)
		}
		m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if h.Talking() || !strings.Contains(m.notice, "mic off") {
			t.Fatalf("Esc left the mic open (notice %q)", m.notice)
		}
		if got := m.badgeText(); strings.Contains(got, "●") {
			t.Fatalf("badge %q after mic off", got)
		}
	}
}

// Meters are a braille line: one dot per sample at its level, two
// samples per cell.
func TestBrailleLineMeter(t *testing.T) {
	if got := braille([]uint8{0, 0, 3, 3, 1, 2}); got != "\u28c0\u2809\u2814" {
		t.Fatalf("braille = %q", got)
	}
	if got := braille([]uint8{2}); got != "⢂" {
		t.Fatalf("odd count = %q", got)
	}
}

// In a huddle the bar shows our own rolling meter (10 samples, 5 cells)
// with ● live while the mic sends and ⊘ muted otherwise; others get 2
// cells. Samples roll only on the animation tick while we're in.
func TestOwnVoiceMeter(t *testing.T) {
	m, h := newHuddleModel(t, 120, 30)
	clk := newClock(m)
	m.fx.off = false
	h.rosters[m.active] = []uint64{2}
	runInput(t, m, "/huddle")
	h.update(m)
	v := checkView(t, m, "muted")
	if !regexp.MustCompile(`you [⠀-⣿]{5} ⊘ muted`).MatchString(v) || !regexp.MustCompile(`bob [⠀-⣿]{2}`).MatchString(v) {
		t.Fatalf("muted bar:\n%s", v)
	}
	runInput(t, m, "/talk")
	h.update(m)
	for i := 0; i < 12; i++ {
		clk.add(200 * time.Millisecond)
		m.Update(frameMsg{gen: m.fx.gen})
	}
	if s := m.hud.meters[m.me.ID]; len(s) != meMeterN {
		t.Fatalf("own samples %v, want %d", s, meMeterN)
	}
	if s := m.hud.meters[2]; len(s) != otherMeterN {
		t.Fatalf("bob's samples %v, want %d", s, otherMeterN)
	}
	if !m.slowWanted() {
		t.Fatal("no tick in a huddle")
	}
	v = checkView(t, m, "live")
	if !strings.Contains(v, "● live") || strings.Contains(v, "⊘ muted") {
		t.Fatalf("live bar:\n%s", v)
	}
	// talking samples sit above the floor
	moved := false
	for _, l := range m.hud.meters[m.me.ID] {
		moved = moved || l > 0
	}
	if !moved {
		t.Fatal("own meter flat while talking")
	}
	runInput(t, m, "/leave-huddle")
	h.update(m)
	m.Update(frameMsg{gen: m.fx.gen})
	if m.hud.meters != nil {
		t.Fatal("meters kept after leaving")
	}
}
