package background

import (
	"testing"
	"time"

	"github.com/skailama/tc/internal/alert"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// A DM notifies and opens a 5 s coalescing window; a second DM lands in
// it. The user opens tc before the window closes, reads, and quits without
// any new message arriving meanwhile. The window's FlushTimer fired while
// tc was open, so Core.Fire returned early without closing it: Notes[20]
// stays pending forever, and every later DM in that channel is folded into
// a window whose timer is gone. The channel never notifies again.
func TestCore_BreakFlushWhileUIOpenSilencesChannelForever(t *testing.T) {
	open := false
	out := &fakeOut{}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	c := NewCore(&fakeSource{}, out, alert.Env{}, func() bool { return open }, func() time.Time { return now })

	var timers []alert.Timer
	timers = append(timers, c.Handle(dm(1, "one"))...)
	now = now.Add(time.Second)
	timers = append(timers, c.Handle(dm(2, "two"))...)
	if out.n() != 1 || len(timers) != 1 {
		t.Fatalf("setup: %d notes, timers %v", out.n(), timers)
	}

	// tc opens; the window's timer fires while it is open; tc closes.
	open = true
	now = now.Add(alert.MentionWindow)
	for _, tm := range timers {
		c.Fire(tm)
	}
	open = false

	// An hour later bob DMs again, several times.
	for i := uint64(0); i < 3; i++ {
		now = now.Add(time.Hour)
		for _, tm := range c.Handle(dm(10+i, "later")) {
			now = now.Add(tm.After)
			c.Fire(tm)
		}
	}
	if out.n() == 1 {
		t.Fatalf("no notification for 3 DMs over 3 hours after tc was opened once; pending %+v", c.Eng.Notes[20])
	}
}

// rosterSource adds a DM 30 with carol (4) and live huddle rosters.
type rosterSource struct {
	fakeSource
	roster map[uint64][]uint64
}

func (r *rosterSource) Channels() []client.ChannelView {
	return append(r.fakeSource.Channels(), client.ChannelView{Channel: proto.Channel{ID: 30, Kind: proto.ChanDM, Members: []uint64{1, 4}}, Display: "carol", Member: true})
}
func (r *rosterSource) Users() []proto.User {
	return append(r.fakeSource.Users(), proto.User{ID: 4, Name: "carol"})
}
func (r *rosterSource) Huddle(ch uint64) []uint64 { return r.roster[ch] }

// Same for a ring: its end timer fires while tc is open, so the ring never
// ends (no cooldown) while bob's huddle goes on without us. Hours later,
// carol's DM huddle only gets the "already ringing" plain popup, never a
// real ring (no resends, no ring cue).
func TestCore_BreakRingEndWhileUIOpenLeavesRingStuck(t *testing.T) {
	open := false
	out := &fakeOut{}
	src := &rosterSource{roster: map[uint64][]uint64{20: {2}}}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	c := NewCore(src, out, alert.Env{}, func() bool { return open }, func() time.Time { return now })
	ts := c.Handle(client.Update{HuddleStarted: []client.HuddleStart{{Chan: 20, By: 2}}})
	if c.Eng.Ring.Ch != 20 || len(ts) != 2 {
		t.Fatalf("setup: ring %+v timers %v", c.Eng.Ring, ts)
	}
	open = true
	now = now.Add(alert.RingFor)
	for _, tm := range ts {
		c.Fire(tm)
	}
	open = false
	now = now.Add(time.Hour)
	src.roster[30] = []uint64{4}
	ts = c.Handle(client.Update{Huddle: true, HuddleStarted: []client.HuddleStart{{Chan: 30, By: 4}}})
	if c.Eng.Ring.Ch != 30 || len(ts) == 0 {
		t.Fatalf("carol's huddle didn't ring: ring still %+v (from an hour ago), timers %v", c.Eng.Ring, ts)
	}
}
