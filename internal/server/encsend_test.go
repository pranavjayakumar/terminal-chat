package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// encFor encrypts body for ch at the epoch of bundle b, signed by dev. The
// server can't check K, so any key will do.
func encFor(t *testing.T, sid e2ecrypto.ServerID, dev *e2ecrypto.DeviceKeys, ch uint64, epoch uint32, b proto.Signed, body string) []byte {
	t.Helper()
	k, err := e2ecrypto.NewEpochKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := e2ecrypto.Encrypt(rand.Reader, k, dev.Ed25519, sid, ch, epoch, e2ecrypto.EH(b.B), dev.ID, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func TestEncLimitMatchesE2ECrypto(t *testing.T) {
	if proto.MaxEnc != e2ecrypto.MaxEncSize {
		t.Fatal("proto.MaxEnc drifted from e2ecrypto")
	}
}

func newNonce() []byte {
	n := make([]byte, 16)
	rand.Read(n)
	return n
}

// errMsg sends op and returns its error (nil if ok).
func (c *tclient) errOf(op string, args any) *proto.Error {
	c.t.Helper()
	r := c.req(op, args)
	if r.OK {
		return nil
	}
	return r.Err
}

func wantErr(t *testing.T, what string, e *proto.Error, code, msgPart string) {
	t.Helper()
	if e == nil || e.Code != code || !strings.Contains(e.Msg, msgPart) {
		t.Fatalf("%s: got %v, want %s (%q)", what, e, code, msgPart)
	}
}

// Encrypted sends (§13.5): plaintext into an e2e channel is refused;
// duplicates are found before the epoch is checked; stale epochs and stale
// bundles are stale_epoch; the enc must be signed by an active device of
// the sender; history and snapshots return it as is; deleting clears it.
func TestEncryptedSend(t *testing.T) {
	ts := startServer(t, "tcp")
	advance := manualClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	carol := ts.registerCaps(t, "carol", false, e2eCaps)
	snap, _ := alice.syncFresh()
	bob.syncFresh()
	carol.syncFresh()
	gen := generalID(t, snap)
	sid := ts.sid()
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	a1, _ := alice.registerDevice(ts)
	b1, _ := bob.registerDevice(ts)
	roster := []uint64{alice.user.ID, bob.user.ID}

	send := func(c *tclient, a proto.SendArgs) *proto.Error { return c.errOf(proto.OpSend, a) }
	// Epoch 0: nothing to encrypt to, and plaintext is refused.
	wantErr(t, "plaintext", send(alice, proto.SendArgs{Chan: ch.ID, Body: "hi", Nonce: newNonce()}),
		proto.ErrBadRequest, "upgrade tc")
	e1 := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1)
	wantErr(t, "epoch 0", send(alice, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, a1, ch.ID, 1, e1, "x")}),
		proto.ErrStaleEpoch, "")
	alice.rotate(ch.ID, e1)

	enc := encFor(t, sid, a1, ch.ID, 1, e1, "secret")
	wantErr(t, "plaintext after epoch 1", send(alice, proto.SendArgs{Chan: ch.ID, Body: "hi", Nonce: newNonce()}),
		proto.ErrBadRequest, "upgrade tc")
	wantErr(t, "body and enc", send(alice, proto.SendArgs{Chan: ch.ID, Body: "hi", Nonce: newNonce(), Enc: enc}), proto.ErrBadRequest, "")
	wantErr(t, "enc into plaintext channel", send(alice, proto.SendArgs{Chan: gen, Nonce: newNonce(), Enc: enc}), proto.ErrBadRequest, "not encrypted")
	wantErr(t, "malformed enc", send(alice, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: enc[:40]}), proto.ErrBadRequest, "")
	wantErr(t, "huge enc", send(alice, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: make([]byte, proto.MaxEnc+1)}), proto.ErrTooLarge, "")
	// Signed by bob's device but sent by alice; signed by a1's id with
	// another key; by an unknown device; for another epoch-1 bundle.
	wantErr(t, "other's device", send(alice, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, b1, ch.ID, 1, e1, "x")}),
		proto.ErrBadRequest, "devices")
	forged := *newDevKeys(t)
	forged.ID = a1.ID
	wantErr(t, "bad signature", send(alice, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, &forged, ch.ID, 1, e1, "x")}),
		proto.ErrBadRequest, "signature")
	wantErr(t, "unknown device", send(alice, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, newDevKeys(t), ch.ID, 1, e1, "x")}),
		proto.ErrBadRequest, "devices")
	other := bundle(t, a1, sid, ch.ID, 1, nil, roster, a1, b1)
	wantErr(t, "other bundle", send(alice, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, a1, ch.ID, 1, other, "x")}),
		proto.ErrBadRequest, "bundle")
	wantErr(t, "future epoch", send(alice, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, a1, ch.ID, 2, e1, "x")}),
		proto.ErrStaleEpoch, "")

	nonce := newNonce()
	var r1 proto.SendRes
	alice.ok(proto.OpSend, proto.SendArgs{Chan: ch.ID, Nonce: nonce, Enc: enc}, &r1)
	m := bob.msgNew()
	if m.ID != r1.Msg || m.Body != "" || !bytes.Equal(m.Enc, enc) || m.Author != alice.user.ID {
		t.Fatalf("bob got %+v", m)
	}
	var r2 proto.SendRes
	alice.ok(proto.OpSend, proto.SendArgs{Chan: ch.ID, Nonce: nonce, Enc: enc}, &r2)
	if r2.Msg != r1.Msg {
		t.Fatalf("retry made message %d, want %d", r2.Msg, r1.Msg)
	}
	// The same enc under a new nonce is a replay, not a second message.
	wantErr(t, "replayed enc", send(alice, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: enc}),
		proto.ErrConflict, "already sent")
	wantErr(t, "same nonce, other enc", send(alice, proto.SendArgs{Chan: ch.ID, Nonce: nonce, Enc: encFor(t, sid, a1, ch.ID, 1, e1, "secret")}),
		proto.ErrConflict, "")

	// bob rotates: the stored message is still found by its nonce before
	// the epoch is checked (a lost RES must not turn into a failure).
	advance(rotateChanEveryMs * time.Millisecond)
	e2 := bundle(t, b1, sid, ch.ID, 2, &e1, roster, a1, b1)
	bob.rotate(ch.ID, e2)
	alice.ok(proto.OpSend, proto.SendArgs{Chan: ch.ID, Nonce: nonce, Enc: enc}, &r2)
	if r2.Msg != r1.Msg {
		t.Fatalf("retry after rotation made message %d, want %d", r2.Msg, r1.Msg)
	}
	wantErr(t, "old epoch", send(alice, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, a1, ch.ID, 1, e1, "late")}),
		proto.ErrStaleEpoch, "")

	// A member joins: the epoch-2 bundle is stale until someone rotates.
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: carol.user.ID}, nil)
	wantErr(t, "stale bundle", send(alice, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, a1, ch.ID, 2, e2, "x")}),
		proto.ErrStaleEpoch, "rotated")
	e3 := bundle(t, a1, sid, ch.ID, 3, &e2, []uint64{alice.user.ID, bob.user.ID, carol.user.ID}, a1, b1)
	alice.rotate(ch.ID, e3)
	enc3 := encFor(t, sid, a1, ch.ID, 3, e3, "hello carol")
	var r3 proto.SendRes
	alice.ok(proto.OpSend, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: enc3}, &r3)

	// Plaintext edits can't sneak text in either.
	wantErr(t, "plaintext edit", alice.errOf(proto.OpEdit, proto.EditArgs{Msg: r3.Msg, Body: "clear"}), proto.ErrBadRequest, "upgrade tc")

	// History and snapshots carry enc as is.
	var h proto.HistoryRes
	bob.ok(proto.OpHistory, proto.HistoryArgs{Chan: ch.ID, Limit: 10}, &h)
	if len(h.Msgs) != 2 || !bytes.Equal(h.Msgs[0].Enc, enc) || !bytes.Equal(h.Msgs[1].Enc, enc3) || h.Msgs[1].Body != "" {
		t.Fatalf("history %+v", h.Msgs)
	}
	sn, _ := bob.syncFreshAgain(t, ts)
	found := false
	for _, x := range sn.Msgs {
		if x.ID == r3.Msg {
			found = bytes.Equal(x.Enc, enc3)
		}
	}
	if !found {
		t.Fatal("snapshot lost the enc")
	}

	// Deleting scrubs the enc from the message and its msg.new event.
	alice.ok(proto.OpDelete, proto.MsgArgs{Msg: r3.Msg}, nil)
	var stored []byte
	if err := ts.st.db.QueryRow(`SELECT enc FROM messages WHERE id = ?`, r3.Msg).Scan(&stored); err != nil || stored != nil {
		t.Fatalf("deleted message keeps enc %x (%v)", stored, err)
	}
	var data []byte
	ts.st.db.QueryRow(`SELECT data FROM events WHERE seq = ?`, r3.Msg).Scan(&data)
	var ev proto.Message
	if err := proto.Unmarshal(data, &ev); err != nil || ev.Enc != nil || !ev.Deleted {
		t.Fatalf("msg.new after delete %+v (%v)", ev, err)
	}
}

