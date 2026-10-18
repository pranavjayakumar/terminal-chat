package tui

import (
	"strings"
	"testing"

	"github.com/skailama/tc/internal/client"
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

// Typing /dm @me (or my own name) and Enter must open my notes, never
// complete to another person whose name contains "me".
func TestDMMeThroughPopup(t *testing.T) {
	for _, in := range []string{"/dm @me", "/dm @alice", "/dm @Me"} {
		m, f := newTestModelFake(t, 100, 30)
		f.users = append(f.users, proto.User{ID: 77, Name: "james"}, proto.User{ID: 78, Name: "alicex"})
		m.reloadUsers()
		typeKeys(m, in)
		for i := 0; i < 3 && m.input.Value() != ""; i++ {
			run(m, m.handleKey(keyMsg("enter")))
		}
		if !f.called("dm:[]") {
			t.Fatalf("%s: calls %v, input %q", in, f.calls, m.input.Value())
		}
	}
	// In a plain message @me isn't offered: it means nothing there.
	m, _ := newTestModelFake(t, 100, 30)
	typeKeys(m, "hi @m")
	for _, it := range m.comp.items {
		if it.text == "@me" {
			t.Fatal("@me offered outside /dm")
		}
	}
}

// Ctrl-K finds existing notes by "me" too.
func TestSwitcherNotesByMe(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	f.chans = append(f.chans, client.ChannelView{Channel: proto.Channel{ID: 30, Kind: proto.ChanDM, Members: []uint64{1}}, Display: "alice (notes)", Member: true})
	m.reloadChans()
	m.handleKey(keyMsg("ctrl+k"))
	for _, r := range "me" {
		m.handleKey(keyMsg(string(r)))
	}
	if len(m.sw.shown) == 0 || m.sw.shown[0].ch != 30 {
		t.Fatalf("shown %+v", m.sw.shown)
	}
}
