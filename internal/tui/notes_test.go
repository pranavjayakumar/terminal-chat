package tui

import (
	"strings"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

// My notes (the DM with only me, where reminders land) open with /dm @me,
// /dm @<my name> and from Ctrl-K, before any reminder created them.
func TestOpenNotes(t *testing.T) {
	for _, in := range []string{"/dm @me", "/dm @alice", "/dm me"} {
		m, f := newTestModelFake(t, 100, 30)
		m.input.SetValue(in)
		run(m, m.handleKey(keyMsg("enter")))
		if m.noticeErr {
			t.Fatalf("%s: error %q", in, m.notice)
		}
		if !f.called("dm:[]") {
			t.Fatalf("%s: notes not opened, calls %v", in, f.calls)
		}
	}

	m, f := newTestModelFake(t, 100, 30)
	m.handleKey(keyMsg("ctrl+k"))
	for _, r := range "notes" {
		m.handleKey(keyMsg(string(r)))
	}
	if len(m.sw.shown) == 0 || !strings.Contains(m.sw.shown[0].label, "alice (notes)") {
		t.Fatalf("Ctrl-K notes: shown %+v", m.sw.shown)
	}
	checkView(t, m, "ctrl-k notes")
	run(m, m.handleKey(keyMsg("enter")))
	if !f.called("dm:[]") {
		t.Fatalf("Ctrl-K notes: not opened, calls %v", f.calls)
	}
}

// An account named "me" can't catch /dm @me: that is always my notes.
func TestDMMeNeverAnotherUser(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	f.users = append(f.users, proto.User{ID: 77, Name: "me"})
	m.reloadUsers()
	m.input.SetValue("/dm @me")
	run(m, m.handleKey(keyMsg("enter")))
	if !f.called("dm:[]") || f.called("dm:[77]") {
		t.Fatalf("calls %v", f.calls)
	}
}
