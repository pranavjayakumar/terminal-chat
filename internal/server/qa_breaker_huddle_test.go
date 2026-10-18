package server

import (
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// Coverage for paths the existing huddle tests don't drive over the wire.

// A participant disabled mid-huddle is dropped at once: the others get a
// roster without them and nothing more is relayed to or from them.
func TestHuddle_BreakDisabledMidHuddle(t *testing.T) {
	ts := startServer(t, "quic")
	adm := ts.registerCaps(t, "adm", true, voiceCaps)
	adm.syncFresh()
	a, b := ts.voiceClient(t, "ana"), ts.voiceClient(t, "bo")
	g := a.gen
	a.join(g)
	b.join(g)
	a.rosterIs(g, a.user.ID, b.user.ID)

	adm.ok(proto.OpUserDisable, proto.UserArgs{User: b.user.ID}, nil)
	a.rosterIs(g, a.user.ID)
	a.sendDg(a.frame(0, "to-nobody"))
	time.Sleep(100 * time.Millisecond)
	ts.hud.mu.RLock()
	n := len(ts.hud.bySess)
	ts.hud.mu.RUnlock()
	if n != 1 {
		t.Fatalf("%d participations after disabling one of two", n)
	}
}

// No huddle in a DM (or private channel) you aren't part of, whatever the
// arguments; and a non-member's datagrams carrying a participant's src go
// nowhere.
func TestHuddle_BreakOutsiderDMAndSpoof(t *testing.T) {
	ts := startServer(t, "quic")
	a, b, c := ts.voiceClient(t, "ana"), ts.voiceClient(t, "bo"), ts.voiceClient(t, "cy")
	var dm proto.ChanRes
	a.ok(proto.OpDMOpen, proto.DMOpenArgs{Users: []uint64{b.user.ID}}, &dm)
	c.fail(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: dm.Chan.ID}, proto.ErrForbidden)
	c.fail(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: dm.Chan.ID, Dev: make([]byte, 16), Stream: make([]byte, 16)}, proto.ErrForbidden)

	var pr proto.ChanRes
	a.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "priv", Private: true}, &pr)
	ch := pr.Chan.ID
	a.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch, User: b.user.ID}, nil)
	c.fail(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: ch}, proto.ErrForbidden)
	a.join(ch)
	b.join(ch)
	a.rosterIs(ch, a.user.ID, b.user.ID)
	c.src = a.src
	spoof := c.frame(0, "spoof")
	c.sendDg(spoof)
	if got := b.drain(200 * time.Millisecond); len(got) != 0 {
		t.Fatalf("outsider's spoofed frame relayed: %x", got)
	}
}

// A second session of the same user (another device) that isn't in the
// huddle can't speak with the first session's src, and when it joins, the
// first session's participation ends and its frames stop being relayed.
func TestHuddle_BreakTwoSessionsSameUser(t *testing.T) {
	ts := startServer(t, "quic")
	a, b := ts.voiceClient(t, "ana"), ts.voiceClient(t, "bo")
	g := a.gen
	a.join(g)
	b.join(g)
	a.rosterIs(g, a.user.ID, b.user.ID)

	a2c, bye := ts.connectCaps(t, a.signer, "", "", voiceCaps)
	if bye != "" {
		t.Fatal(bye)
	}
	a2c.syncFresh()
	a2 := &vclient{tclient: a2c, gen: g}
	a2.dg, _ = transport.Datagrams(a2c.conn)
	a2.src = a.src
	a2.sendDg(a2.frame(0, "other-session"))
	if got := b.drain(200 * time.Millisecond); len(got) != 0 {
		t.Fatalf("non-participating session spoke with its sibling's src: %x", got)
	}
	a2.join(g)
	b.rosterIs(g, b.user.ID, a.user.ID)
	a.sendDg(a.frame(1, "old-session"))
	if got := b.drain(200 * time.Millisecond); len(got) != 0 {
		t.Fatalf("superseded session's frame relayed: %x", got)
	}
	a2.sendDg(a2.frame(2, "new-session"))
	if got := b.drain(300 * time.Millisecond); len(got) != 1 {
		t.Fatalf("new session's frame: got %d", len(got))
	}
	// huddle.leave from the superseded session is a no-op for the new one.
	a.ok(proto.OpHuddleLeave, struct{}{}, nil)
	time.Sleep(100 * time.Millisecond)
	ts.hud.mu.RLock()
	n := len(ts.hud.bySess)
	ts.hud.mu.RUnlock()
	if n != 2 {
		t.Fatalf("%d participations, want 2", n)
	}
}
