package server

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// tag is a well-formed reaction tag (the server can't check its HMAC).
func tag(i int) string { return fmt.Sprintf("~AAAAAAAAAAAAAA%02d", i) }

// Encrypted edit, delete and react (§13.5, step 8): the server checks what
// it can (framing, signer, epoch) and stores and forwards the encs; it
// never sees an emoji.
func TestEncryptedEditDeleteReact(t *testing.T) {
	ts := startServer(t, "tcp")
	advance := manualClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps) // admin
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	snap, _ := alice.syncFresh()
	bob.syncFresh()
	gen := generalID(t, snap)
	sid := ts.sid()
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	a1, _ := alice.registerDevice(ts)
	b1, _ := bob.registerDevice(ts)
	roster := []uint64{alice.user.ID, bob.user.ID}
	e1 := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1)
	alice.rotate(ch.ID, e1)
	var sent proto.SendRes
	alice.ok(proto.OpSend, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, a1, ch.ID, 1, e1, "orig")}, &sent)
	id := sent.Msg
	plainMsg := alice.send(gen, "plain").Msg
	edit := func(c *tclient, a proto.EditArgs) *proto.Error { return c.errOf(proto.OpEdit, a) }

	// ---- edit ----
	ed1 := encFor(t, sid, a1, ch.ID, 1, e1, "edit 1")
	wantErr(t, "body and enc", edit(alice, proto.EditArgs{Msg: id, Body: "x", Enc: ed1}), proto.ErrBadRequest, "both")
	wantErr(t, "malformed", edit(alice, proto.EditArgs{Msg: id, Enc: ed1[:50]}), proto.ErrBadRequest, "malformed")
	wantErr(t, "enc edit of plaintext channel", edit(alice, proto.EditArgs{Msg: plainMsg, Enc: ed1}), proto.ErrBadRequest, "not encrypted")
	wantErr(t, "not the author", edit(bob, proto.EditArgs{Msg: id, Enc: encFor(t, sid, b1, ch.ID, 1, e1, "x")}), proto.ErrForbidden, "")
	wantErr(t, "other's device", edit(alice, proto.EditArgs{Msg: id, Enc: encFor(t, sid, b1, ch.ID, 1, e1, "x")}), proto.ErrBadRequest, "devices")
	wantErr(t, "future epoch", edit(alice, proto.EditArgs{Msg: id, Enc: encFor(t, sid, a1, ch.ID, 2, e1, "x")}), proto.ErrStaleEpoch, "")
	alice.ok(proto.OpEdit, proto.EditArgs{Msg: id, Enc: ed1}, nil)
	ev := bob.event(proto.KMsgEdit)
	var me proto.MsgEdit
	if proto.Unmarshal(ev.Data, &me) != nil || me.Msg != id || me.Body != "" || !bytes.Equal(me.Enc, ed1) || me.Edited == 0 {
		t.Fatalf("bob got edit %+v", me)
	}
	firstEdit := ev.Seq

	// bob rotates: a retry of the current edit is still found first; a new
	// edit at the old epoch is stale.
	advance(rotateChanEveryMs * time.Millisecond)
	e2 := bundle(t, b1, sid, ch.ID, 2, &e1, roster, a1, b1)
	bob.rotate(ch.ID, e2)
	var r proto.SeqRes
	alice.ok(proto.OpEdit, proto.EditArgs{Msg: id, Enc: ed1}, &r)
	if r.Seq != 0 {
		t.Fatalf("edit retry made event %d", r.Seq)
	}
	wantErr(t, "old epoch", edit(alice, proto.EditArgs{Msg: id, Enc: encFor(t, sid, a1, ch.ID, 1, e1, "late")}), proto.ErrStaleEpoch, "")
	ed2 := encFor(t, sid, a1, ch.ID, 2, e2, "edit 2")
	alice.ok(proto.OpEdit, proto.EditArgs{Msg: id, Enc: ed2}, nil)

	// The superseded edit's enc is scrubbed from the log; msg.new carries
	// the original enc and the latest edit; history too.
	var data []byte
	ts.st.db.QueryRow(`SELECT data FROM events WHERE seq = ?`, firstEdit).Scan(&data)
	var old proto.MsgEdit
	if proto.Unmarshal(data, &old) != nil || old.Enc != nil || old.Msg != id {
		t.Fatalf("superseded edit %+v", old)
	}
	ts.st.db.QueryRow(`SELECT data FROM events WHERE seq = ?`, id).Scan(&data)
	var nm proto.Message
	if proto.Unmarshal(data, &nm) != nil || nm.Enc == nil || !bytes.Equal(nm.EditEnc, ed2) {
		t.Fatalf("msg.new after edits %+v", nm)
	}
	history := func(c *tclient) proto.Message {
		var h proto.HistoryRes
		c.ok(proto.OpHistory, proto.HistoryArgs{Chan: ch.ID, Limit: 10}, &h)
		for _, m := range h.Msgs {
			if m.ID == id {
				return m
			}
		}
		t.Fatal("message not in history")
		return proto.Message{}
	}
	if h := history(bob); !bytes.Equal(h.EditEnc, ed2) || h.Enc == nil || h.Edited == 0 {
		t.Fatalf("history %+v", h)
	}

	// ---- react ----
	react := func(c *tclient, op string, a proto.ReactArgs) *proto.Error { return c.errOf(op, a) }
	renc := encFor(t, sid, b1, ch.ID, 1, e1, "react") // the message's epoch
	wantErr(t, "plain emoji", react(bob, proto.OpReact, proto.ReactArgs{Msg: id, Emoji: "👍", Enc: renc}), proto.ErrBadRequest, "upgrade")
	wantErr(t, "tag without enc", react(bob, proto.OpReact, proto.ReactArgs{Msg: id, Emoji: tag(0)}), proto.ErrBadRequest, "upgrade")
	wantErr(t, "enc in plaintext channel", react(bob, proto.OpReact, proto.ReactArgs{Msg: plainMsg, Emoji: "👍", Enc: renc}), proto.ErrBadRequest, "not encrypted")
	wantErr(t, "other epoch", react(bob, proto.OpReact, proto.ReactArgs{Msg: id, Emoji: tag(0), Enc: encFor(t, sid, b1, ch.ID, 2, e2, "r")}), proto.ErrBadRequest, "epoch")
	wantErr(t, "other's device", react(alice, proto.OpReact, proto.ReactArgs{Msg: id, Emoji: tag(0), Enc: renc}), proto.ErrBadRequest, "devices")
	bob.ok(proto.OpReact, proto.ReactArgs{Msg: id, Emoji: tag(0), Enc: renc}, nil)
	ev = alice.event(proto.KReactAdd)
	var ra proto.Reaction
	if proto.Unmarshal(ev.Data, &ra) != nil || ra.Emoji != tag(0) || !bytes.Equal(ra.Enc, renc) {
		t.Fatalf("alice got %+v", ra)
	}
	bob.ok(proto.OpReact, proto.ReactArgs{Msg: id, Emoji: tag(0), Enc: encFor(t, sid, b1, ch.ID, 1, e1, "again")}, &r)
	if r.Seq != 0 {
		t.Fatal("a second reaction with the same tag isn't a no-op")
	}
	for i := 1; i < maxReactionsPerUser; i++ {
		bob.ok(proto.OpReact, proto.ReactArgs{Msg: id, Emoji: tag(i), Enc: encFor(t, sid, b1, ch.ID, 1, e1, "r")}, nil)
	}
	wantErr(t, "cap per user", react(bob, proto.OpReact, proto.ReactArgs{Msg: id, Emoji: tag(50), Enc: encFor(t, sid, b1, ch.ID, 1, e1, "r")}),
		proto.ErrBadRequest, "too many")
	h := history(alice)
	if len(h.Reactions) != maxReactionsPerUser || len(h.Renc) != maxReactionsPerUser || h.Renc[0].Emoji != tag(0) ||
		!bytes.Equal(h.Renc[0].Enc, renc) || h.Renc[0].User != bob.user.ID {
		t.Fatalf("history reactions %v %+v", h.Reactions, h.Renc)
	}
	unenc := encFor(t, sid, b1, ch.ID, 1, e1, "unreact")
	bob.ok(proto.OpUnreact, proto.ReactArgs{Msg: id, Emoji: tag(0), Enc: unenc}, nil)
	if h := history(alice); len(h.Renc) != maxReactionsPerUser-1 {
		t.Fatalf("after unreact %+v", h.Renc)
	}

	// ---- delete ----
	del := func(c *tclient, a proto.DeleteArgs) *proto.Error { return c.errOf(proto.OpDelete, a) }
	wantErr(t, "delete at another epoch", del(alice, proto.DeleteArgs{Msg: id, Enc: encFor(t, sid, a1, ch.ID, 2, e2, "d")}), proto.ErrBadRequest, "epoch")
	wantErr(t, "delete signed by another's device", del(alice, proto.DeleteArgs{Msg: id, Enc: encFor(t, sid, b1, ch.ID, 1, e1, "d")}), proto.ErrBadRequest, "devices")
	denc := encFor(t, sid, a1, ch.ID, 1, e1, "del")
	alice.ok(proto.OpDelete, proto.DeleteArgs{Msg: id, Enc: denc}, nil)
	ev = bob.event(proto.KMsgDelete)
	var md proto.MsgDelete
	if proto.Unmarshal(ev.Data, &md) != nil || md.Msg != id || !bytes.Equal(md.Enc, denc) {
		t.Fatalf("bob got delete %+v", md)
	}
	if h := history(bob); !h.Deleted || h.Enc != nil || h.EditEnc != nil || !bytes.Equal(h.DelEnc, denc) || len(h.Renc) != 0 {
		t.Fatalf("deleted message %+v", h)
	}
	ts.st.db.QueryRow(`SELECT data FROM events WHERE seq = ?`, id).Scan(&data)
	nm = proto.Message{}
	if proto.Unmarshal(data, &nm) != nil || nm.Enc != nil || nm.EditEnc != nil || !bytes.Equal(nm.DelEnc, denc) {
		t.Fatalf("msg.new after delete %+v", nm)
	}

	// An admin can delete someone else's message, but can't sign for them.
	var bs proto.SendRes
	bob.ok(proto.OpSend, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, b1, ch.ID, 2, e2, "bob's")}, &bs)
	wantErr(t, "admin signs a delete", del(alice, proto.DeleteArgs{Msg: bs.Msg, Enc: encFor(t, sid, a1, ch.ID, 2, e2, "d")}), proto.ErrForbidden, "")
	alice.ok(proto.OpDelete, proto.DeleteArgs{Msg: bs.Msg}, nil)
	var dstored []byte
	if err := ts.st.db.QueryRow(`SELECT del_enc FROM messages WHERE id = ?`, bs.Msg).Scan(&dstored); err != nil || dstored != nil {
		t.Fatalf("unsigned delete stored %x (%v)", dstored, err)
	}
}
