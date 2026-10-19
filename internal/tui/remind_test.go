package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

const notesChan = 21

// withNotes gives the fake a private notes DM (only alice) and reloads.
func withNotes(m *Model, f *fakeClient) {
	f.chans = append(f.chans, client.ChannelView{
		Channel: proto.Channel{ID: notesChan, Kind: proto.ChanDM, Members: []uint64{1}},
		Display: "alice (notes)", Member: true,
	})
	m.reloadChans()
}

func TestRemindCommandSchedules(t *testing.T) {
	m, f := newTestModelFake(t, 100, 24)
	newClock(m) // 2026-10-09 12:00 UTC
	runInput(t, m, "/remind 10m call mom")
	if len(f.reminded) != 1 {
		t.Fatalf("calls %v", f.calls)
	}
	got := f.reminded[0]
	if !got.at.Equal(time.Date(2026, 10, 9, 12, 10, 0, 0, time.UTC)) || got.text != "call mom" || got.ch != m.active {
		t.Fatalf("remind call %+v (active %d)", got, m.active)
	}
	if m.noticeErr || !strings.Contains(m.notice, "reminder 1 set for 12:10") || m.input.Value() != "" {
		t.Fatalf("notice %q err=%v input=%q", m.notice, m.noticeErr, m.input.Value())
	}

	runInput(t, m, "/remind tomorrow 9am stand-up  notes")
	got = f.reminded[1]
	if !got.at.Equal(time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)) || got.text != "stand-up  notes" {
		t.Fatalf("tomorrow: %+v", got)
	}
	if !strings.Contains(m.notice, "set for tomorrow 09:00") {
		t.Fatalf("notice %q", m.notice)
	}

	// 11:00 has passed today, so it means tomorrow.
	runInput(t, m, "/remind 11:00 lunch")
	if got = f.reminded[2]; !got.at.Equal(time.Date(2026, 10, 10, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("clock time in the past: %+v", got)
	}
}

func TestRemindFromNotesAttachesNoChannel(t *testing.T) {
	m, f := newTestModelFake(t, 100, 24)
	newClock(m)
	withNotes(m, f)
	m.openChannel(notesChan)
	runInput(t, m, "/remind 1h water the plants")
	if len(f.reminded) != 1 || f.reminded[0].ch != 0 {
		t.Fatalf("in notes: %+v", f.reminded)
	}
	// A DM with another person does carry its channel.
	m.openChannel(20)
	runInput(t, m, "/remind 1h ask bob")
	if f.reminded[1].ch != 20 {
		t.Fatalf("in a DM: %+v", f.reminded[1])
	}
}

func TestRemindCommandErrorsKeepTheInput(t *testing.T) {
	m, f := newTestModelFake(t, 100, 24)
	newClock(m)
	for _, tc := range []struct{ line, want string }{
		{"/remind", "when?"},
		{"/remind soon call", "when?"},
		{"/remind 10m", "about what"},
		{"/remind tomorrow call mom", "needs a time"},
		{"/remind 25:00 x", "when?"},
		{"/remind 0m x", "future"},
		{"/remind 999d x", "a year"},
	} {
		m.input.SetValue(tc.line)
		press(m, "enter")
		if !m.noticeErr || !strings.Contains(m.notice, tc.want) || m.input.Value() != tc.line {
			t.Errorf("%q: err=%v notice=%q input=%q, want %q", tc.line, m.noticeErr, m.notice, m.input.Value(), tc.want)
		}
	}
	if len(f.reminded) != 0 {
		t.Fatalf("a rejected command reached the backend: %+v", f.reminded)
	}

	// Server-side refusals are shown, not swallowed.
	m.input.Reset()
	f.remindErr = proto.Errf(proto.ErrConflict, "too many pending reminders")
	runInput(t, m, "/remind 1h x")
	if !m.noticeErr || !strings.Contains(m.notice, "too many pending reminders") {
		t.Fatalf("notice %q err=%v", m.notice, m.noticeErr)
	}
	f.remindErr = client.ErrOffline
	runInput(t, m, "/remind 1h x")
	if !m.noticeErr || !strings.Contains(m.notice, "offline") {
		t.Fatalf("offline notice %q", m.notice)
	}
	f.remindErr = errors.New("this server doesn't support reminders")
	runInput(t, m, "/remind 1h x")
	if !strings.Contains(m.notice, "doesn't support reminders") {
		t.Fatalf("unsupported notice %q", m.notice)
	}
}

func TestRemindersOverlayAndDelete(t *testing.T) {
	m, f := newTestModelFake(t, 100, 24)
	newClock(m)
	runInput(t, m, "/reminders")
	if m.mode == modeList || !strings.Contains(m.notice, "no reminders") {
		t.Fatalf("empty: mode %v notice %q", m.mode, m.notice)
	}

	runInput(t, m, "/remind 10m call mom")
	runInput(t, m, "/remind tomorrow 9am line one\nline two \x1b]0;pwned\a")
	f.reminders[1].Text = "line one\nline two \x1b]0;pwned\a"
	runInput(t, m, "/reminders")
	if m.mode != modeList {
		t.Fatalf("mode %v notice %q", m.mode, m.notice)
	}
	v := checkView(t, m, "reminders")
	for _, want := range []string{"2 reminders", "1  12:10  call mom (from #general)", "2  tomorrow 09:00  line one …"} {
		if !strings.Contains(v, want) {
			t.Errorf("overlay lacks %q:\n%s", want, v)
		}
	}
	for _, l := range strings.Split(m.View(), "\n") {
		if strings.ContainsAny(l, "\a") || strings.Contains(l, "\x1b]") {
			t.Errorf("terminal escape reached the screen: %q", l)
		}
	}
	press(m, "esc")

	runInput(t, m, "/remind-delete 1")
	if !f.called("remind-delete:1") || !strings.Contains(m.notice, "reminder 1 deleted") {
		t.Fatalf("delete: calls %v notice %q", f.calls, m.notice)
	}
	runInput(t, m, "/remind-delete #2")
	if len(f.reminders) != 0 {
		t.Fatalf("left %v", f.reminders)
	}
	runInput(t, m, "/remind-delete 2")
	if !m.noticeErr || !strings.Contains(m.notice, "remind-delete: not found") {
		t.Fatalf("unknown id: %q err=%v", m.notice, m.noticeErr)
	}
	for _, line := range []string{"/remind-delete", "/remind-delete x", "/remind-delete 0", "/remind-delete 1 2"} {
		m.input.SetValue(line)
		press(m, "enter")
		if !m.noticeErr || !strings.Contains(m.notice, "usage") || m.input.Value() != line {
			t.Errorf("%q: %q err=%v input=%q", line, m.notice, m.noticeErr, m.input.Value())
		}
	}
	for _, size := range [][2]int{{20, 6}, {40, 8}, {10, 4}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		checkView(t, m, "small")
	}
}

