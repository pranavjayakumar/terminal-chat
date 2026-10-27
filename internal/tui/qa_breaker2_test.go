package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// A history fetch in flight when I leave a channel answers after the leave:
// its "no more" must not mark the (now empty) cache complete, or a rejoin
// skips the refetch exactly like #74.
func TestHistoryReplyAfterLeave_Break(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	m.status = f.status
	cmd := m.openChannel(11) // backend has < 30 messages: a fetch starts
	if !m.histBusy[11] {
		t.Fatal("setup: no history fetch for #backend")
	}
	// Leave while the fetch is in flight: the cache for #backend is dropped.
	f.chans[0].Member = false
	f.msgs[11] = nil
	m.Update(updateMsg(client.Update{Channels: true}))
	run(m, m.openChannel(10))
	// The in-flight request now answers "that's everything".
	run(m, cmd)
	// Rejoin.
	f.chans[0].Member = true
	m.Update(updateMsg(client.Update{Channels: true}))
	before := f.history
	run(m, m.openChannel(11))
	if f.history == before {
		t.Fatal("rejoined channel's history not fetched: a reply that landed after the leave marked it complete")
	}
}

// #93 again, with the removal landing while history is loading: the
// server's forbidden answer must not surface as an error.
func TestHistoryErrorAfterRemoval_Break(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	m.status = f.status
	f.msgs[13] = nil
	cmd := m.openChannel(13)
	if !m.histBusy[13] {
		t.Fatal("setup: no history fetch for #secret")
	}
	for i := range f.chans {
		if f.chans[i].ID == 13 {
			f.chans[i].Member, f.chans[i].Removed = false, true
		}
	}
	m.Update(updateMsg(client.Update{Channels: true}))
	f.histErr = &proto.Error{Code: proto.ErrForbidden}
	run(m, cmd)
	if m.noticeErr {
		t.Fatalf("removed channel shows an error: %q", m.notice)
	}
}

// The second Ctrl-C quits only while the hint is on screen. An async
// openChannel (here: a /dm result) that cancels an edit clears the hint via
// clearNotice, but the arm stays: a Ctrl-C then quits with no hint shown.
func TestCtrlCArmOutlivesClearedHint_Break(t *testing.T) {
	m, _ := newTestModelFake(t, 80, 24)
	now := time.Now()
	m.now = func() time.Time { return now }
	press(m, "up")
	for i, x := range m.main.msgs {
		if x.ID == 103 {
			m.main.sel = i
		}
	}
	press(m, "e")
	if m.editing != 103 {
		t.Fatalf("setup: not editing (%d)", m.editing)
	}
	m.handleKey(keyMsg("ctrl+c"))
	if m.notice != m.kn.of(ctrlCHint) {
		t.Fatalf("setup: notice %q", m.notice)
	}
	m.Update(openChanMsg{ch: 20}) // e.g. a /dm typed earlier answers now
	if m.notice == m.kn.of(ctrlCHint) {
		t.Skip("hint still up")
	}
	now = now.Add(time.Second)
	if cmd := m.handleKey(keyMsg("ctrl+c")); cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatalf("Ctrl-C quit while the hint was gone (notice %q)", m.notice)
		}
	}
}

// The webhook's curl command is shown once. A slower command's result
// (/webhooks, /search...) arriving while the secret viewer is up replaces
// it: the URL can never be seen again, yet it lingers in m.view.
func TestHookViewerStolenByAsyncList_Break(t *testing.T) {
	m, _ := newHookModel(t, 100, 30)
	m.openChannel(10)
	m.Update(hookShownMsg{name: "ci-bot", curl: fakeCurl})
	if m.mode != modeViewer {
		t.Fatalf("setup: mode %v", m.mode)
	}
	if strings.Contains(plain(m), fakeURL) {
		t.Fatal("URL on screen before s")
	}
	m.Update(listMsg{title: "1 webhook", rows: []string{"5 ci → #general"}})
	press(m, "esc")
	if m.mode == modeNormal && strings.Contains(m.view.copy, fakeURL) {
		t.Fatalf("secret viewer was replaced unseen and its URL lingers (mode %v)", m.mode)
	}
}

// Same race through a snapshot Reset: Reset clears histDone, but a reply
// from before the reset (for the old cache) lands after and marks the new
// cache complete, so its history is never fetched.
func TestHistoryReplyAfterReset_Break(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	m.status = f.status
	cmd := m.openChannel(11)
	if !m.histBusy[11] {
		t.Fatal("setup: no history fetch for #backend")
	}
	f.msgs[11] = nil // the snapshot replaced the cache
	m.Update(updateMsg(client.Update{Reset: true}))
	run(m, cmd) // the pre-reset request answers "no more"
	run(m, m.openChannel(10))
	m.openChannel(11) // the fetch it starts is what counts, not its timing
	if !m.histBusy[11] {
		t.Fatal("after a reset, a stale history reply marked the new cache complete: no refetch")
	}
}

// If my own account is named "me", /dm @me offers the same "@me" twice.
func TestDMCompMeTwice_Break(t *testing.T) {
	f := newFake() // renamed before the model starts reading it
	f.me.Name = "me"
	f.users[0].Name = "me"
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.input.SetValue("/dm @m")
	m.input.CursorEnd()
	m.refreshComp()
	n := 0
	for _, it := range m.comp.items {
		if it.text == "@me" {
			n++
		}
	}
	if n > 1 {
		t.Fatalf("@me offered %d times: %+v", n, m.comp.items)
	}
}
