package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Watching someone in another huddle leaves ours: only when asked twice.
func TestWatchAsksBeforeLeavingHuddle(t *testing.T) {
	m, w, _ := newWatchModel(t, 100, 24)
	runInput(t, m, "/huddle")
	mine := m.active
	other := mine + 1000
	w.rosters[other] = []uint64{2}
	w.live[other] = 2
	w.update(m)
	runInput(t, m, "/watch @bob")
	if in, _ := w.InHuddle(); in != mine || !m.noticeErr || !strings.Contains(m.notice, "again to leave") {
		t.Fatalf("left without asking: in %d, notice %q", in, m.notice)
	}
	runInput(t, m, "/watch @bob")
	if in, _ := w.InHuddle(); in != other || w.watching != 2 {
		t.Fatalf("second /watch: in %d watching %d (%q)", in, w.watching, m.notice)
	}
}

// Esc while /pair is still starting stops it too.
func TestPairEscWhileStarting(t *testing.T) {
	m, w, _ := newWatchModel(t, 100, 24)
	runInput(t, m, "/huddle")
	m.wt.pairing = true
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if w.stops != 1 || m.wt.pairing {
		t.Fatalf("Esc during a starting /pair: %d stops", w.stops)
	}
}
