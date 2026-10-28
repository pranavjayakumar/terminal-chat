package server

import (
	"strings"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

func TestDisplayName(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	bob := ts.register(t, "bob", false)
	alice.syncFresh()
	bob.syncFresh()

	alice.ok(proto.OpUserUpdate, proto.UserUpdateArgs{Display: "Alice Liddell 🐇"}, nil)
	ev := bob.event(proto.KUserUpdate)
	var u proto.User
	proto.Unmarshal(ev.Data, &u)
	if u.ID != alice.user.ID || u.Display != "Alice Liddell 🐇" || u.Name != "alice" {
		t.Fatalf("user.update: %+v", u)
	}
	var noop proto.SeqRes
	alice.ok(proto.OpUserUpdate, proto.UserUpdateArgs{Display: "Alice Liddell 🐇"}, &noop)
	if noop.Seq != 0 {
		t.Fatal("unchanged display produced an event")
	}
	for _, bad := range []string{"two\nlines", "\x1b[31mred", "rtl‮evil", strings.Repeat("x", proto.MaxDisplay+1),
		"ad\u200bmin", "Alice\u2060", " Alice", "Alice ", "\u3164", "A" + strings.Repeat("\u0301", 8)} {
		alice.fail(proto.OpUserUpdate, proto.UserUpdateArgs{Display: bad}, proto.ErrBadRequest)
	}
	// Survives a fresh sync.
	snap, _ := bob.syncFreshAgain(t, ts)
	for _, su := range snap.Users {
		if su.ID == alice.user.ID && su.Display != "Alice Liddell 🐇" {
			t.Fatalf("snapshot display %q", su.Display)
		}
	}
}

func TestThreadHistory(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.register(t, "alice", true)
	snap, _ := alice.syncFresh()
	gen := generalID(t, snap)
	root := alice.send(gen, "root").Msg
	other := alice.send(gen, "other root").Msg
	var replies []uint64
	for i := range 5 {
		nonce := make([]byte, 16)
		nonce[0] = byte(i + 1)
		var r proto.SendRes
		alice.ok(proto.OpSend, proto.SendArgs{Chan: gen, Body: "reply", Nonce: nonce, Thread: root}, &r)
		replies = append(replies, r.Msg)
	}
	alice.ok(proto.OpSend, proto.SendArgs{Chan: gen, Body: "elsewhere", Nonce: make([]byte, 16), Thread: other}, nil)

	var h proto.HistoryRes
	alice.ok(proto.OpHistory, proto.HistoryArgs{Chan: gen, Thread: root, Limit: 3}, &h)
	if len(h.Msgs) != 3 || !h.More || h.Msgs[2].ID != replies[4] {
		t.Fatalf("page 1: %d msgs more=%v", len(h.Msgs), h.More)
	}
	for _, m := range h.Msgs {
		if m.Thread != root {
			t.Fatalf("message %d from another thread", m.ID)
		}
	}
	alice.ok(proto.OpHistory, proto.HistoryArgs{Chan: gen, Thread: root, Limit: 3, Before: h.Msgs[0].ID}, &h)
	if len(h.Msgs) != 2 || h.More {
		t.Fatalf("page 2: %d msgs more=%v", len(h.Msgs), h.More)
	}
}
