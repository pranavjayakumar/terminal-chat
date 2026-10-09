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
