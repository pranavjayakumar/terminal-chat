package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
)

func setFakeMsg(f *fakeClient, ch uint64, x client.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.msgs[ch] {
		if f.msgs[ch][i].ID == x.ID {
			f.msgs[ch][i] = x
			return
		}
	}
	f.msgs[ch] = append(f.msgs[ch], x)
}

// A message that arrives inside a channel's coalescing window is held in
// m.notes and shown when the window closes. If its author deletes it in
// the meantime (the classic "pasted a password into the DM, deleted it at
// once"), the pending popup still fires with the deleted text: nothing
// drops or rechecks the pending note when a message is deleted. The popup
// then sits in Notification Center / the GNOME tray history.
func TestNotify_BreakDeletedMessageStillPopsUp(t *testing.T) {
	t.Skip("issue #97")
	m, f := newTestModelFake(t, 100, 24)
	clk := newClock(m)
	first := newMsg(501, 20, 2, "hey")
	setFakeMsg(f, 20, client.Message{Message: first})
	deliver(m, first)
	gen := m.notes[20].gen
	clk.add(time.Second)
	secret := newMsg(502, 20, 2, "prod db password: hunter2")
	setFakeMsg(f, 20, client.Message{Message: secret})
	deliver(m, secret)

	// bob deletes it a second later; the client reports the channel changed.
	secret.Deleted, secret.Body = true, ""
	setFakeMsg(f, 20, client.Message{Message: secret})
	run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{20: true}}))

	clk.add(notifyWindow)
	m.Update(notifyFlushMsg{ch: 20, gen: gen})
	for _, n := range notes(m).sent() {
		if strings.Contains(n.Body, "hunter2") || strings.Contains(n.Title, "hunter2") {
			t.Fatalf("a deleted message's text was shown in a popup: %+v", n)
		}
	}
}

// Same window, but the message is edited before the window closes: the
// popup shows the pre-edit text (e.g. a typo'd secret the author fixed).
func TestNotify_BreakEditedMessagePopsUpOldText(t *testing.T) {
	t.Skip("issue #97")
	m, f := newTestModelFake(t, 100, 24)
	clk := newClock(m)
	first := newMsg(511, 20, 2, "hey")
	setFakeMsg(f, 20, client.Message{Message: first})
	deliver(m, first)
	gen := m.notes[20].gen
	clk.add(time.Second)
	x := newMsg(512, 20, 2, "token ghp_LEAKED123")
	setFakeMsg(f, 20, client.Message{Message: x})
	deliver(m, x)
	x.Body, x.Edited = "token [redacted]", clk.t.UnixMilli()
	setFakeMsg(f, 20, client.Message{Message: x})
	run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{20: true}}))

	clk.add(notifyWindow)
	m.Update(notifyFlushMsg{ch: 20, gen: gen})
	for _, n := range notes(m).sent() {
		if strings.Contains(n.Body, "ghp_LEAKED123") {
			t.Fatalf("popup shows text the author already edited away: %+v", n)
		}
	}
}

// Highlight words deliberately don't match inside fenced code blocks
// (commit 711df6e: "pasted logs and diffs lit up on every keyword"), but
// @mentions still do: a pasted package.json or stack trace containing
// "@alice/..." pops up a mention notification with the mention sound.
func TestNotify_BreakMentionInsideCodeBlockNotifies(t *testing.T) {
	t.Skip("issue #100")
	m, _ := newTestModelFake(t, 100, 24)
	newClock(m)
	deliver(m, newMsg(521, 11, 2, "deps:\n```\n\"@alice/utils\": \"^1.2.0\",\n\"@babel/core\": \"7\"\n```"))
	if s := notes(m).sent(); len(s) != 0 {
		t.Fatalf("an @name inside a fenced code block notified as a mention: %+v", s)
	}
}

// "A mention never waits out a level-all channel's 30 s window: it opens
// its own (the soft messages before it are folded in)." They aren't: the
// pending note (with its count) is deleted and the new window starts at
// zero, so the soft messages counted before the mention are never
// announced — neither in the mention popup nor in any summary.
func TestNotify_BreakMentionDropsCountedLevelAllMessages(t *testing.T) {
	t.Skip("issue #100")
	m, _ := newTestModelFake(t, 100, 24)
	clk := newClock(m)
	runInput(t, m, "/notify all #secret")
	deliver(m, newMsg(531, 13, 3, "one"))
	clk.add(time.Second)
	deliver(m, newMsg(532, 13, 3, "two"), newMsg(533, 13, 3, "three"))
	if m.notes[13] == nil || m.notes[13].n != 2 {
		t.Fatalf("setup: want 2 counted messages, got %+v", m.notes[13])
	}
	clk.add(3 * time.Second)
	deliver(m, newMsg(534, 13, 3, "hey @alice"))
	gen := m.notes[13].gen
	clk.add(allWindow)
	m.Update(notifyFlushMsg{ch: 13, gen: gen})
	s := notes(m).sent()
	// 4 messages: one popup for "one", then something must account for
	// "two" and "three" (a count, or a summary).
	for _, n := range s[1:] {
		if strings.Contains(n.Body, "new messages") {
			return
		}
	}
	t.Fatalf("messages two and three were never announced; popups: %+v", s)
}

// The global cap ("1 notification per 2 s applies to cued popups",
// NOTIFICATIONS §2.5 rule 3) only covers messages: channel huddle-start
// popups neither check nor update lastNotify. A burst of huddle starts in
// several level-all channels (or a server replaying them) fires one cued
// popup per channel at the same instant, on top of a message popup.
func TestNotify_BreakChannelHuddlePopupsBypassGlobalCap(t *testing.T) {
	t.Skip("issue #100")
	m, h := newHuddleModel(t, 100, 24)
	newClock(m)
	runInput(t, m, "/notify global all")
	deliver(m, newMsg(541, 11, 2, "hey @alice"))
	before := len(notes(m).sent())
	if before != 1 {
		t.Fatalf("setup: %d popups", before)
	}
	h.hmu.Lock()
	h.rosters[11] = []uint64{2}
	h.rosters[13] = []uint64{3}
	h.hmu.Unlock()
	m.Update(updateMsg(client.Update{Huddle: true, HuddleStarted: []client.HuddleStart{{Chan: 11, By: 2}, {Chan: 13, By: 3}}}))
	cued := 0
	for _, n := range notes(m).sent() {
		if n.Cue != notify.CueNone {
			cued++
		}
	}
	if cued > 1 {
		t.Fatalf("%d cued popups in the same instant (cap: 1 per %v): %+v", cued, notifyEvery, notes(m).sent())
	}
}
