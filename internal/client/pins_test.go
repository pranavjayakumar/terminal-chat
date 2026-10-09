package client

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

func pinIDsOf(ms []Message) []uint64 {
	var out []uint64
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func sameIDs(a, b []uint64) bool { return equalIDs(a, b) }

func TestPinEventsApplied(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	fc.event(21, proto.KPinAdd, general, bob, proto.MsgRef{Msg: 10})
	fc.event(22, proto.KPinAdd, general, alice, proto.MsgRef{Msg: 11})
	waitFor(t, "pins", func() bool { return lastSeq(t, c) == 22 })
	if got := pinIDsOf(c.Pins(general)); !sameIDs(got, []uint64{11, 10}) {
		t.Fatalf("Pins = %v, want newest first [11 10]", got)
	}
	if got := chanView(t, c, general).Pins; !sameIDs(got, []uint64{11, 10}) {
		t.Fatalf("ChannelView.Pins = %v", got)
	}
	if got := c.Pins(general)[1].Body; got != "first" {
		t.Fatalf("body = %q", got)
	}

	// Repeated pin.add (replay) doesn't duplicate or reorder.
	fc.event(23, proto.KPinAdd, general, bob, proto.MsgRef{Msg: 10})
	fc.event(24, proto.KPinRemove, general, bob, proto.MsgRef{Msg: 11})
	waitFor(t, "unpin", func() bool { return lastSeq(t, c) == 24 })
	if got := pinIDsOf(c.Pins(general)); !sameIDs(got, []uint64{10}) {
		t.Fatalf("Pins after unpin = %v", got)
	}

	// A pinned message that gets deleted drops out of Pins at once, before
	// (or without) the pin.remove.
	fc.event(25, proto.KMsgDelete, general, bob, proto.MsgRef{Msg: 10})
	waitFor(t, "delete", func() bool { return lastSeq(t, c) == 25 })
	if got := c.Pins(general); len(got) != 0 {
		t.Fatalf("deleted message still listed: %v", pinIDsOf(got))
	}
	fc.event(26, proto.KPinRemove, general, bob, proto.MsgRef{Msg: 10})
	waitFor(t, "remove", func() bool { return lastSeq(t, c) == 26 })
	if got := chanView(t, c, general).Pins; len(got) != 0 {
		t.Fatalf("ChannelView.Pins = %v", got)
	}
}

func TestPinsFromChannelObject(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	fc.event(21, proto.KChanUpdate, general, bob, proto.Channel{ID: general, Name: "general", Kind: proto.ChanPublic,
		Members: []uint64{alice, bob}, Pins: []uint64{11, 10}})
	waitFor(t, "update", func() bool { return lastSeq(t, c) == 21 })
	if got := pinIDsOf(c.Pins(general)); !sameIDs(got, []uint64{11, 10}) {
		t.Fatalf("Pins = %v", got)
	}
	// A later update without pins clears them; membership changes keep them.
	fc.event(22, proto.KChanJoin, general, bob, proto.ChanJoin{User: bob, Chan: proto.Channel{ID: general, Name: "general",
		Kind: proto.ChanPublic, Members: []uint64{alice, bob}, Pins: []uint64{10}}})
	waitFor(t, "join", func() bool { return lastSeq(t, c) == 22 })
	if got := pinIDsOf(c.Pins(general)); !sameIDs(got, []uint64{10}) {
		t.Fatalf("Pins = %v", got)
	}
	fc.event(23, proto.KChanLeave, general, bob, proto.UserRef{User: bob})
	waitFor(t, "leave", func() bool { return lastSeq(t, c) == 23 })
	if got := pinIDsOf(c.Pins(general)); !sameIDs(got, []uint64{10}) {
		t.Fatalf("Pins after another user left = %v", got)
	}
}

func TestFetchPinsFillsMissingBodies(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	// Message 3 is older than anything cached.
	fc.event(21, proto.KPinAdd, general, bob, proto.MsgRef{Msg: 3})
	fc.event(22, proto.KPinAdd, general, bob, proto.MsgRef{Msg: 10})
	waitFor(t, "pins", func() bool { return lastSeq(t, c) == 22 })
	if got := pinIDsOf(c.Pins(general)); !sameIDs(got, []uint64{10}) {
		t.Fatalf("uncached pin listed before fetch: %v", got)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- c.FetchPins(ctx, general)
	}()
	var a proto.ChanArgs
	r := fc.expectReq(proto.OpPinsList, &a)
	if a.Chan != general {
		t.Fatalf("pins.list chan %d", a.Chan)
	}
	fc.ok(r.ID, proto.MsgsRes{Msgs: []proto.Message{
		{ID: 10, Chan: general, Author: bob, Body: "first"},
		{ID: 3, Chan: general, Author: alice, Body: "ancient"},
	}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := c.Pins(general)
	if !sameIDs(pinIDsOf(got), []uint64{10, 3}) || got[1].Body != "ancient" {
		t.Fatalf("Pins after fetch = %+v", got)
	}
	// The fetched message isn't injected into the channel's timeline.
	if b := bodies(c.Messages(general, 50)); b != "first,mine" {
		t.Fatalf("timeline = %q", b)
	}
}

func TestPinUnpinOps(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	for _, tc := range []struct {
		op string
		do func(context.Context, uint64) error
	}{{proto.OpPin, c.Pin}, {proto.OpUnpin, c.Unpin}} {
		done := make(chan error, 1)
		go func() { done <- tc.do(context.Background(), 10) }()
		var a proto.MsgArgs
		r := fc.expectReq(tc.op, &a)
		if a.Msg != 10 {
			t.Fatalf("%s msg %d", tc.op, a.Msg)
		}
		fc.ok(r.ID, proto.SeqRes{Seq: 0})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestPinsClearedOnReset(t *testing.T) {
	c, _ := localClient(t)
	if err := c.tx(func(tx *sql.Tx) error {
		return storeChan(tx, proto.Channel{ID: general, Name: "general", Kind: proto.ChanPublic, Members: []uint64{alice}, Pins: []uint64{5}})
	}); err != nil {
		t.Fatal(err)
	}
	if got := c.pinIDs()[general]; len(got) != 1 {
		t.Fatalf("pins = %v", got)
	}
	if err := resetCache(c.db, serverID); err != nil {
		t.Fatal(err)
	}
	if got := c.pinIDs()[general]; len(got) != 0 {
		t.Fatalf("pins after reset = %v", got)
	}
}
