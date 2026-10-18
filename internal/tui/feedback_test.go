package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/proto"
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
		shown := m.notice
		if m.mode == modeList { // a long topic opens in the list, wrapped
			shown = strings.Join(m.list.rows, " ")
			press(m, "esc")
		}
		if !strings.Contains(shown, in+" ") {
			t.Fatalf("%s: shown %q", in, shown)
		}
	}
}

// A bare server error code reads as words, not as "not_found".
func TestErrorCodesReadable(t *testing.T) {
	m := newTestModel(t, 100, 30)
	for code, want := range map[string]string{
		proto.ErrNotFound:    "x: not found",
		proto.ErrForbidden:   "x: not allowed",
		proto.ErrRateLimited: "x: too many requests — wait a moment",
	} {
		m.setErr("x", &proto.Error{Code: code})
		if m.notice != want {
			t.Errorf("%s: %q, want %q", code, m.notice, want)
		}
		if got := errText(&proto.Error{Code: code}); got != strings.TrimPrefix(want, "x: ") {
			t.Errorf("errText %s: %q", code, got)
		}
	}
	m.setErr("x", &proto.Error{Code: proto.ErrForbidden, Msg: "only admins"})
	if m.notice != "x: not allowed: only admins" {
		t.Errorf("with a message: %q", m.notice)
	}
}

// /notify and /sounds list every setting on its own row, so nothing is cut
// at 80 columns, and no usage text points at "?" (which is just text).
func TestNotifySettingsReadable(t *testing.T) {
	m := newTestModel(t, 80, 24)
	submitText(m, "/notify")
	v := checkView(t, m, "/notify")
	for _, want := range []string{"#general: mentions (default)", "default: mentions", "content: full", "quiet hours: off", "do not disturb: off", "sounds: on"} {
		if !strings.Contains(v, want) {
			t.Errorf("/notify at 80 cols lacks %q:\n%s", want, v)
		}
	}
	press(m, "esc")
	submitText(m, "/sounds")
	v = checkView(t, m, "/sounds")
	for _, want := range []string{"sounds: on", "mention: on", "mic: on", "huddle cue volume: 60"} {
		if !strings.Contains(v, want) {
			t.Errorf("/sounds at 80 cols lacks %q:\n%s", want, v)
		}
	}
	press(m, "esc")
	for _, in := range []string{"/notify bogus", "/sounds bogus on", "/sounds on"} {
		submitText(m, in)
		if strings.Contains(m.notice, "?") || strings.Contains(m.notice, "popups still show") && in == "/sounds on" {
			t.Errorf("%s: %q", in, m.notice)
		}
		press(m, "ctrl+u")
	}
}

// An empty channel says so and how to get around, instead of a blank pane
// (what a brand-new user sees first).
func TestEmptyChannelHint(t *testing.T) {
	for _, sz := range [][2]int{{120, 35}, {80, 24}, {40, 12}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		f.msgs[11] = nil
		m.histDone[11] = true
		run(m, m.openChannel(11))
		v := checkView(t, m, "empty")
		if !strings.Contains(v, "no messages yet") || sz[0] >= 80 && !strings.Contains(v, "F1 help") {
			t.Fatalf("%v: no hint:\n%s", sz, v)
		}
		m.histDone[11] = false
		m.histBusy[11] = true
		if v := checkView(t, m, "loading"); strings.Contains(v, "no messages yet") {
			t.Fatalf("hint while history loads:\n%s", v)
		}
	}
}

// Review follow-ups: a replaced Ctrl-C hint disarms; a failed history
// fetch doesn't claim the channel is empty; hidden list rows are counted;
// a partly failed /add says who got in.
func TestReviewFollowUps(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	m.handleKey(keyMsg("ctrl+c"))
	m.setInfo("added @bob")
	if cmd := m.handleKey(keyMsg("ctrl+c")); cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("quit after the hint was replaced")
		}
	}

	f.msgs[11] = nil
	run(m, m.openChannel(11))
	m.histBusy[11], m.histDone[11] = false, false
	if v := checkView(t, m, "unknown history"); strings.Contains(v, "no messages yet") {
		t.Fatal("hint shown while history is unknown")
	}

	m2 := newTestModel(t, 40, 8)
	m2.showList("t", []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"})
	if v := checkView(t, m2, "list"); !strings.Contains(v, "more") {
		t.Fatalf("no more marker:\n%s", v)
	}

	m3, f3 := newTestModelFake(t, 100, 30)
	f3.addErrAfter = 1
	f3.addErr = &proto.Error{Code: proto.ErrForbidden}
	submitText(m3, "/add @bob @carol")
	if !strings.Contains(m3.notice, "added @bob; @carol: not allowed") {
		t.Fatalf("partial add: %q", m3.notice)
	}
}
