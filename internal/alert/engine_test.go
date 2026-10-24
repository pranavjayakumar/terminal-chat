package alert

import (
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/proto"
)

type world struct{ roster []uint64 }

func (world) Me() uint64 { return 1 }
func (world) Chan(id uint64) (client.ChannelView, bool) {
	switch id {
	case 20:
		return client.ChannelView{Channel: proto.Channel{ID: 20, Kind: proto.ChanDM, Members: []uint64{1, 2}}, Display: "bob", Member: true}, true
	case 10:
		return client.ChannelView{Channel: proto.Channel{ID: 10, Name: "general", Members: []uint64{1, 2}}, Member: true}, true
	case 30:
		return client.ChannelView{Channel: proto.Channel{ID: 30, Kind: proto.ChanAgentDM, Members: []uint64{1}, Creator: 1}, Display: "rocky", Member: true}, true
	}
	return client.ChannelView{}, false
}
func (world) User(id uint64) proto.User {
	return map[uint64]proto.User{1: {ID: 1, Name: "alice"}, 2: {ID: 2, Name: "bob"}}[id]
}
func (world) MentionNames() []string                      { return []string{"alice"} }
func (world) Highlights() []string                        { return nil }
func (world) InMyThread(proto.Message) bool               { return false }
func (world) Current(proto.Message) (proto.Message, bool) { return proto.Message{}, false }
func (world) Looking(uint64) bool                         { return false }
func (world) InHuddle(uint64) bool                        { return false }
func (w world) Roster(uint64) ([]uint64, bool)            { return w.roster, true }

type out struct{ n []notify.Note }

func (o *out) Send(n notify.Note) bool { o.n = append(o.n, n); return true }

func msg(id, ch uint64, body string) []client.Message {
	return []client.Message{{Message: proto.Message{ID: id, Chan: ch, Author: 2, Body: body}}}
}

// A window: the first message at once, the rest summarized at the timer;
// the 2 s cap holds a second channel's first message back.
func TestEngineWindowsAndCap(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	o := &out{}
	e := New(world{}, o, func() time.Time { return now })
	ts := e.Messages(msg(1, 20, "one"))
	if len(o.n) != 1 || len(ts) != 1 || ts[0].After != MentionWindow {
		t.Fatalf("first: %d notes, timers %+v", len(o.n), ts)
	}
	e.Messages(msg(2, 20, "two"))
	e.Messages(msg(3, 20, "three"))
	// Another channel within the cap: no popup yet, its own window.
	ts2 := e.Messages(msg(4, 10, "@alice hi"))
	if len(o.n) != 1 || len(ts2) != 1 {
		t.Fatalf("capped: %d notes", len(o.n))
	}
	now = now.Add(MentionWindow)
	e.Flush(20, ts[0].Gen)
	if len(o.n) != 2 || o.n[1].Body != "2 new messages" || o.n[1].Title != "bob (DM)" {
		t.Fatalf("summary: %+v", o.n)
	}
	// Right after another popup: the flush waits out the cap.
	if again := e.Flush(10, ts2[0].Gen); len(again) != 1 || again[0].After <= 0 || len(o.n) != 2 {
		t.Fatalf("cap on flush: %+v %d", again, len(o.n))
	}
	now = now.Add(Every)
	e.Flush(10, ts2[0].Gen)
	if len(o.n) != 3 || o.n[2].Body != "@alice hi" {
		t.Fatalf("held-back mention: %+v", o.n)
	}
	// A stale generation does nothing.
	if e.Flush(20, ts[0].Gen) != nil || len(o.n) != 3 {
		t.Fatal("stale flush fired")
	}
}

// A ring: resent once, ended by its timer with a cooldown, stopped by
// settings that no longer allow it, and by the huddle emptying.
func TestEngineRing(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	o := &out{}
	w := world{roster: []uint64{2}}
	e := New(w, o, func() time.Time { return now })
	start := []client.HuddleStart{{Chan: 20, By: 2}}
	ts := e.HuddleStarts(start)
	if e.Ring.Ch != 20 || len(ts) != 2 || len(o.n) != 1 || o.n[0].Cue != notify.CueRing {
		t.Fatalf("ring: %+v %+v %+v", e.Ring, ts, o.n)
	}
	e.RingTimer(e.Ring.Gen-1, true) // stale
	e.RingTimer(e.Ring.Gen, true)
	if len(o.n) != 2 {
		t.Fatalf("resend: %d", len(o.n))
	}
	e.RingTimer(e.Ring.Gen, false)
	if e.Ring.Ch != 0 {
		t.Fatal("ring outlived its timer")
	}
	now = now.Add(RingRepeat + time.Second)
	if e.HuddleStarts(start); e.Ring.Ch != 0 {
		t.Fatal("rang again inside the cooldown")
	}
	now = now.Add(RingCooldown)
	e.HuddleStarts(start)
	if e.Ring.Ch != 20 {
		t.Fatal("no ring after the cooldown")
	}
	p := DefaultPrefs()
	p.DNDUntil = now.Add(time.Hour).UnixMilli()
	e.SetPrefs(p)
	if e.Ring.Ch != 0 {
		t.Fatal("dnd didn't stop the ring")
	}
	e.SetPrefs(DefaultPrefs())
	now = now.Add(RingCooldown + RingRepeat)
	e.HuddleStarts(start)
	e.W = world{roster: nil} // the huddle ended
	e.CheckRing()
	if e.Ring.Ch != 0 {
		t.Fatal("ring survived an empty huddle")
	}
}

// My agent's reply is not my own message: it notifies like a DM, titled
// after the agent.
func TestEngineAgentReply(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	o := &out{}
	e := New(world{}, o, func() time.Time { return now })
	mine := proto.Message{ID: 1, Chan: 30, Author: 1, Body: "my ask"}
	reply := proto.Message{ID: 2, Chan: 30, Author: 1, Agent: "rocky", Body: "done: build is green"}
	if e.ShouldNotify(mine) {
		t.Fatal("my own message notified")
	}
	if !e.ShouldNotify(reply) {
		t.Fatal("my agent's reply didn't notify")
	}
	if title, body, _ := e.NoteText(reply); title != "rocky (your agent)" || body != "done: build is green" {
		t.Fatalf("note %q %q", title, body)
	}
}
