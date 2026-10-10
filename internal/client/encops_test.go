package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// Encrypted edits, deletes and reactions (step 8) against a scripted
// server: what the client applies, and what it sends.

// sealP encrypts any Plain into dm at epoch (bundle b, key k) as dev.
func sealP(t *testing.T, dev *e2ecrypto.DeviceKeys, k e2ecrypto.Key, epoch uint32, b proto.Signed, p any) []byte {
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

func openP(t *testing.T, k e2ecrypto.Key, signer *e2ecrypto.DeviceKeys, enc []byte, p any) {
	t.Helper()
	raw, err := e2ecrypto.Decrypt(k, signer.Ed25519Pub, testSID(), dm, enc)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(raw, p); err != nil {
		t.Fatal(err)
	}
}

// opsDM is an encDM with carol's device verified too (a member of other
// channels, not of the DM) and helpers for bob's messages.
type opsDM struct {
	*encDM
	carol peer
}

func newOpsDM(t *testing.T) *opsDM {
	t.Helper()
	carolP := newPeer(t, carol, nil)
	d := &opsDM{encDM: newEncDM(t, proto.Snapshot{Devs: []proto.Device{{Cert: carolP.cert}}}), carol: carolP}
	waitFor(t, "carol verified", func() bool {
		_, ok, _ := d.c.Trust().Device(testSID(), carolP.dev.ID)
		return ok
	})
	return d
}

// post sends a msg.new by author (device dev) and waits until it's applied.
func (d *opsDM) post(t *testing.T, author uint64, dev *e2ecrypto.DeviceKeys, body string) (uint64, []byte) {
	t.Helper()
	enc := sealAs(t, dev, d.k1, 1, d.e1, plainMsg{T: "msg", TS: 1, Body: body})
	id := d.f.post(proto.Message{Chan: dm, Author: author, Enc: enc})
	d.wait(t, id)
	mid := e2ecrypto.MID(enc)
	return id, mid[:]
}

func (d *opsDM) wait(t *testing.T, seq uint64) {
	t.Helper()
	waitFor(t, "applied", func() bool { return lastSeq(t, d.c) >= seq })
}

// event sends an event and waits until it's applied.
func (d *opsDM) event(t *testing.T, kind string, actor uint64, data any) {
	t.Helper()
	d.wait(t, d.f.event(kind, dm, actor, data))
}

func (d *opsDM) edit(t *testing.T, signer *e2ecrypto.DeviceKeys, v uint32, body string, ref []byte) []byte {
	t.Helper()
	return sealP(t, signer, d.k1, 1, d.e1, plainEdit{T: "edit", TS: 100 + int64(v), Body: body, Ref: ref, V: v})
}

func (d *opsDM) msg(t *testing.T, id uint64) Message {
	t.Helper()
	m, ok := findMsg(d.c, id)
	if !ok {
		t.Fatalf("message %d not cached", id)
	}
	return m
}

func rejected(c *Client, part string) bool {
	for _, r := range c.E2ERejects() {
		if strings.Contains(r, part) {
			return true
		}
	}
	return false
}

// A verified edit applies; a replayed older version or the same edit again
// changes nothing.
func TestEncEditApplies(t *testing.T) {
	d := newOpsDM(t)
	id, mid := d.post(t, bob, d.b1.dev, "helo")
	d.event(t, proto.KMsgEdit, bob, proto.MsgEdit{Msg: id, Edited: 77, Enc: d.edit(t, d.b1.dev, 1, "hello", mid)})
	if m := d.msg(t, id); m.Body != "hello" || m.Edited != 101 || m.Sec != SecE2E {
		t.Fatalf("after edit %+v", m)
	}
	v2 := d.edit(t, d.b1.dev, 2, "hello v2", mid)
	d.event(t, proto.KMsgEdit, bob, proto.MsgEdit{Msg: id, Edited: 78, Enc: v2})
	for name, enc := range map[string][]byte{
		"older version":          d.edit(t, d.b1.dev, 1, "rolled back", mid),
		"same version, older ts": sealP(t, d.b1.dev, d.k1, 1, d.e1, plainEdit{T: "edit", TS: 1, Body: "other v2", Ref: mid, V: 2}),
		"same bytes":             v2,
	} {
		d.event(t, proto.KMsgEdit, bob, proto.MsgEdit{Msg: id, Edited: 79, Enc: enc})
		if m := d.msg(t, id); m.Body != "hello v2" || m.Edited != 102 {
			t.Fatalf("%s applied: %+v", name, m)
		}
	}
	if !rejected(d.c, "not newer") {
		t.Fatalf("no reason logged: %q", d.c.E2ERejects())
	}
}

// Edits signed by anyone but the author, made for another message, of
// another type or with bad text change nothing; nor do refetched copies
// that carry them, while a refetched verified edit applies.
func TestEncEditForgedOrMoved(t *testing.T) {
	d := newOpsDM(t)
	a, midA := d.post(t, bob, d.b1.dev, "first")
	b, midB := d.post(t, bob, d.b1.dev, "second")
	cases := map[string][]byte{
		"signed by another member": d.edit(t, d.me, 1, "alice says", midA),
		"signed by a non-member":   d.edit(t, d.carol.dev, 1, "carol says", midA),
		"made for another message": d.edit(t, d.b1.dev, 1, "moved", midB),
		"not an edit":              sealAs(t, d.b1.dev, d.k1, 1, d.e1, plainMsg{T: "msg", TS: 2, Body: "msg as edit"}),
		"control characters":       d.edit(t, d.b1.dev, 1, "hi \x1b[2J", midA),
		"no ref":                   d.edit(t, d.b1.dev, 1, "no ref", nil),
		"garbage":                  []byte("not an enc"),
		"other epoch, unknown key": sealP(t, d.b1.dev, newK(t), 7, d.e1, plainEdit{T: "edit", TS: 2, Body: "x", Ref: midA, V: 1}),
	}
	for name, enc := range cases {
		d.event(t, proto.KMsgEdit, bob, proto.MsgEdit{Msg: a, Edited: 5, Enc: enc})
		if m := d.msg(t, a); m.Body != "first" || m.Edited != 0 || m.Sec != SecE2E {
			t.Fatalf("%s: %+v", name, m)
		}
	}
	// Refetched copies (history): a moved edit, a server-claimed edited
	// time, swapped original bytes with an edit: nothing changes.
	var origA []byte
	d.c.db.QueryRow(`SELECT enc FROM msgs WHERE id = ?`, a).Scan(&origA)
	other := sealAs(t, d.b1.dev, d.k1, 1, d.e1, plainMsg{T: "msg", TS: 1, Body: "swapped"})
	midOther := e2ecrypto.MID(other)
	for name, m := range map[string]proto.Message{
		"moved edit":    {ID: a, Chan: dm, Author: bob, Enc: origA, Edited: 9, EditEnc: d.edit(t, d.b1.dev, 1, "moved", midB)},
		"edited claim":  {ID: a, Chan: dm, Author: bob, Enc: origA, Edited: 9},
		"no enc":        {ID: a, Chan: dm, Author: bob, Body: "server text"},
		"no enc, moved": {ID: a, Chan: general, Author: carol, Body: "server text"},
		"swapped bytes": {ID: a, Chan: dm, Author: bob, Enc: other, Edited: 9, EditEnc: d.edit(t, d.b1.dev, 1, "x", midOther[:])},
	} {
		if err := d.c.saveMsgs([]proto.Message{m}); err != nil {
			t.Fatal(err)
		}
		if got := d.msg(t, a); got.Body != "first" || got.Edited != 0 || got.Sec != SecE2E {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	// A verified edit in a refetched copy applies.
	var origB []byte
	d.c.db.QueryRow(`SELECT enc FROM msgs WHERE id = ?`, b).Scan(&origB)
	if err := d.c.saveMsgs([]proto.Message{{ID: b, Chan: dm, Author: bob, Enc: origB, Edited: 9,
		EditEnc: d.edit(t, d.b1.dev, 1, "second, edited", midB)}}); err != nil {
		t.Fatal(err)
	}
	if got := d.msg(t, b); got.Body != "second, edited" || got.Edited != 101 {
		t.Fatalf("refetched edit %+v", got)
	}
	// And a later refetch without it (or with an older one) keeps it.
	if err := d.c.saveMsgs([]proto.Message{{ID: b, Chan: dm, Author: bob, Enc: origB}}); err != nil {
		t.Fatal(err)
	}
	if got := d.msg(t, b); got.Body != "second, edited" || got.Edited != 101 {
		t.Fatalf("refetch dropped the edit: %+v", got)
	}
}

// serveOps answers edit/delete/react like a server: it records the args
// and emits the event (actor alice).
type opLog struct {
	mu    sync.Mutex
	edits []proto.EditArgs
	dels  []proto.DeleteArgs
	reqs  []proto.ReactArgs
}

func serveOps(d *opsDM) *opLog {
	l := &opLog{}
	d.f.mu.Lock()
	d.f.onOp = func(f *e2eFake, r proto.Req) bool {
		var seq uint64
		switch r.Op {
		case proto.OpEdit:
			var a proto.EditArgs
			proto.Unmarshal(r.Args, &a)
			l.mu.Lock()
			l.edits = append(l.edits, a)
			l.mu.Unlock()
			seq = f.event(proto.KMsgEdit, dm, alice, proto.MsgEdit{Msg: a.Msg, Body: a.Body, Edited: 1234, Enc: a.Enc})
		case proto.OpDelete:
			var a proto.DeleteArgs
			proto.Unmarshal(r.Args, &a)
			l.mu.Lock()
			l.dels = append(l.dels, a)
			l.mu.Unlock()
			seq = f.event(proto.KMsgDelete, dm, alice, proto.MsgDelete(a))
		case proto.OpReact, proto.OpUnreact:
			var a proto.ReactArgs
			proto.Unmarshal(r.Args, &a)
			l.mu.Lock()
			l.reqs = append(l.reqs, a)
			l.mu.Unlock()
			kind := proto.KReactAdd
			if r.Op == proto.OpUnreact {
				kind = proto.KReactRemove
			}
			seq = f.event(kind, dm, alice, a)
		default:
			return false
		}
		f.ok(r.ID, proto.SeqRes{Seq: seq})
		return true
	}
	d.f.mu.Unlock()
	return l
}

func ctx5(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Our own edits go out encrypted with the current key, refer to the
// message and count up; the echoed event applies them. Text that receivers
// would reject is refused before anything is sent.
func TestEncEditOwn(t *testing.T) {
	d := newOpsDM(t)
	l := serveOps(d)
	id, mid := d.post(t, alice, d.me, "tpyo")
	ctx := ctx5(t)
	for i, body := range []string{"typo", "typo fixed"} {
		if err := d.c.Edit(ctx, id, body); err != nil {
			t.Fatal(err)
		}
		a := l.edits[i]
		var p plainEdit
		openP(t, d.k1, d.me, a.Enc, &p)
		if a.Body != "" || a.Msg != id || p.T != "edit" || p.Body != body || string(p.Ref) != string(mid) || p.V != uint32(i+1) || p.TS == 0 {
			t.Fatalf("edit %d: %+v %+v", i, a, p)
		}
		if m := d.msg(t, id); m.Body != body || m.Edited == 1234 || m.Edited < 1000000 {
			t.Fatalf("after edit %d: %+v", i, m)
		}
	}
	var pe *proto.Error
	if err := d.c.Edit(ctx, id, "bell \a"); !errors.As(err, &pe) || pe.Code != proto.ErrBadRequest || len(l.edits) != 2 {
		t.Fatalf("control characters: %v (%d sent)", err, len(l.edits))
	}
	// Our delete is signed, under the message's own epoch.
	if err := d.c.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	var p plainDel
	openP(t, d.k1, d.me, l.dels[0].Enc, &p)
	if p.T != "del" || string(p.Ref) != string(mid) {
		t.Fatalf("delete %+v", p)
	}
	if m := d.msg(t, id); !m.Deleted || m.Sec != SecE2E || m.Body != "" {
		t.Fatalf("after delete %+v", m)
	}
}

// A delete signed by the author hides the message; one without the
// author's signature (an admin, the server, another member's device, a
// delete made for another message) shows as removed (not by author). The
// text goes either way, also from local search.
func TestEncDeleteSignedVsUnsigned(t *testing.T) {
	d := newOpsDM(t)
	del := func(signer *e2ecrypto.DeviceKeys, ref []byte) []byte {
		return sealP(t, signer, d.k1, 1, d.e1, plainDel{T: "del", TS: 3, Ref: ref})
	}
	a, midA := d.post(t, bob, d.b1.dev, "secret a")
	b, _ := d.post(t, bob, d.b1.dev, "secret b")
	c, _ := d.post(t, bob, d.b1.dev, "secret c")
	e, _ := d.post(t, bob, d.b1.dev, "secret e")
	d.event(t, proto.KMsgDelete, bob, proto.MsgDelete{Msg: a, Enc: del(d.b1.dev, midA)})
	d.event(t, proto.KMsgDelete, carol, proto.MsgDelete{Msg: b})
	d.event(t, proto.KMsgDelete, alice, proto.MsgDelete{Msg: c, Enc: del(d.me, midA)})
	d.event(t, proto.KMsgDelete, bob, proto.MsgDelete{Msg: e, Enc: del(d.b1.dev, midA)}) // made for a
	want := map[uint64]Sec{a: SecE2E, b: SecRemoved, c: SecRemoved, e: SecRemoved}
	for id, sec := range want {
		if m := d.msg(t, id); !m.Deleted || m.Body != "" || m.Sec != sec {
			t.Errorf("message %d: %+v, want %s", id, m, sec)
		}
	}
	if got, _ := d.c.Search(ctx5(t), "secret", dm); len(got) != 0 {
		t.Fatalf("deleted text still searchable: %+v", got)
	}
	// Deleted messages first seen in a refetch: only the server's claimed
	// mid (Message.mid) can be checked against the delete's ref; without one,
	// or with one that's another message's, it isn't the author's delete.
	midZ := e2ecrypto.MID(sealAs(t, d.b1.dev, d.k1, 1, d.e1, plainMsg{T: "msg", TS: 1, Body: "never seen"}))
	midY := e2ecrypto.MID(sealAs(t, d.b1.dev, d.k1, 1, d.e1, plainMsg{T: "msg", TS: 1, Body: "never seen either"}))
	if err := d.c.saveMsgs([]proto.Message{
		{ID: 900, Chan: dm, Author: bob, Deleted: true, DelEnc: del(d.b1.dev, midZ[:]), Mid: midZ[:]},
		{ID: 901, Chan: dm, Author: bob, Deleted: true, Mid: midY[:]},
		{ID: 902, Chan: dm, Author: bob, Deleted: true, DelEnc: del(d.carol.dev, midY[:]), Mid: midY[:]},
		{ID: 903, Chan: dm, Author: bob, Deleted: true, DelEnc: del(d.b1.dev, midY[:])},
		{ID: 904, Chan: dm, Author: bob, Deleted: true, DelEnc: del(d.b1.dev, midA), Mid: midA},
		{ID: 905, Chan: dm, Author: bob, Deleted: true, DelEnc: del(d.b1.dev, midY[:]), Mid: midZ[:]},
	}); err != nil {
		t.Fatal(err)
	}
	for id, sec := range map[uint64]Sec{900: SecE2E, 901: SecRemoved, 902: SecRemoved, 903: SecRemoved, 904: SecRemoved, 905: SecRemoved} {
		if m := d.msg(t, id); !m.Deleted || m.Sec != sec {
			t.Errorf("refetched %d: %+v, want %s", id, m, sec)
		}
	}
	// A refetch of a, deleted, doesn't lose who deleted it or its mid.
	if err := d.c.saveMsgs([]proto.Message{{ID: a, Chan: dm, Author: bob, Deleted: true, DelEnc: del(d.b1.dev, midA)}}); err != nil {
		t.Fatal(err)
	}
	var mid []byte
	d.c.db.QueryRow(`SELECT mid FROM msgs WHERE id = ?`, a).Scan(&mid)
	if m := d.msg(t, a); m.Sec != SecE2E || string(mid) != string(midA) {
		t.Fatalf("refetched a %+v mid %x", m, mid)
	}
}

// Reactions in an encrypted channel: the server sees a tag; only reactions
// whose enc verifies show, by their real emoji, with set semantics.
func TestEncReactions(t *testing.T) {
	d := newOpsDM(t)
	l := serveOps(d)
	id, mid := d.post(t, bob, d.b1.dev, "react to me")
	_, midO := d.post(t, bob, d.b1.dev, "other")
	tagOf := func(emoji string, m []byte) string {
		tg, err := e2ecrypto.ReactTag(d.k1, testSID(), dm, 1, e2ecrypto.MsgID(m), emoji)
		if err != nil {
			t.Fatal(err)
		}
		return tg
	}
	renc := func(signer *e2ecrypto.DeviceKeys, emoji string, ref []byte, add bool) []byte {
		return sealP(t, signer, d.k1, 1, d.e1, plainReact{T: "react", TS: 4, Ref: ref, Emoji: emoji, Add: add})
	}
	thumbs := tagOf("👍", mid)
	good := renc(d.b1.dev, "👍", mid, true)
	shown := func() map[string][]uint64 { return d.msg(t, id).Reactions }
	for name, r := range map[string]proto.Reaction{
		"bad signer":    {Msg: id, Emoji: thumbs, Enc: renc(d.carol.dev, "👍", mid, true)},
		"wrong ref":     {Msg: id, Emoji: tagOf("🎉", mid), Enc: renc(d.b1.dev, "🎉", midO, true)},
		"tag mismatch":  {Msg: id, Emoji: tagOf("😀", mid), Enc: renc(d.b1.dev, "🎉", mid, true)},
		"tag of other":  {Msg: id, Emoji: tagOf("🎉", midO), Enc: renc(d.b1.dev, "🎉", mid, true)},
		"remove as add": {Msg: id, Emoji: tagOf("🙂", mid), Enc: renc(d.b1.dev, "🙂", mid, false)},
		"no enc":        {Msg: id, Emoji: tagOf("🚀", mid)},
		"plain emoji":   {Msg: id, Emoji: "👀"},
		"garbage enc":   {Msg: id, Emoji: tagOf("🔥", mid), Enc: []byte("junk")},
	} {
		d.event(t, proto.KReactAdd, bob, r)
		if r := shown(); len(r) != 0 {
			t.Fatalf("%s shown: %v", name, r)
		}
	}
	d.event(t, proto.KReactAdd, bob, proto.Reaction{Msg: id, Emoji: thumbs, Enc: good})
	if r := shown(); len(r) != 1 || len(r["👍"]) != 1 || r["👍"][0] != bob {
		t.Fatalf("verified reaction %v", r)
	}
	// Ours: the server gets only the tag, the same one bob's 👍 has.
	ctx := ctx5(t)
	if err := d.c.React(ctx, id, "👍"); err != nil {
		t.Fatal(err)
	}
	a := l.reqs[0]
	if a.Emoji != thumbs || strings.Contains(a.Emoji, "👍") || !e2ecrypto.ValidReactTag(a.Emoji) {
		t.Fatalf("react args %+v", a)
	}
	var p plainReact
	openP(t, d.k1, d.me, a.Enc, &p)
	if p.T != "react" || p.Emoji != "👍" || !p.Add || string(p.Ref) != string(mid) {
		t.Fatalf("react plain %+v", p)
	}
	if r := shown(); len(r) != 1 || len(r["👍"]) != 2 {
		t.Fatalf("after our reaction %v", r)
	}
	if err := d.c.Unreact(ctx, id, "👍"); err != nil {
		t.Fatal(err)
	}
	openP(t, d.k1, d.me, l.reqs[1].Enc, &p)
	if l.reqs[1].Emoji != thumbs || p.Add {
		t.Fatalf("unreact %+v %+v", l.reqs[1], p)
	}
	if r := shown(); len(r["👍"]) != 1 || r["👍"][0] != bob {
		t.Fatalf("after unreact %v", r)
	}
	// A refetch shows what its encs prove: tags without one are ignored.
	var orig []byte
	d.c.db.QueryRow(`SELECT enc FROM msgs WHERE id = ?`, id).Scan(&orig)
	party := tagOf("🎉", mid)
	if err := d.c.saveMsgs([]proto.Message{{ID: id, Chan: dm, Author: bob, Enc: orig,
		Reactions: map[string][]uint64{thumbs: {bob}, party: {bob, carol}},
		Renc:      []proto.ReactEnc{{User: bob, Emoji: thumbs, Enc: good}, {User: bob, Emoji: party, Enc: renc(d.b1.dev, "🎉", mid, true)}}}}); err != nil {
		t.Fatal(err)
	}
	if r := shown(); len(r) != 2 || len(r["👍"]) != 1 || len(r["🎉"]) != 1 {
		t.Fatalf("refetched reactions %v", r)
	}
	d.event(t, proto.KReactRemove, bob, proto.Reaction{Msg: id, Emoji: thumbs})
	if r := shown(); len(r) != 1 || len(r["🎉"]) != 1 {
		t.Fatalf("after bob's unreact %v", r)
	}
}

// Local search follows encrypted edits and deletes.
func TestEncLocalSearchFollowsEdits(t *testing.T) {
	d := newOpsDM(t)
	id, mid := d.post(t, bob, d.b1.dev, "the zebra plan")
	search := func(q string) int {
		got, err := d.c.Search(ctx5(t), q, dm)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if search("zebra") != 1 {
		t.Fatal("not found before the edit")
	}
	d.event(t, proto.KMsgEdit, bob, proto.MsgEdit{Msg: id, Edited: 5, Enc: d.edit(t, d.b1.dev, 1, "the giraffe plan", mid)})
	if search("zebra") != 0 || search("giraffe") != 1 {
		t.Fatal("search doesn't follow the edit")
	}
	d.event(t, proto.KMsgDelete, bob, proto.MsgDelete{Msg: id, Enc: sealP(t, d.b1.dev, d.k1, 1, d.e1, plainDel{T: "del", TS: 3, Ref: mid})})
	if search("giraffe") != 0 || search("plan") != 0 {
		t.Fatal("deleted message still found")
	}
}

// An edit, a reaction and a delete of messages still waiting for their
// key are kept and judged once the key arrives.
func TestEncOpsWaitForKey(t *testing.T) {
	d := newOpsDM(t)
	k2 := newK(t)
	e2 := mkBundle(t, d.b1.dev, dm, 2, &d.e1, dmRoster, k2, &d.k1, d.b1.dev) // no box for us
	d.f.keyEpoch(dm, bob, e2)
	waitFor(t, "waiting for epoch 2", func() bool { st := d.c.E2EStatus(dm); return st.State == E2EWaitingKey && st.Epoch == 2 })
	seal := func(p any) []byte { return sealP(t, d.b1.dev, k2, 2, e2, p) }
	encA, encB := seal(plainMsg{T: "msg", TS: 1, Body: "draft"}), seal(plainMsg{T: "msg", TS: 1, Body: "doomed"})
	a := d.f.post(proto.Message{Chan: dm, Author: bob, Enc: encA})
	b := d.f.post(proto.Message{Chan: dm, Author: bob, Enc: encB})
	midA, midB := e2ecrypto.MID(encA), e2ecrypto.MID(encB)
	d.event(t, proto.KMsgEdit, bob, proto.MsgEdit{Msg: a, Edited: 9, Enc: seal(plainEdit{T: "edit", TS: 2, Body: "final", Ref: midA[:], V: 1})})
	tag, _ := e2ecrypto.ReactTag(k2, testSID(), dm, 2, midA, "🎉")
	d.event(t, proto.KReactAdd, bob, proto.Reaction{Msg: a, Emoji: tag, Enc: seal(plainReact{T: "react", TS: 2, Ref: midA[:], Emoji: "🎉", Add: true})})
	d.event(t, proto.KMsgDelete, bob, proto.MsgDelete{Msg: b, Enc: seal(plainDel{T: "del", TS: 2, Ref: midB[:]})})
	if m := d.msg(t, a); m.Sec != SecWaiting || m.Body != "" || len(m.Reactions) != 0 {
		t.Fatalf("before the key: %+v", m)
	}
	if m := d.msg(t, b); !m.Deleted || m.Sec != SecWaiting {
		t.Fatalf("delete before the key: %+v", m)
	}
	eph, ct, err := e2ecrypto.SealShare(rand.Reader, d.me.X25519Pub, e2ecrypto.ShareAD{ServerID: testSID(), Chan: dm, Epoch: 2, Recipient: d.me.ID}, k2)
	if err != nil {
		t.Fatal(err)
	}
	d.f.event(proto.KKeyShare, dm, bob, proto.KeyShareArgs{Chan: dm, Epoch: 2, Dev: d.me.ID[:], Eph: eph[:], CT: ct[:]})
	waitFor(t, "edit and reaction applied", func() bool {
		m := d.msg(t, a)
		return m.Sec == SecE2E && m.Body == "final" && m.Edited == 2 && len(m.Reactions["🎉"]) == 1
	})
	waitFor(t, "signed delete judged", func() bool { m := d.msg(t, b); return m.Deleted && m.Sec == SecE2E })
}

// Two of bob's devices edit the same message at once with the same v: the
// higher (v, ts, mid) wins whatever order the edits arrive in, so every
// client shows the same text.
func TestEncConcurrentEditsConverge(t *testing.T) {
	d := newOpsDM(t)
	b2 := newPeer(t, bob, d.b1.ssh) // bob's second device, same SSH key
	d.event(t, proto.KDeviceNew, bob, proto.Device{Cert: b2.cert})
	waitFor(t, "b2 verified", func() bool { _, ok, _ := d.c.Trust().Device(testSID(), b2.dev.ID); return ok })
	type edit struct {
		enc  []byte
		body string
	}
	mk := func(dev *e2ecrypto.DeviceKeys, ts int64, body string, mid []byte) edit {
		return edit{sealP(t, dev, d.k1, 1, d.e1, plainEdit{T: "edit", TS: ts, Body: body, Ref: mid, V: 1}), body}
	}
	for _, sameTS := range []bool{false, true} {
		var want []string
		var got []string
		for _, reversed := range []bool{false, true} {
			id, mid := d.post(t, bob, d.b1.dev, "orig")
			tsY := int64(20)
			if sameTS {
				tsY = 10
			}
			x, y := mk(d.b1.dev, 10, "from b1", mid), mk(b2.dev, tsY, "from b2", mid)
			win := y.body
			if mx, my := e2ecrypto.MID(x.enc), e2ecrypto.MID(y.enc); sameTS && bytes.Compare(mx[:], my[:]) > 0 {
				win = x.body
			}
			order := []edit{x, y}
			if reversed {
				order = []edit{y, x}
			}
			for _, e := range order {
				d.event(t, proto.KMsgEdit, bob, proto.MsgEdit{Msg: id, Edited: 5, Enc: e.enc})
			}
			want = append(want, win)
			got = append(got, d.msg(t, id).Body)
		}
		if got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("same ts %v: got %q, want %q", sameTS, got, want)
		}
		if !sameTS && (got[0] != "from b2" || got[1] != "from b2") {
			t.Fatalf("the later ts didn't win: %q", got)
		}
	}
}

// Deletes and reactions into a channel in alarm are refused locally, like
// sends and edits.
func TestEncOpsBlockedInAlarm(t *testing.T) {
	d := newOpsDM(t)
	l := serveOps(d)
	mine, _ := d.post(t, alice, d.me, "mine")
	theirs, _ := d.post(t, bob, d.b1.dev, "theirs")
	if err := d.c.Trust().RaiseAlarm(testSID(), dm, "fork: test"); err != nil {
		t.Fatal(err)
	}
	ctx := ctx5(t)
	for name, err := range map[string]error{
		"delete":  d.c.Delete(ctx, mine),
		"react":   d.c.React(ctx, theirs, "👍"),
		"unreact": d.c.Unreact(ctx, theirs, "👍"),
		"edit":    d.c.Edit(ctx, mine, "changed"),
	} {
		if !errors.Is(err, ErrE2EAlarm) {
			t.Errorf("%s: %v", name, err)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.dels)+len(l.reqs)+len(l.edits) != 0 {
		t.Fatal("something was sent")
	}
}

// Reacting to or deleting a message that isn't cached is refused: its
// channel (encrypted? in alarm?) is unknown, so a plaintext emoji could
// land in an encrypted channel.
func TestEncUncachedOpsRefused(t *testing.T) {
	d := newOpsDM(t)
	l := serveOps(d)
	ctx := ctx5(t)
	for name, err := range map[string]error{
		"react":   d.c.React(ctx, 4242, "👍"),
		"unreact": d.c.Unreact(ctx, 4242, "👍"),
		"delete":  d.c.Delete(ctx, 4242),
	} {
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.ErrNotFound {
			t.Errorf("%s: %v", name, err)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.dels)+len(l.reqs) != 0 {
		t.Fatal("something was sent")
	}
}

// What waits for a key is bounded per message and per channel, and the
// reactions of deleted messages aren't kept.
func TestEncPendingBounded(t *testing.T) {
	defer func(m, c int) { maxPendingPerMsg, maxPendingPerChan = m, c }(maxPendingPerMsg, maxPendingPerChan)
	maxPendingPerMsg, maxPendingPerChan = 3, 5
	d := newOpsDM(t)
	k2 := newK(t)
	e2 := mkBundle(t, d.b1.dev, dm, 2, &d.e1, dmRoster, k2, &d.k1, d.b1.dev) // no box for us
	d.f.keyEpoch(dm, bob, e2)
	waitFor(t, "waiting for epoch 2", func() bool { st := d.c.E2EStatus(dm); return st.State == E2EWaitingKey && st.Epoch == 2 })
	seal := func(p any) []byte { return sealP(t, d.b1.dev, k2, 2, e2, p) }
	var ids []uint64
	for i := range 3 {
		enc := seal(plainMsg{T: "msg", TS: 1, Body: fmt.Sprint("waiting ", i)})
		id := d.f.post(proto.Message{Chan: dm, Author: bob, Enc: enc})
		ids = append(ids, id)
		mid := e2ecrypto.MID(enc)
		for j := range 4 {
			emoji := fmt.Sprint(j)
			tag, _ := e2ecrypto.ReactTag(k2, testSID(), dm, 2, mid, emoji)
			d.event(t, proto.KReactAdd, bob, proto.Reaction{Msg: id, Emoji: tag, Enc: seal(plainReact{T: "react", TS: 2, Ref: mid[:], Emoji: emoji, Add: true})})
		}
	}
	count := func(q string, args ...any) int {
		var n int
		d.c.db.QueryRow(q, args...).Scan(&n)
		return n
	}
	for _, id := range ids {
		if n := count(`SELECT COUNT(*) FROM rencs WHERE msg = ? AND st = 0`, id); n > maxPendingPerMsg {
			t.Fatalf("message %d keeps %d pending reactions", id, n)
		}
	}
	if n := count(`SELECT COUNT(*) FROM rencs WHERE st = 0`); n != maxPendingPerChan {
		t.Fatalf("channel keeps %d pending reactions, want %d", n, maxPendingPerChan)
	}
	// Deleting a message drops its reactions.
	d.event(t, proto.KMsgDelete, carol, proto.MsgDelete{Msg: ids[0]})
	if n := count(`SELECT COUNT(*) FROM rencs WHERE msg = ?`, ids[0]); n != 0 {
		t.Fatalf("deleted message keeps %d reactions", n)
	}
}

// Step-7 caches don't know who deleted an encrypted message: migrating
// shows those as not by the author.
func TestEncMigrateStep7Deletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	db, err := openCache(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`INSERT INTO msgs (id, chan, author, body, thread, time, edited, deleted, reactions, sec, mid) VALUES (1, 5, 2, '', 0, 1, 0, 1, '{}', 1, x'01')`,
		`INSERT INTO msgs (id, chan, author, body, thread, time, edited, deleted, reactions, sec, mid) VALUES (2, 5, 2, 'live', 0, 1, 0, 0, '{}', 1, x'02')`,
		`INSERT INTO msgs (id, chan, author, body, thread, time, edited, deleted, reactions, sec) VALUES (3, 5, 2, '', 0, 1, 0, 1, '{}', 0)`,
		`ALTER TABLE msgs DROP COLUMN denc`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if db, err = openCache(path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for id, want := range map[int]Sec{1: SecRemoved, 2: SecE2E, 3: SecPlain} {
		var sec Sec
		if err := db.QueryRow(`SELECT sec FROM msgs WHERE id = ?`, id).Scan(&sec); err != nil || sec != want {
			t.Errorf("message %d: %s (%v), want %s", id, sec, err, want)
		}
	}
}
