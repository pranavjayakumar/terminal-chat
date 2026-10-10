package e2ecrypto

import (
	"bytes"
	"testing"
)

func fuzzSeeds(f *testing.F, fx *fixture) {
	f.Add([]byte{})
	f.Add([]byte{EncVersion})
	f.Add(make([]byte, MinEncSize))
	for _, p := range [][]byte{nil, []byte("hi"), bytes.Repeat([]byte("x"), 300)} {
		f.Add(fx.encrypt(f, p))
	}
}

func FuzzParseEnc(f *testing.F) {
	fuzzSeeds(f, newFixture(f))
	f.Fuzz(func(t *testing.T, enc []byte) {
		e, err := ParseEnc(enc)
		if err != nil {
			return
		}
		if len(enc) < MinEncSize || len(enc) > MaxEncSize || enc[0] != EncVersion ||
			len(e.CT) != len(enc)-EncHeaderSize-SigSize || (len(e.CT)-TagSize)%PadBlock != 0 {
			t.Fatalf("accepted bad framing: len %d ct %d", len(enc), len(e.CT))
		}
	})
}

func FuzzDecrypt(f *testing.F) {
	fx := newFixture(f)
	fuzzSeeds(f, fx)
	f.Fuzz(func(t *testing.T, enc []byte) {
		if p, err := Decrypt(fx.k, fx.alice.Ed25519Pub, fx.sid, fx.ch, enc); err == nil {
			if _, perr := ParseEnc(enc); perr != nil || len(p) > MaxPlainSize {
				t.Fatalf("decrypted unparseable/oversized enc")
			}
		}
		// Re-signed as a member holding a device key, the input reaches the AEAD
		// and unpad layers, which must also fail cleanly on garbage.
		if _, err := ParseEnc(enc); err == nil {
			re := resign(t, enc, fx.alice.Ed25519, fx.sid, fx.ch)
			_, _ = Decrypt(fx.k, fx.alice.Ed25519Pub, fx.sid, fx.ch, re)
		}
	})
}

// FuzzUnpad: unpad accepts exactly pad256's output, so a member can't make
// two clients read one plaintext differently.
func FuzzUnpad(f *testing.F) {
	f.Add([]byte{})
	f.Add(pad256(nil))
	f.Add(pad256([]byte("hi")))
	f.Add(append(pad256(bytes.Repeat([]byte{0}, 255)), 0))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := unpad(b)
		if err != nil {
			return
		}
		if !bytes.Equal(pad256(p), b) {
			t.Fatalf("unpad accepted non-canonical padding: %x", b)
		}
	})
}

// FuzzOpenBoxes: epoch boxes, key.share boxes and backlinks with
// attacker-chosen ephemeral keys and ciphertexts fail cleanly, and only the
// genuine ones open.
func FuzzOpenBoxes(f *testing.F) {
	fx := newFixture(f)
	r := newDetRand("boxes")
	eph, err := NewEphemeral(r)
	if err != nil {
		f.Fatal(err)
	}
	bad := BoxAD{ServerID: fx.sid, Chan: fx.ch, Epoch: fx.ep, Recipient: fx.bob.ID, By: fx.alice.ID}
	box, err := SealKey(eph, fx.bob.X25519Pub, bad, fx.k)
	if err != nil {
		f.Fatal(err)
	}
	sad := ShareAD{ServerID: fx.sid, Chan: fx.ch, Epoch: fx.ep, Recipient: fx.bob.ID}
	seph, share, err := SealShare(r, fx.bob.X25519Pub, sad, fx.k)
	if err != nil {
		f.Fatal(err)
	}
	prev, _ := NewEpochKey(r)
	back, err := SealBack(r, fx.k, prev, fx.sid, fx.ch, fx.ep)
	if err != nil {
		f.Fatal(err)
	}
	ephPub := eph.PublicKey().Bytes()
	f.Add(ephPub, box[:])
	f.Add(seph[:], share[:])
	f.Add(back[:24], back[24:])
	f.Add(make([]byte, 32), make([]byte, BoxCTSize))
	f.Fuzz(func(t *testing.T, e, ct []byte) {
		var e32 [32]byte
		copy(e32[:], e)
		if k, err := OpenKey(fx.bob.X25519, e32, bad, ct); err == nil &&
			(k != fx.k || !bytes.Equal(ct, box[:]) || !bytes.Equal(e32[:], ephPub)) {
			t.Fatalf("forged epoch box opened")
		}
		if k, err := OpenShare(fx.bob.X25519, e32, sad, ct); err == nil &&
			(k != fx.k || !bytes.Equal(ct, share[:]) || e32 != seph) {
			t.Fatalf("forged key.share opened")
		}
		bl := append(append([]byte(nil), e...), ct...)
		if k, err := OpenBack(fx.k, fx.sid, fx.ch, fx.ep, bl); err == nil && (k != prev || !bytes.Equal(bl, back[:])) {
			t.Fatalf("forged backlink opened")
		}
		_ = ValidX25519(e32)
	})
}
