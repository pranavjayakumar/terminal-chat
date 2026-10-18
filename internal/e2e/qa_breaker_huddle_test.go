package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/skailama/tc/internal/voice"
)

// A member who isn't in a channel's huddle and leaves the channel is never
// sent anything about that huddle again (§14.2: the empty roster goes only
// to users whose participation ended). The client keeps the last roster it
// had, so Huddles() keeps reporting a 🎧 for the channel after the huddle
// has long ended, and after rejoining the channel (the server sends no
// roster on chan.join when no huddle runs) the stale roster is shown as a
// live huddle with someone in it.
func TestHuddle_BreakStaleRosterAfterLeavingChannel(t *testing.T) {
	us := huddlers(t, "quic", "ana", "bo")
	ana, bo := us[0], us[1]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := ana.CreateChannel(ctx, "ops", false, "")
	if err != nil {
		t.Fatal(err)
	}
	ch := c.ID
	if err := bo.Join(ctx, ch); err != nil {
		t.Fatal(err)
	}
	ana.SetVoicePlatform((&fakeAudio{}).platform)
	if err := ana.JoinHuddle(ctx, ch); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bo sees the huddle", func() bool { return len(bo.Huddle(ch)) == 1 })

	if err := bo.Leave(ctx, ch); err != nil {
		t.Fatal(err)
	}
	ana.LeaveHuddle()
	// ana (still a member) learns the huddle ended.
	eventually(t, "ana sees the huddle end", func() bool { return len(ana.Huddle(ch)) == 0 })
	time.Sleep(500 * time.Millisecond)
	if n, ok := bo.Huddles()[ch]; ok {
		t.Errorf("after leaving the channel and the huddle ending, bo still shows a huddle of %d there (roster %v)", n, bo.Huddle(ch))
	}

	if err := bo.Join(ctx, ch); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if r := bo.Huddle(ch); len(r) != 0 {
		t.Errorf("after rejoining, bo shows an ended huddle as live: roster %v", r)
	}
}

// Consequence of the stale roster: when a new huddle starts in the channel
// after bo rejoined, the client thinks one was already running (had=true)
// and never reports the start.
func TestHuddle_BreakStaleRosterSwallowsNextStart(t *testing.T) {
	us := huddlers(t, "quic", "ana", "bo")
	ana, bo := us[0], us[1]
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	c, err := ana.CreateChannel(ctx, "ops", false, "")
	if err != nil {
		t.Fatal(err)
	}
	ch := c.ID
	if err := bo.Join(ctx, ch); err != nil {
		t.Fatal(err)
	}
	ana.SetVoicePlatform((&fakeAudio{}).platform)
	if err := ana.JoinHuddle(ctx, ch); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bo sees the huddle", func() bool { return len(bo.Huddle(ch)) == 1 })
	if err := bo.Leave(ctx, ch); err != nil {
		t.Fatal(err)
	}
	ana.LeaveHuddle()
	eventually(t, "ana sees the huddle end", func() bool { return len(ana.Huddle(ch)) == 0 })
	if err := bo.Join(ctx, ch); err != nil {
		t.Fatal(err)
	}
	boStarts := collectStarts(bo)
	time.Sleep(2500 * time.Millisecond) // past the chan.join baseline
	if err := ana.JoinHuddle(ctx, ch); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-boStarts:
		if s.Chan != ch {
			t.Fatalf("start %+v", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a huddle started live after bo rejoined was never reported as a start")
	}
}

// LeaveHuddle while a JoinHuddle is still opening the sound card (the user
// presses /leave, or quits the huddle view, a moment after /huddle): the
// server has already admitted us (RES came back), LeaveHuddle finds
// c.hud.cur == nil and sends nothing, and JoinHuddle then sees its gen is
// stale and tears down locally, firing huddle.leave only when *not* stale.
// Nobody tells the server: we stay in the roster, everyone sees us "in"
// the huddle and the relay keeps sending us their audio until we
// disconnect.
func TestHuddle_BreakLeaveDuringJoinLeavesGhost(t *testing.T) {
	us := huddlers(t, "quic", "ana", "bo")
	ana, bo := us[0], us[1]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gen := ana.channel("general")
	opening := make(chan struct{})
	left := make(chan struct{})
	ana.SetVoicePlatform(func() (voice.Platform, error) {
		return voice.Platform{Codec: fCodec{}, Open: func(func(out, in []int16)) (voice.Duplex, error) {
			close(opening)
			<-left
			return nopDuplex{}, nil
		}}, nil
	})
	done := make(chan error, 1)
	go func() { done <- ana.JoinHuddle(ctx, gen) }()
	select {
	case <-opening:
	case <-time.After(10 * time.Second):
		t.Fatal("join never reached the device")
	}
	ana.LeaveHuddle()
	close(left)
	if err := <-done; err == nil {
		t.Fatal("join succeeded although we left meanwhile")
	}
	if _, ok := ana.InHuddle(); ok {
		t.Fatal("still in a huddle locally")
	}
	time.Sleep(1500 * time.Millisecond)
	if r := bo.Huddle(gen); len(r) != 0 {
		t.Fatalf("ana left (locally torn down) but the server still lists her: bo sees roster %v", r)
	}
}