func TestReminderNotifiesEvenThoughItIsMine(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	clk := newClock(m)
	nt := notes(m)
	withNotes(m, f)
	want := [][2]string{}
	check := func(step string) {
		t.Helper()
		if got := nt.got(); !slices.Equal(got, want) {
			t.Fatalf("%s:\n got %q\nwant %q", step, got, want)
		}
	}

	// The reminder the server posted into my notes.
	deliver(m, newMsg(401, notesChan, 1, "⏰ call mom (from #general)"))
	want = append(want, [2]string{"⏰ Reminder", "call mom (from #general)"})
	check("reminder")

	// Everything else of mine stays silent.
	clk.add(10 * time.Second)
	deliver(m,
		newMsg(402, notesChan, 1, "buy milk"),                   // a plain note to myself
		newMsg(403, 11, 1, "⏰ not in my notes"),                 // my message elsewhere
		newMsg(404, 20, 1, "⏰ in a DM with bob"),                // two-person DM
		newMsg(405, notesChan, 2, "⏰ someone else's lookalike"), // not mine (can't happen in notes)
	)
	check("lookalikes")
	var bot proto.Message = newMsg(406, notesChan, 9, "⏰ from a bot")
	f.users = append(f.users, proto.User{ID: 9, Name: "ci-bot", Bot: true})
	m.reloadUsers()
	deliver(m, bot)
	check("bot")

	// Looking at the notes with the window focused: the log shows it.
	clk.add(10 * time.Second)
	m.openChannel(notesChan)
	deliver(m, newMsg(407, notesChan, 1, "⏰ visible already"))
	check("active and focused")

	// The notes are open but the window isn't focused: notify.
	m.Update(tea.BlurMsg{})
	clk.add(10 * time.Second)
	deliver(m, newMsg(408, notesChan, 1, "⏰ multi\nline"))
	want = append(want, [2]string{"⏰ Reminder", "multi"})
	check("blurred")

	// A stored hostile body is sanitized in the notification.
	clk.add(10 * time.Second)
	deliver(m, newMsg(409, notesChan, 1, "⏰ evil \x1b]0;pwned\a text"))
	got := nt.got()
	if n := len(got); n != 2 {
		// Coalesced into the open window for this channel; flush it.
		m.Update(notifyFlushMsg{ch: notesChan, gen: m.notes[notesChan].gen})
		got = nt.got()
	}
	for _, g := range got {
		if strings.ContainsAny(g[0]+g[1], "\x1b\a") {
			t.Fatalf("control characters in a notification: %q", g)
		}
	}
}

func TestReminderNotificationsCoalesce(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	clk := newClock(m)
	nt := notes(m)
	withNotes(m, f)
	deliver(m, newMsg(401, notesChan, 1, "⏰ one"))
	gen := m.notes[notesChan].gen
	clk.add(time.Second)
	deliver(m, newMsg(402, notesChan, 1, "⏰ two"), newMsg(403, notesChan, 1, "⏰ three"))
	if n := len(nt.got()); n != 1 {
		t.Fatalf("%d notifications inside the window", n)
	}
	clk.add(6 * time.Second)
	m.Update(notifyFlushMsg{ch: notesChan, gen: gen})
	got := nt.got()
	if len(got) != 2 || got[1] != [2]string{"⏰ Reminder", "2 new messages"} {
		t.Fatalf("summary %q", got)
	}
}

func TestReminderCountsAsUnreadInTheSidebar(t *testing.T) {
	m, f := newTestModelFake(t, 100, 24)
	withNotes(m, f)
	f.chans[len(f.chans)-1].Unread = 1
	f.chans[len(f.chans)-1].Mention = true
	m.reloadChans()
	v := checkView(t, m, "sidebar")
	if !strings.Contains(v, "alice (notes)") {
		t.Fatalf("notes missing from the sidebar:\n%s", v)
	}
}

// Encrypted channels refuse (the server stores reminders in the clear), say
// why, and keep the typed command.
func TestRemindInEncryptedChannelRefuses(t *testing.T) {
	m, f := newTestModelFake(t, 100, 24)
	newClock(m)
	m.openChannel(20)
	m.e2e[20] = client.E2EStatus{State: client.E2EOK}
	m.input.SetValue("/remind 1h ask bob")
	run(m, m.submit())
	if len(f.reminded) != 0 || !m.noticeErr || !strings.Contains(m.notice, "stored unencrypted") {
		t.Fatalf("reminded %+v notice %q", f.reminded, m.notice)
	}
	if m.input.Value() != "/remind 1h ask bob" {
		t.Fatalf("input lost: %q", m.input.Value())
	}
}
