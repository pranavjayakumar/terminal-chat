package identity

import (
	"bytes"
	"crypto/rand"
	"slices"
	"testing"

	"github.com/skailama/tc/internal/e2ecrypto"
)

// Fuzz targets for the signed E2E objects a server or member controls
// (docs/E2E.md step 12): everything the decoder accepts is canonical (it
// re-encodes to the same bytes) and satisfies the decoder's invariants, and
// nothing but the genuine object verifies.

func FuzzEpochBundle(f *testing.F) {
	a, b := newDevice(f), newDevice(f)
	k, err := e2ecrypto.NewEpochKey(rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	p := EpochParams{ServerID: testSID, Chan: 7, Epoch: 1, Roster: []uint64{1, 3}, Time: testNow.UnixMilli(),
		K: k, Recipients: recipientsOf(a, b)}
	e1, err := NewEpochBundle(rand.Reader, a, p)
	if err != nil {
		f.Fatal(err)
	}
	p.Epoch, p.Prev, p.PrevK = 2, e2ecrypto.ChainHash(e1.B), &k
	e2, err := NewEpochBundle(rand.Reader, a, p)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(e1.B, e1.Sig)
	f.Add(e2.B, e2.Sig)
	f.Add([]byte{0x80}, []byte{})
	f.Add(bytes.Repeat([]byte{0xdc, 0xff, 0xff}, 4), e1.Sig)
	f.Fuzz(func(t *testing.T, raw, sig []byte) {
		if eb, err := decodeEpochBundle(raw); err == nil {
			w := epochBundleB{V: 1, ServerID: eb.ServerID[:], Chan: eb.Chan, Epoch: eb.Epoch, Prev: eb.Prev[:],
				Roster: eb.Roster, By: eb.By[:], Time: eb.Time, KC: eb.KC[:], Eph: eb.Eph[:]}
			for _, bx := range eb.Boxes {
				w.Boxes = append(w.Boxes, epochBoxB{Dev: bx.Dev[:], CT: bx.CT[:]})
			}
			if eb.Back != nil {
				w.Back = eb.Back[:]
			}
			if re := mustMarshal(t, w); !bytes.Equal(re, raw) {
				t.Fatalf("accepted non-canonical b %x (canonical %x)", raw, re)
			}
			if eb.Epoch == 0 || len(eb.Roster) == 0 || len(eb.Boxes) == 0 || len(eb.Boxes) > MaxBundleBoxes ||
				!slices.IsSorted(eb.Roster) || (eb.Epoch == 1) != (eb.Back == nil) {
				t.Fatalf("accepted an invalid bundle: %+v", eb)
			}
			devs := map[e2ecrypto.DevID]bool{}
			for _, bx := range eb.Boxes {
				if devs[bx.Dev] {
					t.Fatal("accepted two boxes to one device")
				}
				devs[bx.Dev] = true
			}
			// Must not panic on anything decoded.
			_ = eb.CheckBoxKeys(keysOf(a, b))
			_ = eb.BoxDevs()
		}
		if _, err := PeekEpochBundle(Signed{B: raw, Sig: sig}); err == nil && len(raw)+len(sig) > MaxBundleSize {
			t.Fatal("peeked an oversized bundle")
		}
		if _, err := (Signed{B: raw, Sig: sig}).VerifyEpochBundle(testSID, a.Ed25519Pub); err == nil &&
			!(bytes.Equal(raw, e1.B) || bytes.Equal(raw, e2.B)) {
			t.Fatalf("forged bundle verified: %x", raw)
		}
	})
}

func FuzzKeyEndorse(f *testing.F) {
	s, n := signerFromKey(f, ed25519Key(f)), signerFromKey(f, ed25519Key(f))
	pub := s.PublicKey().Marshal()
	se, err := NewKeyEndorse(s, testSID, 4, n.PublicKey(), testNow)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(se.B, se.Sig)
	f.Add([]byte{0x80}, []byte{})
	f.Fuzz(func(t *testing.T, raw, sig []byte) {
		if e, err := decodeKeyEndorse(raw); err == nil {
			re := mustMarshal(t, keyEndorseB{V: 1, ServerID: e.ServerID[:], User: e.User, NewSSH: e.NewSSH, Time: e.Time})
			if !bytes.Equal(re, raw) {
				t.Fatalf("accepted non-canonical b %x (canonical %x)", raw, re)
			}
		}
		_, _ = PeekKeyEndorse(Signed{B: raw, Sig: sig})
		if _, err := (Signed{B: raw, Sig: sig}).VerifyKeyEndorse(testSID, pub); err == nil && !bytes.Equal(raw, se.B) {
			t.Fatalf("forged endorsement verified: %x", raw)
		}
	})
}

func FuzzDeviceRevoke(f *testing.F) {
	s := signerFromKey(f, ed25519Key(f))
	pub := s.PublicKey().Marshal()
	by := newDevice(f)
	victim := newDevice(f)
	rs, err := NewDeviceRevoke(s, testSID, 4, victim.ID, testNow)
	if err != nil {
		f.Fatal(err)
	}
	rd, err := NewDeviceRevokeByDevice(by, testSID, 4, victim.ID, testNow)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(rs.B, rs.Sig)
	f.Add(rd.B, rd.Sig)
	f.Add([]byte{0x80}, []byte{})
	f.Fuzz(func(t *testing.T, raw, sig []byte) {
		if r, err := decodeRevokeB(raw); err == nil {
			re := mustMarshal(t, deviceRevokeB{V: 1, ServerID: r.ServerID[:], User: r.User, Dev: r.Dev[:], Time: r.Time})
			if !bytes.Equal(re, raw) {
				t.Fatalf("accepted non-canonical b %x (canonical %x)", raw, re)
			}
		}
		_, _ = PeekDeviceRevoke(Signed{B: raw, Sig: sig})
		in := Signed{B: raw, Sig: sig}
		if _, err := in.VerifyDeviceRevoke(testSID, pub); err == nil && !bytes.Equal(raw, rs.B) {
			t.Fatalf("forged revoke verified: %x", raw)
		}
		if _, err := in.VerifyDeviceRevokeByDevice(testSID, by.Ed25519Pub); err == nil && !bytes.Equal(raw, rd.B) {
			t.Fatalf("forged device revoke verified: %x", raw)
		}
	})
}
