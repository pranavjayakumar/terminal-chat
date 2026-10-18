package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
)

// A huddle that ends without /leave-huddle says why, once (#76).
func TestHuddleEndedReasonShown(t *testing.T) {
	m, h := newHuddleModel(t, 100, 24)
	runInput(t, m, "/huddle")
	m.clearNotice()
	drop := func(why error) {
		h.hmu.Lock()
		h.rosters[h.in] = []uint64{1}
		h.in, h.talking, h.warn = 0, false, why
		h.hmu.Unlock()
		h.update(m)
	}
	drop(client.ErrHuddleMoved)
	v := checkView(t, m, "moved")
	if !strings.Contains(m.notice, "moved to your other device") || !strings.Contains(v, "moved to your other device") {
		t.Fatalf("notice %q\n%s", m.notice, v)
	}
	m.clearNotice()
	h.update(m)
	if m.notice != "" {
		t.Fatalf("reason repeated: %q", m.notice)
	}
	// The next participation ending the same way says so again.
	h.warn = nil
	runInput(t, m, "/huddle")
	m.clearNotice()
	drop(client.ErrHuddleMoved)
	if !strings.Contains(m.notice, "moved") {
		t.Fatalf("second drop not shown: %q", m.notice)
	}
	h.warn = nil
	runInput(t, m, "/huddle")
	m.clearNotice()
	drop(client.ErrHuddleConnLost)
	if !strings.Contains(m.notice, "connection lost") {
		t.Fatalf("notice %q", m.notice)
	}
}

// Reopening the mic never leaves "mic off" next to the red ● (#77).
func TestHuddleMicOffNoticeClearedOnTalk(t *testing.T) {
	m, h := newHuddleModel(t, 100, 24)
	runInput(t, m, "/huddle")
	runInput(t, m, "/talk")
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if h.Talking() || m.notice != "mic off" {
		t.Fatalf("Esc: talking=%v notice %q", h.Talking(), m.notice)
	}
	runInput(t, m, "/talk")
	if v := checkView(t, m, "talk again"); strings.Contains(v, "mic off") {
		t.Fatalf("mic on but the status line says mic off:\n%s", v)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	ctrlT(m)
	if v := checkView(t, m, "ctrl+t again"); strings.Contains(v, "mic off") {
		t.Fatalf("mic on (ctrl+t) but the status line says mic off:\n%s", v)
	}
}
