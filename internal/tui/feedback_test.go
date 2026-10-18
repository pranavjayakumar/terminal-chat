package tui

import (
	"strings"
	"testing"
)

// Commands that change something say so; a bare /topic or /name doesn't
// silently wipe the topic or display name.
func TestCommandFeedback(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	for _, tc := range []struct{ in, call, notice string }{
		{"/topic deploys only", "topic deploys only", "topic set"},
		{"/topic --clear", "topic ", "topic cleared"},
		{"/name Alice L.", "name:Alice L.", "display name: Alice L."},
		{"/name --clear", "name:", "display name cleared"},
		{"/away lunch", "away", "away: lunch"},
		{"/back", "", "back"},
		{"/add @carol", "", "added @carol"},
	} {
		m.clearNotice()
		submitText(m, tc.in)
		if m.input.Value() != "" || m.noticeErr {
			t.Fatalf("%s: input %q notice %q", tc.in, m.input.Value(), m.notice)
		}
		if tc.call != "" && !f.called(tc.call) {
			t.Fatalf("%s: %q not called: %v", tc.in, tc.call, f.calls)
		}
		if !strings.Contains(m.notice, tc.notice) {
			t.Fatalf("%s: notice %q, want %q", tc.in, m.notice, tc.notice)
		}
	}
	// Bare /topic and /name show the current value and how to change it,
	// and change nothing.
	n := len(f.calls)
	for _, in := range []string{"/topic", "/name"} {
		submitText(m, in)
		if len(f.calls) != n {
			t.Fatalf("%s called %v", in, f.calls[n:])
		}
		if !strings.Contains(m.notice, in+" ") {
			t.Fatalf("%s: notice %q", in, m.notice)
		}
	}
}
