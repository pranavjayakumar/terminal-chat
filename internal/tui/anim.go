package tui

import (
	"math"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
)

// Motion. Every effect is a start time plus a fixed duration, read against
// m.now() while drawing. The frame tick runs only while one is in progress
// (fast) or something loops on screen, like typing dots (slow), so an idle
// screen costs no CPU.
const (
	fastFrame = time.Second / 30
	slowFrame = 200 * time.Millisecond

	dArrive = 200 * time.Millisecond
	dSend   = 300 * time.Millisecond
	dSpark  = 500 * time.Millisecond
	dAck    = 450 * time.Millisecond
	dReact  = 600 * time.Millisecond
	dSlide  = 150 * time.Millisecond
	dPane   = 200 * time.Millisecond
	dDialog = 125 * time.Millisecond
	dTheme  = 250 * time.Millisecond
	dToast  = 150 * time.Millisecond
	dPulse  = 700 * time.Millisecond
	dBoot   = 300 * time.Millisecond
)

type mkey struct {
	id    uint64
	local int64
}

func keyOf(id uint64, local int64) mkey {
	if id != 0 {
		return mkey{id: id}
	}
	return mkey{local: local}
}

type reactFx struct {
	emoji string
	at    time.Time
}

type fxState struct {
	off     bool // Settings › Animations off: everything is instant
	running bool // a frame tick is scheduled
	gen     int

	arrive map[mkey]time.Time
	sent   map[int64]time.Time // by outbox id: the message has no id yet
	acked  map[mkey]time.Time
	react  map[mkey]reactFx
	pulse  map[uint64]time.Time // channel whose mention badge grew

	slideN  int
	slideAt time.Time

	sbFrom, sbTo int
	sbAt         time.Time

	paneAt   time.Time // thread pane opened or closed
	paneOpen bool
	paneW    int

	dialogAt time.Time

	themePrev *canvas
	themeAt   time.Time

	bootAt time.Time // the loading screen started fading out
}

type frameMsg struct{ gen int }

func ease(p float64) float64 {
	p = min(max(p, 0), 1)
	return 1 - math.Pow(1-p, 3)
}

// progress is how far an effect that started at t is, 0..1; 1 when off.
func (m *Model) progress(t time.Time, d time.Duration) float64 {
	if m.fx.off || t.IsZero() {
		return 1
	}
	return min(max(float64(m.now().Sub(t))/float64(d), 0), 1)
}

func (m *Model) running(t time.Time, d time.Duration) bool {
	return !m.fx.off && !t.IsZero() && m.now().Sub(t) < d
}

// fastWanted reports whether any transition is still in progress.
func (m *Model) fastWanted() bool {
	f := &m.fx
	if f.off {
		return false
	}
	for _, t := range f.arrive {
		if m.running(t, dArrive) {
			return true
		}
	}
	for _, t := range f.sent {
		if m.running(t, dSpark) {
			return true
		}
	}
	for _, t := range f.acked {
		if m.running(t, dAck) {
			return true
		}
	}
	for _, r := range f.react {
		if m.running(r.at, dReact) {
			return true
		}
	}
	for _, t := range f.pulse {
		if m.running(t, dPulse) {
			return true
		}
	}
	return m.running(f.slideAt, dSlide) || m.running(f.sbAt, dSlide) || m.running(f.paneAt, dPane) ||
		m.running(f.dialogAt, dDialog) || m.running(f.themeAt, dTheme) || m.running(f.bootAt, dBoot) ||
		m.toastMoving()
}

// slowWanted reports whether something loops on screen: typing dots, a
// talking meter, the loading spinner.
func (m *Model) slowWanted() bool {
	if m.fx.off {
		return false
	}
	if m.booting() {
		return true
	}
	if len(m.typing) > 0 && m.mode == modeNormal {
		return true
	}
	for _, on := range m.hud.speaking {
		if on {
			return true
		}
	}
	return m.hud.in // the huddle bar's timer and our mic meter
}

