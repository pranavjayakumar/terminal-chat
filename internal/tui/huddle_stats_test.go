package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/skailama/tc/internal/client"
)

// statsFake is a huddle backend that reports call quality.
type statsFake struct {
	*huddleFake
	st client.HuddleStats
}

func (s *statsFake) HuddleStats() (client.HuddleStats, bool) {
	if _, in := s.InHuddle(); !in {
		return client.HuddleStats{}, false
	}
	return s.st, true
}

func TestHuddleStatsAndTCP(t *testing.T) {
	for _, w := range []int{100, 40} {
		m, h := newHuddleModel(t, w, 24)
		sf := &statsFake{huddleFake: h, st: client.HuddleStats{TCP: true, Kbps: 31.6, LossPct: 0.4, FEC: 0, BufferMs: 60, Late: 3}}
		m.c = sf
		runInput(t, m, "/huddle stats")
		wantErr := "not in a huddle"
		if !client.VoiceAvailable() {
			wantErr = "no voice"
		}
		if !m.noticeErr || !strings.Contains(m.notice, wantErr) {
			t.Fatalf("stats outside a huddle: %q", m.notice)
		}
		runInput(t, m, "/huddle")
		runInput(t, m, "/huddle")
		if !strings.HasPrefix(m.notice, "voice via tcp · echo cancellation on (system)") {
			t.Fatalf("/huddle in a TCP huddle: %q", m.notice)
		}
		runInput(t, m, "/huddle stats")
		want := "tcp · loss 0.4% · buf 60ms · 3 late · 32 kb/s"
		if m.noticeErr || m.notice != want {
			t.Fatalf("stats %q", m.notice)
		}
		if v := checkView(t, m, "stats"); !strings.Contains(v, "loss 0.4%") || !strings.Contains(v, "tcp") {
			t.Fatalf("loss not visible at %d columns:\n%s", w, v)
		}
		v := checkView(t, m, "stats notice")
		for _, l := range strings.Split(v, "\n") {
			if lipgloss.Width(l) > w {
				t.Fatalf("line wider than %d: %q", w, l)
			}
		}
		sf.st = client.HuddleStats{FEC: -1}
		runInput(t, m, "/huddle stats")
		if m.notice != "udp · loss 0.0%" {
			t.Fatalf("fresh UDP stats %q", m.notice)
		}
		runInput(t, m, "/huddle stat")
		if !m.noticeErr || !strings.Contains(m.notice, "usage: /huddle [stats]") {
			t.Fatalf("typo: %q", m.notice)
		}
	}
}
