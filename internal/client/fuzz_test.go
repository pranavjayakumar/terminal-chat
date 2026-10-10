package client

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"unicode/utf8"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/trust"
)

// Fuzz targets for the client's decoders of untrusted E2E input (docs/E2E.md
// step 12): Plain payloads a member seals under a valid key and signature,
// and key.share payloads anyone can send. Run with `make fuzz`.

// fuzzEnv is alice's offline client holding the key of epoch 1 of DM dm
// with bob, whose device b is verified; alice's own device keys are me.
type fuzzEnv struct {
	c   *Client
	sid e2ecrypto.ServerID
	k   e2ecrypto.Key
	eh  e2ecrypto.EpochHash
	b   *e2ecrypto.DeviceKeys
	me  *e2ecrypto.DeviceKeys
}

func newFuzzEnv(t testing.TB) *fuzzEnv {
	t.Helper()
	c, _ := localClient(t)
	sid := testSID()
	if err := setMeta(c.db, "server_id", sid[:]); err != nil {
		t.Fatal(err)
	}
	if err := storeChan(c.db, proto.Channel{ID: dm, Kind: proto.ChanDM, Members: []uint64{alice, bob}, E2E: true, Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	b, err := e2ecrypto.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d := trust.Device{ID: b.ID, User: bob, X25519: b.X25519Pub, Cert: []byte("cert")}
	copy(d.Ed25519[:], b.Ed25519Pub)
	if err := c.trust.PutDevice(sid, d); err != nil {
		t.Fatal(err)
	}
	me, err := c.trust.DeviceKeys(sid, alice)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := e2ecrypto.NewEpochKey(rand.Reader)
	h := sha256.Sum256([]byte("epoch 1"))
	if err := c.trust.AdvanceEpoch(sid, dm, trust.Epoch{Epoch: 1, Hash: h, KC: e2ecrypto.KeyCommitment(k, sid, dm, 1),
		Roster: []uint64{alice, bob}, Key: &k}); err != nil {
		t.Fatal(err)
	}
	return &fuzzEnv{c: c, sid: sid, k: k, eh: e2ecrypto.EpochHash(h[:e2ecrypto.IDSize]), b: b, me: me}
}

// seal encrypts raw (any bytes, as the Plain) as bob's device.
func (e *fuzzEnv) seal(t *testing.T, raw []byte) []byte {
	enc, err := e2ecrypto.Encrypt(rand.Reader, e.k, e.b.Ed25519, e.sid, dm, 1, e.eh, e.b.ID, raw)
	if err != nil {
		t.Skip() // too large to seal
	}
	return enc
}

// FuzzPlain: whatever Plain bytes a member seals, judging, editing,
// deleting and reacting never panic, and only a well-formed msg Plain with
// valid text is shown, exactly as sealed.
func FuzzPlain(f *testing.F) {
	for _, p := range []any{
		plainMsg{T: "msg", TS: 1, Body: "hi"},
		plainMsg{T: "msg", TS: 1, Body: "reply", ThreadRef: make([]byte, 16)},
		plainEdit{T: "edit", TS: 2, Body: "fixed", Ref: make([]byte, 16), V: 1},
		plainDel{T: "del", TS: 3, Ref: make([]byte, 16)},
		plainReact{T: "react", TS: 4, Ref: make([]byte, 16), Emoji: "👍", Add: true},
	} {
		f.Add([]byte(proto.MustRaw(p)), uint8(0))
	}
	f.Add([]byte{0xc1}, uint8(1))
	f.Add([]byte{0x84, 0xa1, 't', 0xa3, 'm', 's', 'g', 0xa2, 't', 's', 0x01, 0xa4, 'b', 'o', 'd', 'y', 0xa1, 'a', 0xa4, 'b', 'o', 'd', 'y', 0xa1, 'b'}, uint8(2))
	e := newFuzzEnv(f)
	var id uint64 = 1000
	f.Fuzz(func(t *testing.T, raw []byte, thread uint8) {
		// The Plain decoders on their own.
		var pm plainMsg
		var pe plainEdit
		var pd plainDel
		var pr plainReact
		_ = proto.Unmarshal(raw, &pm)
		_ = proto.Unmarshal(raw, &pe)
		_ = proto.Unmarshal(raw, &pd)
		_ = proto.Unmarshal(raw, &pr)
		_ = validText(pm.Body)

		enc := e.seal(t, raw)
		id++
		m := proto.Message{ID: id, Chan: dm, Author: bob, Enc: enc, Time: 1}
		if thread%3 == 1 {
			m.Thread = 1 // a root we don't have: waits
		}
		sec, err := e.c.saveMsg(e.c.db, &m, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if sec == SecE2E {
			var p plainMsg
			if proto.Unmarshal(raw, &p) != nil || p.T != "msg" || !validText(p.Body) || p.Body != m.Body || !utf8.ValidString(m.Body) {
				t.Fatalf("shown %q from Plain %x", m.Body, raw)
			}
		} else if m.Body != "" {
			t.Fatalf("unverified message shows %q", m.Body)
		}
		// The same bytes as an edit, a delete and a reaction of a verified
		// message of bob's.
		rootID := id
		root := proto.Message{ID: rootID, Chan: dm, Author: bob, Time: 1,
			Enc: e.seal(t, []byte(proto.MustRaw(plainMsg{T: "msg", TS: 1, Body: "root"})))}
		id++
		root.ID = id
		if sec, err := e.c.saveMsg(e.c.db, &root, true, nil); err != nil || sec != SecE2E {
			t.Fatalf("root: %v %v", sec, err)
		}
		row, ok := getMsgRow(e.c.db, root.ID)
		if !ok {
			t.Fatal("root not stored")
		}
		if e.c.applyEdit(e.c.db, row, enc, 0, nil) {
			after, _ := getMsgRow(e.c.db, root.ID)
			var p plainEdit
			if proto.Unmarshal(raw, &p) != nil || p.T != "edit" || !bytes.Equal(p.Ref, row.mid) || after.body != p.Body {
				t.Fatalf("edit applied from Plain %x", raw)
			}
		}
		_ = e.c.deleteVerdict(e.c.db, e.sid, true, dm, root.ID, bob, row.mid, enc, 0, nil)
		_, _ = e.c.judgeReact(e.c.db, row, bob, "~AAAAAAAAAAAAAAAA", enc, 0, nil)
	})
}

// FuzzKeyShare: key.share payloads (anyone can send one) are stored only
// when well-formed and for a member of an encrypted channel, and applying
// them never sets a key that doesn't match the epoch's kc.
func FuzzKeyShare(f *testing.F) {
	e := newFuzzEnv(f)
	// A real share for epoch 2 (which we hold no key for), and garbage.
	k2, _ := e2ecrypto.NewEpochKey(rand.Reader)
	h2 := sha256.Sum256([]byte("epoch 2"))
	if err := e.c.trust.AdvanceEpoch(e.sid, dm, trust.Epoch{Epoch: 2, Hash: h2, KC: e2ecrypto.KeyCommitment(k2, e.sid, dm, 2),
		Roster: []uint64{alice, bob}}); err != nil {
		f.Fatal(err)
	}
	eph, ct, err := e2ecrypto.SealShare(rand.Reader, e.me.X25519Pub, e2ecrypto.ShareAD{ServerID: e.sid, Chan: dm, Epoch: 2, Recipient: e.me.ID}, k2)
	if err != nil {
		f.Fatal(err)
	}
	good := proto.KeyShareArgs{Chan: dm, Epoch: 2, Dev: e.me.ID[:], Eph: eph[:], CT: ct[:]}
	f.Add([]byte(proto.MustRaw(good)), uint64(bob))
	f.Add([]byte(proto.MustRaw(proto.KeyShareArgs{Chan: dm, Epoch: 2, Dev: e.me.ID[:], Eph: make([]byte, 32), CT: make([]byte, e2ecrypto.BoxCTSize)})), uint64(bob))
	f.Add([]byte{0xc1}, uint64(0))
	var seq uint64
	f.Fuzz(func(t *testing.T, data []byte, sharer uint64) {
		var a proto.KeyShareArgs
		if proto.Unmarshal(data, &a) != nil {
			return
		}
		seq++
		if err := storeShare(e.c.db, alice, a.Chan, sharer, seq, a, &e.me.ID); err != nil {
			t.Fatal(err)
		}
		e.c.applyShares(e.sid, dm, e.me)
		for _, ep := range []uint32{1, 2} {
			got, ok, err := e.c.trust.Epoch(e.sid, dm, ep)
			if err != nil || !ok {
				t.Fatal(err)
			}
			if got.Key != nil && e2ecrypto.KeyCommitment(*got.Key, e.sid, dm, ep) != got.KC {
				t.Fatalf("epoch %d holds a key that doesn't match kc", ep)
			}
			if ep == 1 && *got.Key != e.k {
				t.Fatal("a share replaced a key we had")
			}
		}
		var n int
		e.c.db.QueryRow(`SELECT COUNT(*) FROM key_shares`).Scan(&n)
		if n > maxCachedShares {
			t.Fatalf("%d cached shares", n)
		}
	})
}
