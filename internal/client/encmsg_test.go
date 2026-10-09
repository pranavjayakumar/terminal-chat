package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// Encrypted messages against a scripted server (step 7): sending with
// retries, and what the client accepts.

var dmRoster = []uint64{alice, bob}

// encDM is alice's client in DM dm with bob (device b1), at epoch 1 (key
// k1, bundle e1 at seq seq1), both devices boxed.
type encDM struct {
	fs   *fakeServer
	c    *Client
	f    *e2eFake
	me   *e2ecrypto.DeviceKeys
	b1   peer
	k1   e2ecrypto.Key
	e1   proto.Signed
	seq1 uint64
}

func newEncDM(t *testing.T, sn proto.Snapshot) *encDM {
	t.Helper()
	fs, c := e2eClient(t)
	c.timing.devWait = 5 * time.Second
	run(t, c)
	d := &encDM{fs: fs, c: c, b1: newPeer(t, bob, nil), k1: newK(t)}
	sn.Chans = append(sn.Chans, dmChan(0))
	sn.Devs = append(sn.Devs, proto.Device{Cert: d.b1.cert})
	d.f = startE2E(t, fs, sn, proto.DevicesListRes{})
	d.me = d.f.myKeys(c)
	d.e1 = mkBundle(t, d.b1.dev, dm, 1, nil, dmRoster, d.k1, nil, d.me, d.b1.dev)
	d.seq1 = d.f.keyEpoch(dm, bob, d.e1)
	waitFor(t, "epoch 1", status(c, dm, E2EOK))
	return d
}

