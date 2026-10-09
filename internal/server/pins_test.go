package server

import (
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

func pinSetup(t *testing.T) (*testServer, *tclient, *tclient, uint64) {
	t.Helper()
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	bob := ts.register(t, "bob", false)
	alice.syncFresh()
	bob.syncFresh()
	var cr proto.ChanRes
	alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "secret", Private: true}, &cr)
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: cr.Chan.ID, User: bob.user.ID}, nil)
	bob.event(proto.KChanJoin)
	return ts, alice, bob, cr.Chan.ID
}

func TestPinCapAdvertised(t *testing.T) {
	ts := startServer(t, "tcp")
	c := ts.register(t, "alice", false)
	found := false
	for _, cp := range c.welcome.Caps {
		found = found || cp == proto.CapPins
	}
	if !found {
		t.Fatalf("caps %v lack pins", c.welcome.Caps)
	}
}

func TestPinSetSemanticsAndEvents(t *testing.T) {
	_, alice, bob, sec := pinSetup(t)
	m1 := alice.send(sec, "one").Msg
	m2 := alice.send(sec, "two").Msg

	var r proto.SeqRes
	bob.ok(proto.OpPin, proto.MsgArgs{Msg: m1}, &r) // any member may pin
	if r.Seq == 0 {
		t.Fatal("pin returned seq 0")
	}
	ev := alice.event(proto.KPinAdd)
	var ref proto.MsgRef
	proto.Unmarshal(ev.Data, &ref)
	if ref.Msg != m1 || ev.Chan != sec || ev.Actor != bob.user.ID {
		t.Fatalf("pin.add: %+v %+v", ev, ref)
	}
	bob.event(proto.KPinAdd)

	// Pinning again is a no-op: ok, seq 0, no event.
	r = proto.SeqRes{Seq: 9}
	alice.ok(proto.OpPin, proto.MsgArgs{Msg: m1}, &r)
	if r.Seq != 0 {
		t.Fatalf("repeat pin seq %d", r.Seq)
	}
	alice.quiet(100 * time.Millisecond)

	alice.ok(proto.OpPin, proto.MsgArgs{Msg: m2}, nil)
	var pl proto.MsgsRes
	bob.ok(proto.OpPinsList, proto.ChanArgs{Chan: sec}, &pl)
	if len(pl.Msgs) != 2 || pl.Msgs[0].ID != m2 || pl.Msgs[1].ID != m1 {
		t.Fatalf("pins.list newest first: %+v", pl.Msgs)
	}

	r = proto.SeqRes{}
	alice.ok(proto.OpUnpin, proto.MsgArgs{Msg: m1}, &r)
	if r.Seq == 0 {
		t.Fatal("unpin returned seq 0")
	}
	alice.event(proto.KPinRemove)
	r = proto.SeqRes{Seq: 9}
	alice.ok(proto.OpUnpin, proto.MsgArgs{Msg: m1}, &r)
	if r.Seq != 0 {
		t.Fatalf("repeat unpin seq %d", r.Seq)
	}
}

func TestPinPermissions(t *testing.T) {
	ts, alice, bob, sec := pinSetup(t)
	carol := ts.register(t, "carol", false)
	carol.syncFresh()
	m := alice.send(sec, "hi").Msg

	// A non-member can't pin, unpin or list in a private channel; the
	// message is not_found, as for react.
	carol.fail(proto.OpPin, proto.MsgArgs{Msg: m}, proto.ErrNotFound)
	carol.fail(proto.OpUnpin, proto.MsgArgs{Msg: m}, proto.ErrNotFound)
	carol.fail(proto.OpPinsList, proto.ChanArgs{Chan: sec}, proto.ErrForbidden)
	carol.fail(proto.OpPin, proto.MsgArgs{Msg: 99999}, proto.ErrNotFound)

	alice.ok(proto.OpPin, proto.MsgArgs{Msg: m}, nil)
	carol.quiet(150 * time.Millisecond) // events reach members only

	// Someone who left can't pin or unpin any more.
	bob.ok(proto.OpChanLeave, proto.ChanArgs{Chan: sec}, nil)
	bob.fail(proto.OpUnpin, proto.MsgArgs{Msg: m}, proto.ErrNotFound)
	bob.fail(proto.OpPinsList, proto.ChanArgs{Chan: sec}, proto.ErrForbidden)
}

