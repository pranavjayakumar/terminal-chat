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
	if !strings.Contains(m.notice, "#general ended: connection lost · /huddle to rejoin") {
		t.Fatalf("notice %q", m.notice)
	}
	// Viewing another channel, /huddle wouldn't rejoin: don't suggest it.
	gen := m.active
	h.warn = nil
	runInput(t, m, "/huddle")
	m.active = 11
	m.clearNotice()
	drop(client.ErrHuddleDropped)
	if !strings.Contains(m.notice, "#general ended: you were dropped") || strings.Contains(m.notice, "/huddle") {
		t.Fatalf("notice %q", m.notice)
	}
	// An error already up (a failed send in the same drop) isn't replaced.
	m.active = gen
	h.warn = nil
	runInput(t, m, "/huddle")
	m.setNotice("message not sent: offline", true, false, 0)
	drop(client.ErrHuddleConnLost)
	if m.notice != "message not sent: offline" {
		t.Fatalf("notice %q", m.notice)
	}
}

// The ended notice fits and keeps the reason at narrow widths.
func TestHuddleEndedReasonNarrow(t *testing.T) {
	m, h := newHuddleModel(t, 40, 12)
	runInput(t, m, "/huddle")
	m.clearNotice()
	h.hmu.Lock()
	h.rosters[h.in] = nil
	h.in, h.warn = 0, client.ErrHuddleConnLost
	h.hmu.Unlock()
	h.update(m)
	if v := checkView(t, m, "narrow"); !strings.Contains(v, "ended: connection lost") {
		t.Fatalf("reason cut at 40 cols:\n%s", v)
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