// sealAs encrypts Plain p into dm at epoch (bundle b, key k) as device dev.
func sealAs(t *testing.T, dev *e2ecrypto.DeviceKeys, k e2ecrypto.Key, epoch uint32, b proto.Signed, p plainMsg) []byte {
	t.Helper()
	raw, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := e2ecrypto.Encrypt(rand.Reader, k, dev.Ed25519, testSID(), dm, epoch, e2ecrypto.EH(b.B), dev.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func openAs(t *testing.T, k e2ecrypto.Key, signer *e2ecrypto.DeviceKeys, enc []byte) plainMsg {
	t.Helper()
	raw, err := e2ecrypto.Decrypt(k, signer.Ed25519Pub, testSID(), dm, enc)
	if err != nil {
		t.Fatal(err)
	}
	var p plainMsg
	if err := proto.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func nextSend(t *testing.T, f *e2eFake) proto.SendArgs {
	t.Helper()
	select {
	case a := <-f.sends:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("no send")
		return proto.SendArgs{}
	}
}

// resumeE2E serves the client's next connection on old's state: before
// runs ahead of the sync RES (events "while we were away"), no reset.
func resumeE2E(t *testing.T, fs *fakeServer, old *e2eFake, before func(f *e2eFake)) *e2eFake {
	t.Helper()
	fc := fs.accept()
	old.mu.Lock()
	f := &e2eFake{t: t, fc: fc, seq: old.seq, devs: old.devs, bundles: old.bundles,
		registered: make(chan proto.Signed, 4), rotates: make(chan proto.KeyRotateArgs, 16), sends: make(chan proto.SendArgs, 16),
		shares: make(chan proto.KeyShareArgs, 16), needs: make(chan proto.Ephemeral, 64)}
	old.mu.Unlock()
	r := fc.expectReq(proto.OpSync, nil)
	if before != nil {
		before(f)
	}
	f.ok(r.ID, proto.SyncRes{Head: f.seq})
	go f.serve()
	return f
}

func msgsOf(c *Client, ch uint64) []Message { return c.Messages(ch, 100) }

func findMsg(c *Client, id uint64) (Message, bool) {
	for _, m := range msgsOf(c, dm) {
		if m.ID == id {
			return m, true
		}
	}
	for _, m := range c.Thread(id) {
		if m.ID == id {
			return m, true
		}
	}
	return Message{}, false
}

// A send encrypts with the verified head; bob's rotation answered with
// stale_epoch makes the client re-encrypt for epoch 2 with the same send
// nonce and enc nonce, and the message shows once, decrypted.
func TestEncSendStaleRetrySameNonce(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	k2 := newK(t)
	e2 := mkBundle(t, d.b1.dev, dm, 2, &d.e1, dmRoster, k2, &d.k1, d.me, d.b1.dev)
	stale := true
	d.f.setOnSend(func(f *e2eFake, id uint32, a proto.SendArgs) {
		if stale {
			stale = false
			f.keyEpoch(dm, bob, e2) // the winner's event comes before our RES
			f.write(proto.TRes, proto.Res{ID: id, Err: proto.Errf(proto.ErrStaleEpoch, "channel is at epoch 2")})
			return
		}
		f.ok(id, proto.SendRes{Msg: f.msgNew(a, alice)})
	})
	if err := d.c.Send(dm, "hi bob", 0); err != nil {
		t.Fatal(err)
	}
	a1, a2 := nextSend(t, d.f), nextSend(t, d.f)
	h1, err1 := e2ecrypto.ParseEnc(a1.Enc)
	h2, err2 := e2ecrypto.ParseEnc(a2.Enc)
	if err1 != nil || err2 != nil || a1.Body != "" || a2.Body != "" {
		t.Fatalf("not encrypted: %+v %+v", a1, a2)
	}
	if !bytes.Equal(a1.Nonce, a2.Nonce) || h1.Nonce != h2.Nonce {
		t.Fatal("retry used a new nonce")
	}
	if h1.Epoch != 1 || h2.Epoch != 2 || h2.Dev != d.me.ID {
		t.Fatalf("epochs %d → %d", h1.Epoch, h2.Epoch)
	}
	if p := openAs(t, k2, d.me, a2.Enc); p.T != "msg" || p.Body != "hi bob" || p.TS == 0 || p.ThreadRef != nil {
		t.Fatalf("plain %+v", p)
	}
	flush(t, d.c)
	if got := bodies(msgsOf(d.c, dm)); got != "hi bob" {
		t.Fatalf("messages %q", got)
	}
	if m := msgsOf(d.c, dm)[0]; m.Sec != SecE2E || m.ID == 0 {
		t.Fatalf("own message %+v", m)
	}
}

// The RES of a stored message is lost, then bob rotates while we're away:
// the retry sends the identical bytes (the server finds them by nonce
// before looking at the epoch), so there's no duplicate and no failure.
func TestEncLostResThenRotationNoDuplicate(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	var stored uint64
	d.f.setOnSend(func(f *e2eFake, id uint32, a proto.SendArgs) {
		stored = f.msgNew(a, alice)
		f.fc.conn.Close() // RES lost
	})
	if err := d.c.Send(dm, "once", 0); err != nil {
		t.Fatal(err)
	}
	a1 := nextSend(t, d.f)
	waitFor(t, "offline", func() bool { return d.c.Status().State != Online })
	k2 := newK(t)
	e2 := mkBundle(t, d.b1.dev, dm, 2, &d.e1, dmRoster, k2, &d.k1, d.me, d.b1.dev)
	f2 := resumeE2E(t, d.fs, d.f, func(f *e2eFake) { f.keyEpoch(dm, bob, e2) })
	f2.setOnSend(func(f *e2eFake, id uint32, a proto.SendArgs) {
		if !bytes.Equal(a.Enc, a1.Enc) {
			f.write(proto.TRes, proto.Res{ID: id, Err: proto.Errf(proto.ErrConflict, "nonce reused for a different message")})
			return
		}
		f.ok(id, proto.SendRes{Msg: stored})
	})
	a2 := nextSend(t, f2)
	if !bytes.Equal(a2.Enc, a1.Enc) || !bytes.Equal(a2.Nonce, a1.Nonce) {
		t.Fatal("the retry re-encrypted a message that may have been stored")
	}
	flush(t, d.c)
	if got := bodies(msgsOf(d.c, dm)); got != "once" {
		t.Fatalf("messages %q", got)
	}
	if n := len(f2.rotates); n != 0 {
		t.Fatalf("%d rotations", n)
	}
}

// What the client accepts from the server (§13.4 "Receiving").
func TestEncReceiveChecks(t *testing.T) {
	// A plaintext message from before encrypted sends (id 5 < epoch 1's seq).
	d := newEncDM(t, proto.Snapshot{Msgs: []proto.Message{{ID: 5, Chan: dm, Author: bob, Body: "old plaintext"}}})
	c, f, b := d.c, d.f, d.b1.dev
	seal := func(p plainMsg) []byte { return sealAs(t, b, d.k1, 1, d.e1, p) }
	msg := func(text string) plainMsg { return plainMsg{T: "msg", TS: 1, Body: text} }

	good := seal(msg("hello alice"))
	root := f.post(proto.Message{Chan: dm, Author: bob, Enc: good})
	rootMid := e2ecrypto.MID(good)
	cases := []struct {
		name string
		m    proto.Message
		want Sec
		body string
	}{
		{"signer is not the author", proto.Message{Chan: dm, Author: carol, Enc: seal(msg("I am carol"))}, SecInvalid, ""},
		{"control characters", proto.Message{Chan: dm, Author: bob, Enc: seal(msg("hi \x1b]0;pwned\x07"))}, SecInvalid, ""},
		{"blank", proto.Message{Chan: dm, Author: bob, Enc: seal(msg(" \n"))}, SecInvalid, ""},
		{"wrong type", proto.Message{Chan: dm, Author: bob, Enc: seal(plainMsg{T: "edit", TS: 1, Body: "x"})}, SecInvalid, ""},
		{"replayed enc", proto.Message{Chan: dm, Author: bob, Enc: good}, SecInvalid, ""},
		{"body and enc", proto.Message{Chan: dm, Author: bob, Body: "smuggled", Enc: seal(msg("x"))}, SecInvalid, ""},
		{"garbage enc", proto.Message{Chan: dm, Author: bob, Enc: bytes.Repeat([]byte{1}, 400)}, SecInvalid, ""},
		{"reply, right thread_ref", proto.Message{Chan: dm, Author: bob, Thread: root,
			Enc: seal(plainMsg{T: "msg", TS: 1, Body: "in thread", ThreadRef: rootMid[:]})}, SecE2E, "in thread"},
		{"reply moved to another thread", proto.Message{Chan: dm, Author: bob, Thread: root,
			Enc: seal(plainMsg{T: "msg", TS: 1, Body: "moved", ThreadRef: bytes.Repeat([]byte{9}, 16)})}, SecInvalid, ""},
		{"reply pulled out of its thread", proto.Message{Chan: dm, Author: bob,
			Enc: seal(plainMsg{T: "msg", TS: 1, Body: "pulled", ThreadRef: rootMid[:]})}, SecInvalid, ""},
		{"top-level message put in a thread", proto.Message{Chan: dm, Author: bob, Thread: root, Enc: seal(msg("pushed"))}, SecInvalid, ""},
		{"plaintext after epoch 1", proto.Message{Chan: dm, Author: bob, Body: "trust me"}, SecInserted, "trust me"},
	}
	ids := make([]uint64, len(cases))
	for i, tc := range cases {
		ids[i] = f.post(tc.m)
	}
	waitFor(t, "applied", func() bool { return lastSeq(t, c) >= ids[len(ids)-1] })
	if m, ok := findMsg(c, root); !ok || m.Sec != SecE2E || m.Body != "hello alice" {
		t.Fatalf("good message %+v", m)
	}
	for i, tc := range cases {
		m, ok := findMsg(c, ids[i])
		if !ok {
			// Thread replies only show in their thread.
			for _, r := range c.Thread(root) {
				if r.ID == ids[i] {
					m, ok = r, true
				}
			}
		}
		if !ok || m.Sec != tc.want || m.Body != tc.body {
			t.Errorf("%s: %+v (found %v), want %s %q", tc.name, m, ok, tc.want, tc.body)
		}
	}
	waitFor(t, "legacy plaintext", func() bool { m, ok := findMsg(c, 5); return ok && m.Sec == SecLegacy })
	if m, _ := findMsg(c, 5); m.Body != "old plaintext" {
		t.Errorf("legacy plaintext %+v", m)
	}
}

// Messages for an epoch whose key isn't here yet wait as ciphertext and
// open when the key arrives; they're announced in Update.New then.
func TestEncLateKeyDecryptsQueued(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	c, f := d.c, d.f
	k2 := newK(t)
	e2 := mkBundle(t, d.b1.dev, dm, 2, &d.e1, dmRoster, k2, &d.k1, d.b1.dev) // no box for us
	f.keyEpoch(dm, bob, e2)
	waitFor(t, "waiting for epoch 2", func() bool { st := c.E2EStatus(dm); return st.State == E2EWaitingKey && st.Epoch == 2 })
	var ids []uint64
	for _, text := range []string{"first", "second"} {
		ids = append(ids, f.post(proto.Message{Chan: dm, Author: bob, Enc: sealAs(t, d.b1.dev, k2, 2, e2, plainMsg{T: "msg", TS: 1, Body: text})}))
	}
	waitFor(t, "applied", func() bool { return lastSeq(t, c) == ids[1] })
	for _, id := range ids {
		if m, _ := findMsg(c, id); m.Sec != SecWaiting || m.Body != "" {
			t.Fatalf("before the key: %+v", m)
		}
	}
	drain(c)
	eph, ct, err := e2ecrypto.SealShare(rand.Reader, d.me.X25519Pub, e2ecrypto.ShareAD{ServerID: testSID(), Chan: dm, Epoch: 2, Recipient: d.me.ID}, k2)
	if err != nil {
		t.Fatal(err)
	}
	f.event(proto.KKeyShare, dm, bob, proto.KeyShareArgs{Chan: dm, Epoch: 2, Dev: d.me.ID[:], Eph: eph[:], CT: ct[:]})
	waitFor(t, "decrypted", func() bool { return bodies(msgsOf(c, dm)) == "first,second" })
	for _, id := range ids {
		if m, _ := findMsg(c, id); m.Sec != SecE2E {
			t.Fatalf("after the key: %+v", m)
		}
	}
	got := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case u := <-c.Updates():
			for _, m := range u.New {
				got[m.Body] = m.Sec == SecE2E
			}
		case <-deadline:
			t.Fatalf("Update.New got %v", got)
		}
	}
	if !got["first"] || !got["second"] {
		t.Fatalf("Update.New got %v", got)
	}
}

func drain(c *Client) {
	for {
		select {
		case <-c.Updates():
		default:
			return
		}
	}
}

// Without a key for the head (no box, nobody shared yet) a send fails with
// a clear reason and asks for the key; once it arrives the message goes out.
func TestEncNoKeyFailsThenRequeues(t *testing.T) {
	fs, c := e2eClient(t)
	c.timing.devWait = 5 * time.Second
	c.timing.keyNeed = time.Hour // only the failed send asks again
	run(t, c)
	b1 := newPeer(t, bob, nil)
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)}, Devs: []proto.Device{{Cert: b1.cert}}}, proto.DevicesListRes{})
	me := f.myKeys(c)
	k1 := newK(t)
	e1 := mkBundle(t, b1.dev, dm, 1, nil, dmRoster, k1, nil, b1.dev) // no box for alice
	f.keyEpoch(dm, bob, e1)
	waitFor(t, "waiting for a key", status(c, dm, E2EWaitingKey))
	for len(f.needs) > 0 {
		<-f.needs
	}
	c.mu.Lock()
	c.needSent = map[uint64]time.Time{}
	c.mu.Unlock()
	if err := c.Send(dm, "are you there", 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Flush(ctx); err == nil || !strings.Contains(err.Error(), noKeyReason) {
		t.Fatalf("flush: %v", err)
	}
	if ms := msgsOf(c, dm); len(ms) != 1 || ms[0].Failed != noKeyReason {
		t.Fatalf("messages %+v", ms)
	}
	select {
	case n := <-f.needs:
		if n.Chan != dm || n.Epoch != 1 {
			t.Fatalf("key.need %+v", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no key.need")
	}
	if len(f.sends) != 0 {
		t.Fatal("sent without a key")
	}
	eph, ct, err := e2ecrypto.SealShare(rand.Reader, me.X25519Pub, e2ecrypto.ShareAD{ServerID: testSID(), Chan: dm, Epoch: 1, Recipient: me.ID}, k1)
	if err != nil {
		t.Fatal(err)
	}
	f.event(proto.KKeyShare, dm, bob, proto.KeyShareArgs{Chan: dm, Epoch: 1, Dev: me.ID[:], Eph: eph[:], CT: ct[:]})
	a := nextSend(t, f)
	if p := openAs(t, k1, me, a.Enc); p.Body != "are you there" {
		t.Fatalf("plain %+v", p)
	}
	flush(t, c)
}

// Editing a message of an encrypted channel would send the new text in the
// clear: refused locally until encrypted edits exist.
func TestEncEditRefused(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	id := d.f.post(proto.Message{Chan: dm, Author: alice, Enc: sealAs(t, d.me, d.k1, 1, d.e1, plainMsg{T: "msg", TS: 1, Body: "typo"})})
	waitFor(t, "applied", func() bool { return lastSeq(t, d.c) >= id })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.c.Edit(ctx, id, "fixed"); !errors.Is(err, ErrEncryptedEdit) {
		t.Fatalf("edit: %v", err)
	}
	// Text the server can't vet is checked before it's encrypted.
	var pe *proto.Error
	if err := d.c.Send(dm, "bell \a", 0); !errors.As(err, &pe) || pe.Code != proto.ErrBadRequest {
		t.Fatalf("control characters: %v", err)
	}
}

// A send into a channel in alarm is refused at once.
func TestEncSendBlockedInAlarm(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	if err := d.c.Trust().RaiseAlarm(testSID(), dm, "fork: test"); err != nil {
		t.Fatal(err)
	}
	if err := d.c.Send(dm, "hi", 0); !errors.Is(err, ErrE2EAlarm) {
		t.Fatalf("send: %v", err)
	}
	if n := outboxLen(t, d.c); n != 0 {
		t.Fatalf("outbox %d", n)
	}
}

// Local search finds decrypted messages the server can't search.
func TestEncLocalSearch(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	id := d.f.post(proto.Message{Chan: dm, Author: bob, Enc: sealAs(t, d.b1.dev, d.k1, 1, d.e1, plainMsg{T: "msg", TS: 1, Body: "the Zebra plan"})})
	waitFor(t, "applied", func() bool { return lastSeq(t, d.c) >= id })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := d.c.Search(ctx, "zebra PLAN", 0)
	if err != nil || len(got) != 1 || got[0].ID != id || got[0].Body != "the Zebra plan" {
		t.Fatalf("search: %+v %v", got, err)
	}
	if got, _ := d.c.Search(ctx, "zebra nope", 0); len(got) != 0 {
		t.Fatalf("all terms must match: %+v", got)
	}
}
