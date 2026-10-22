package tui

import (
	"fmt"
	"slices"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/alert"
	"github.com/skailama/tc/internal/background"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// The chat window (unfocused, so it looks at nothing) and tc background
// get the same updates, settings and clock: they must show exactly the
// same notifications, in the same order, with the same sounds and tags.
func TestBackgroundParity(t *testing.T) {
	m, h := newHuddleModel(t, 100, 30)
	clk := newClock(m)
	m.Update(tea.BlurMsg{})
	bgOut := &fakeNotifier{}
	core := background.NewCore(h, bgOut, m.opt.NotifyEnv, func() bool { return false }, clk.now)

	compare := func(what string) {
		t.Helper()
		a, b := notes(m).sent(), bgOut.sent()
		if !slices.Equal(a, b) {
			t.Fatalf("%s: chat window and background differ\n ui: %+v\n bg: %+v", what, a, b)
		}
	}
	var flush func()
	// check compares, closes the windows still open (their timers would
	// have fired by the next step), and compares again.
	check := func(what string) {
		t.Helper()
		compare(what)
		flush()
		compare(what + ", windows closed")
	}
	both := func(u client.Update) {
		t.Helper()
		m.Update(updateMsg(u))
		core.Handle(u)
	}
	msgs := func(xs ...client.Message) { both(client.Update{New: xs}) }
	msg := func(id, ch, author uint64, body string) client.Message {
		return client.Message{Message: proto.Message{ID: id, Chan: ch, Author: author, Body: body, Time: clk.now().UnixMilli()}}
	}
	// flush closes every open coalescing window in both, in channel order.
	flush = func() {
		for range 4 {
			var chans []uint64
			for ch := range m.al.Notes {
				chans = append(chans, ch)
			}
			slices.Sort(chans)
			for _, ch := range chans {
				m.Update(notifyFlushMsg{ch: ch, gen: m.al.Notes[ch].Gen})
			}
			chans = chans[:0]
			for ch := range core.Eng.Notes {
				chans = append(chans, ch)
			}
			slices.Sort(chans)
			for _, ch := range chans {
				core.Fire(alert.Timer{Kind: alert.FlushTimer, Ch: ch, Gen: core.Eng.Notes[ch].Gen})
			}
			clk.add(3 * time.Second)
		}
	}
	setting := func(line string) {
		t.Helper()
		runInput(t, m, line)
		if m.noticeErr {
			t.Fatalf("%s: %s", line, m.notice)
		}
		core.Handle(client.Update{Prefs: true})
	}

	msgs(msg(501, 20, 2, "hi there"))
	check("DM")
	clk.add(10 * time.Second)
	msgs(msg(502, 11, 3, "hey @alice"), msg(503, 11, 3, "@alice again"), msg(504, 11, 3, "and again @alice"))
	check("mentions, coalesced")
	clk.add(6 * time.Second)
	flush()
	check("window closed")

	setting("/notify all #general")
	clk.add(10 * time.Second)
	msgs(msg(505, 10, 2, "plain chatter"))
	clk.add(time.Second)
	msgs(msg(506, 10, 3, "more chatter"), msg(507, 10, 2, "@alice in a level-all channel"))
	check("level all, then a mention")
	clk.add(31 * time.Second)
	flush()
	check("level-all window")

	clk.add(10 * time.Second)
	own := msg(508, 11, 1, "@alice my own")
	deleted := msg(509, 11, 2, "@alice gone")
	deleted.Deleted = true
	inserted := msg(510, 20, 2, "server says hi")
	inserted.Sec = client.SecInserted
	msgs(own, deleted, inserted, msg(511, 11, 2, "nothing for me"))
	check("ignored")

	runInput(t, m, "/highlight deploy")
	clk.add(10 * time.Second)
	msgs(msg(512, 11, 2, "deploy is done"))
	check("highlight")
	h.mu.Lock()
	h.myThreads = map[uint64]bool{103: true}
	h.mu.Unlock()
	clk.add(10 * time.Second)
	reply := msg(513, 10, 3, "replying in your thread")
	reply.Thread = 103
	msgs(reply)
	check("reply in my thread")

	setting("/notify content name")
	clk.add(10 * time.Second)
	msgs(msg(514, 20, 2, "secret plans"))
	check("content name")
	setting("/notify content none")
	clk.add(10 * time.Second)
	msgs(msg(515, 20, 2, "more secret plans"))
	check("content none")
	setting("/notify content full")

	setting("/dnd 1h")
	clk.add(10 * time.Second)
	msgs(msg(516, 20, 2, "during dnd"))
	check("dnd")
	setting("/dnd off")

	now := clk.now().Local()
	quiet := fmt.Sprintf("%02d:%02d-%02d:%02d", now.Add(-time.Hour).Hour(), 0, now.Add(2*time.Hour).Hour(), 0)
	setting("/notify quiet " + quiet)
	clk.add(10 * time.Second)
	msgs(msg(517, 20, 2, "during quiet hours"))
	check("quiet hours")
	setting("/notify quiet off")

	setting("/notify off #backend for 2h")
	clk.add(10 * time.Second)
	msgs(msg(518, 11, 3, "@alice muted"))
	check("timed mute")
	clk.add(3 * time.Hour)
	msgs(msg(519, 11, 3, "@alice unmuted"))
	check("mute lapsed")

	setting("/sounds off")
	clk.add(10 * time.Second)
	msgs(msg(520, 20, 2, "silent"))
	check("sounds off")
	setting("/sounds on")

	// Huddles: a DM rings (and is resent), a level-all channel pops up.
	clk.add(10 * time.Second)
	h.hmu.Lock()
	h.rosters[20] = []uint64{2}
	h.rosters[10] = []uint64{3}
	h.hmu.Unlock()
	both(client.Update{Huddle: true, HuddleStarted: []client.HuddleStart{{Chan: 20, By: 2}}})
	check("ring")
	m.Update(ringMsg{gen: m.al.Ring.Gen, resend: true})
	core.Fire(alert.Timer{Kind: alert.RingResendT, Gen: core.Eng.Ring.Gen})
	check("ring resend")
	clk.add(10 * time.Second)
	both(client.Update{Huddle: true, HuddleStarted: []client.HuddleStart{{Chan: 10, By: 3}}})
	check("channel huddle")
	m.Update(ringMsg{gen: m.al.Ring.Gen})
	core.Fire(alert.Timer{Kind: alert.RingEndT, Gen: core.Eng.Ring.Gen})
	if m.al.Ring.Ch != 0 || core.Eng.Ring.Ch != 0 {
		t.Fatal("ring didn't end")
	}

	if n := len(bgOut.sent()); n < 14 {
		t.Fatalf("only %d notifications: the scenario lost its teeth", n)
	}
}