// Server search never returns messages of encrypted channels: their text
// isn't the server's to see, and legacy plaintext in them must not leak.
func TestSearchExcludesE2E(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	alice.syncFresh()
	var plain proto.ChanRes
	alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "later", Private: true}, &plain)
	alice.send(plain.Chan.ID, "zebra crossing")
	search := func() int {
		var r proto.MsgsRes
		alice.ok(proto.OpSearch, proto.SearchArgs{Q: "zebra"}, &r)
		return len(r.Msgs)
	}
	if n := search(); n != 1 {
		t.Fatalf("plaintext channel: %d results", n)
	}
	// As if it had been encrypted (chan.encrypt, step 9): its old
	// plaintext is no longer searchable on the server.
	if _, err := ts.st.db.Exec(`UPDATE channels SET e2e = 1 WHERE id = ?`, plain.Chan.ID); err != nil {
		t.Fatal(err)
	}
	if n := search(); n != 0 {
		t.Fatalf("encrypted channel: %d results", n)
	}
}

// Webhooks can't be created in, or post into, encrypted channels.
func TestHookIntoE2EForbidden(t *testing.T) {
	ts := startHookServer(t)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	alice.syncFresh()
	sec := alice.createE2E("sec")
	wantErr(t, "hook.create", alice.errOf(proto.OpHookCreate, proto.HookCreateArgs{Chan: sec.ID, Name: "ci"}),
		proto.ErrForbidden, "encrypted")

	var plain proto.ChanRes
	alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "later", Private: true}, &plain)
	h := alice.createHook(plain.Chan.ID, "ci")
	if r := ts.post(t, "POST", h.URL, "text/plain", "before"); r.status != 200 {
		t.Fatalf("plaintext channel: %d %s", r.status, r.raw)
	}
	if _, err := ts.st.db.Exec(`UPDATE channels SET e2e = 1 WHERE id = ?`, plain.Chan.ID); err != nil {
		t.Fatal(err)
	}
	r := ts.post(t, "POST", h.URL, "text/plain", "after")
	if r.status != 403 || r.body["error"] != proto.ErrForbidden {
		t.Fatalf("encrypted channel: %d %s", r.status, r.raw)
	}
	var n int
	ts.st.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE chan = ?`, plain.Chan.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d messages, want 1", n)
	}
}