// frameTick schedules the next frame when one is needed and none is.
func (m *Model) frameTick() tea.Cmd {
	if m.fx.running {
		return nil
	}
	d := time.Duration(0)
	switch {
	case m.fastWanted():
		d = fastFrame
	case m.slowWanted():
		d = slowFrame
	default:
		m.prune()
		if d = m.toastWake(); d == 0 {
			return nil
		}
	}
	m.fx.running = true
	m.fx.gen++
	gen := m.fx.gen
	return tea.Tick(d, func(time.Time) tea.Msg { return frameMsg{gen} })
}

// prune drops finished effects so the maps stay small.
func (m *Model) prune() {
	f := &m.fx
	for k, t := range f.arrive {
		if !m.running(t, dArrive) {
			delete(f.arrive, k)
		}
	}
	for k, t := range f.sent {
		if !m.running(t, dSpark) {
			delete(f.sent, k)
		}
	}
	for k, t := range f.acked {
		if !m.running(t, dAck) {
			delete(f.acked, k)
		}
	}
	for k, r := range f.react {
		if !m.running(r.at, dReact) {
			delete(f.react, k)
		}
	}
	for k, t := range f.pulse {
		if !m.running(t, dPulse) {
			delete(f.pulse, k)
		}
	}
	if !m.running(f.themeAt, dTheme) {
		f.themePrev = nil
	}
}

func (f *fxState) init() {
	f.arrive = map[mkey]time.Time{}
	f.sent = map[int64]time.Time{}
	f.acked = map[mkey]time.Time{}
	f.react = map[mkey]reactFx{}
	f.pulse = map[uint64]time.Time{}
}

// themeFrom keeps the last frame so the next ones cross-fade from it.
func (f *fxState) themeFrom(m *Model) {
	if f.off || m.lastCanvas == nil {
		return
	}
	f.themePrev = m.lastCanvas.clone()
	f.themeAt = m.now()
}

// noteChanges compares a pane's messages before and after a reload and
// starts the matching effects: arrival, send pop, sent flash, reaction.
func (m *Model) noteChanges(old, cur []msgSnap, oldMax uint64) {
	if m.fx.off {
		return
	}
	now := m.now()
	prev := make(map[mkey]msgSnap, len(old))
	byLocal := make(map[int64]msgSnap, len(old))
	for _, s := range old {
		prev[s.k] = s
		if s.local != 0 {
			byLocal[s.local] = s
		}
	}
	for _, s := range cur {
		p, ok := prev[s.k]
		if !ok && s.local != 0 {
			p, ok = byLocal[s.local]
		}
		if !ok && s.k.id != 0 && s.k.id <= oldMax {
			continue // older history loaded above: no effect
		}
		switch {
		case !ok && s.mine && s.pending:
			m.fx.sent[s.local] = now
		case !ok && !s.mine:
			m.fx.arrive[s.k] = now
		case !ok && s.mine:
			m.fx.acked[s.k] = now // our outbox message came back with its id
		case ok && p.pending && !s.pending:
			m.fx.acked[s.k] = now
		}
		if ok {
			for e, n := range s.reacts {
				if n > p.reacts[e] {
					m.fx.react[s.k] = reactFx{e, now}
				}
			}
		}
	}
}

// msgSnap is what noteChanges compares.
type msgSnap struct {
	k       mkey
	local   int64
	mine    bool
	pending bool
	reacts  map[string]int
}

func (m *Model) snapshot(ms []client.Message) []msgSnap {
	out := make([]msgSnap, len(ms))
	for i, x := range ms {
		s := msgSnap{k: keyOf(x.ID, x.Local), local: x.Local, mine: x.Author == m.me.ID, pending: x.Pending}
		if len(x.Reactions) > 0 {
			s.reacts = make(map[string]int, len(x.Reactions))
			for e, us := range x.Reactions {
				s.reacts[e] = len(us)
			}
		}
		out[i] = s
	}
	return out
}