func TestPinPublicListReadable(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	bob := ts.register(t, "bob", false)
	snap, _ := alice.syncFresh()
	bob.syncFresh()
	gen := generalID(t, snap)
	m := alice.send(gen, "hello").Msg
	alice.ok(proto.OpPin, proto.MsgArgs{Msg: m}, nil)
	var pl proto.MsgsRes
	bob.ok(proto.OpChanLeave, proto.ChanArgs{Chan: gen}, nil)
	bob.ok(proto.OpPinsList, proto.ChanArgs{Chan: gen}, &pl) // public: readable
	if len(pl.Msgs) != 1 || pl.Msgs[0].ID != m {
		t.Fatalf("%+v", pl)
	}
	bob.fail(proto.OpPin, proto.MsgArgs{Msg: m}, proto.ErrNotFound)
}

func TestPinDeletedMessage(t *testing.T) {
	_, alice, bob, sec := pinSetup(t)
	m := alice.send(sec, "doomed").Msg
	m2 := alice.send(sec, "other").Msg
	alice.ok(proto.OpPin, proto.MsgArgs{Msg: m}, nil)
	alice.ok(proto.OpPin, proto.MsgArgs{Msg: m2}, nil)
	alice.ok(proto.OpDelete, proto.MsgArgs{Msg: m}, nil)

	// pin.remove follows msg.delete, for every member.
	for _, c := range []*tclient{alice, bob} {
		c.event(proto.KMsgDelete)
		ev := c.event(proto.KPinRemove)
		var ref proto.MsgRef
		proto.Unmarshal(ev.Data, &ref)
		if ref.Msg != m {
			t.Fatalf("pin.remove for %d, want %d", ref.Msg, m)
		}
	}
	var pl proto.MsgsRes
	alice.ok(proto.OpPinsList, proto.ChanArgs{Chan: sec}, &pl)
	if len(pl.Msgs) != 1 || pl.Msgs[0].ID != m2 {
		t.Fatalf("pins after delete: %+v", pl.Msgs)
	}
	// A deleted message can't be pinned.
	alice.fail(proto.OpPin, proto.MsgArgs{Msg: m}, proto.ErrNotFound)
}

func TestPinCap(t *testing.T) {
	_, alice, bob, sec := pinSetup(t)
	var ids []uint64
	for i := 0; i < proto.MaxPins+1; i++ {
		c := alice
		if i%2 == 1 {
			c = bob // spread over two send buckets
		}
		ids = append(ids, c.send(sec, "m").Msg)
	}
	for _, id := range ids[:proto.MaxPins] {
		alice.ok(proto.OpPin, proto.MsgArgs{Msg: id}, nil)
	}
	alice.fail(proto.OpPin, proto.MsgArgs{Msg: ids[proto.MaxPins]}, proto.ErrConflict)
	// Re-pinning an existing pin at the cap is still a no-op, not a conflict.
	var r proto.SeqRes
	alice.ok(proto.OpPin, proto.MsgArgs{Msg: ids[0]}, &r)
	if r.Seq != 0 {
		t.Fatal("repeat pin at cap")
	}
}

func TestPinSnapshot(t *testing.T) {
	ts, alice, bob, sec := pinSetup(t)
	m1 := alice.send(sec, "one").Msg
	m2 := alice.send(sec, "two").Msg
	alice.ok(proto.OpPin, proto.MsgArgs{Msg: m1}, nil)
	alice.ok(proto.OpPin, proto.MsgArgs{Msg: m2}, nil)

	c2, _ := ts.connect(t, bob.signer, "", "")
	snap, _ := c2.syncFresh()
	var got []uint64
	for _, c := range snap.Chans {
		if c.ID == sec {
			got = c.Pins
		}
	}
	if len(got) != 2 || got[0] != m2 || got[1] != m1 {
		t.Fatalf("snapshot pins %v, want [%d %d]", got, m2, m1)
	}
}

func TestPinsAreWriteOps(t *testing.T) {
	if !writeOps[proto.OpPin] || !writeOps[proto.OpUnpin] {
		t.Fatal("pin/unpin must count as write ops")
	}
}
