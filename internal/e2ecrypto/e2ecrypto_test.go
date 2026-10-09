package e2ecrypto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// detRand is a deterministic reader: block i = sha256(seed || u64 BE i).
type detRand struct {
	seed []byte
	ctr  uint64
	buf  []byte
}

func newDetRand(seed string) *detRand { return &detRand{seed: []byte(seed)} }

func (r *detRand) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(r.buf) == 0 {
			h := sha256.New()
			h.Write(r.seed)
			h.Write(binary.BigEndian.AppendUint64(nil, r.ctr))
			r.ctr++
			r.buf = h.Sum(nil)
		}
		c := copy(p[n:], r.buf)
		r.buf = r.buf[c:]
		n += c
	}
	return n, nil
}

type fixture struct {
	alice, bob *DeviceKeys
	sid        ServerID
	ch         uint64
	ep         uint32
	eh         EpochHash
	k          Key
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	r := newDetRand("fixture")
	f := &fixture{ch: 42, ep: 3}
	var err error
	if f.alice, err = Generate(r); err != nil {
		t.Fatal(err)
	}
	if f.bob, err = Generate(r); err != nil {
		t.Fatal(err)
	}
	r.Read(f.sid[:])
	r.Read(f.eh[:])
	if f.k, err = NewEpochKey(r); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) encrypt(t testing.TB, plain []byte) []byte {
	t.Helper()
	enc, err := Encrypt(rand.Reader, f.k, f.alice.Ed25519, f.sid, f.ch, f.ep, f.eh, f.alice.ID, plain)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func (f *fixture) boxAD() BoxAD {
	return BoxAD{ServerID: f.sid, Chan: f.ch, Epoch: f.ep, Recipient: f.bob.ID, By: f.alice.ID}
}

// resign re-signs a (possibly modified) enc as a malicious member holding a valid
// device key would, so tests reach the AEAD layer instead of stopping at the signature.
func resign(t testing.TB, enc []byte, priv ed25519.PrivateKey, sid ServerID, ch uint64) []byte {
	t.Helper()
	out := bytes.Clone(enc)
	body := out[:len(out)-SigSize]
	sig, err := SignEd25519(priv, CtxMsgSig, sigBody(sid, ch, body))
	if err != nil {
		t.Fatal(err)
	}
	copy(out[len(out)-SigSize:], sig)
	return out
}

func TestDeviceKeysRoundTrip(t *testing.T) {
	k, err := Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, err := k.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != DeviceKeysSize || b[0] != 1 {
		t.Fatalf("marshal: len %d version %d", len(b), b[0])
	}
	var got DeviceKeys
	if err := got.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	if got.ID != k.ID || got.X25519Pub != k.X25519Pub || !got.X25519.Equal(k.X25519) ||
		!got.Ed25519.Equal(k.Ed25519) || !got.Ed25519Pub.Equal(k.Ed25519Pub) {
		t.Fatal("unmarshal mismatch")
	}
	if !bytes.Equal(got.X25519Pub[:], got.X25519.PublicKey().Bytes()) {
		t.Fatal("x25519 pub not derived from priv")
	}

	bad := bytes.Clone(b)
	bad[0] = 2
	if err := got.UnmarshalBinary(bad); !errors.Is(err, ErrVersion) {
		t.Fatalf("version 2: %v", err)
	}
	for _, n := range []int{0, 1, DeviceKeysSize - 1, DeviceKeysSize + 1} {
		in := make([]byte, n)
		if n > 0 {
			in[0] = 1
		}
		if err := got.UnmarshalBinary(in); !errors.Is(err, ErrMalformed) {
			t.Fatalf("len %d: %v", n, err)
		}
	}
	if _, err := (*DeviceKeys)(nil).MarshalBinary(); err == nil {
		t.Fatal("nil marshal succeeded")
	}
}

func TestGenerateShortRand(t *testing.T) {
	if _, err := Generate(bytes.NewReader(make([]byte, 10))); err == nil {
		t.Fatal("Generate with short rand succeeded")
	}
	if _, err := NewEpochKey(bytes.NewReader(nil)); err == nil {
		t.Fatal("NewEpochKey with empty rand succeeded")
	}
	f := newFixture(t)
	if _, err := Encrypt(bytes.NewReader(nil), f.k, f.alice.Ed25519, f.sid, f.ch, f.ep, f.eh, f.alice.ID, nil); err == nil {
		t.Fatal("Encrypt with empty rand succeeded")
	}
	if _, err := SealBack(bytes.NewReader(nil), f.k, f.k, f.sid, f.ch, f.ep); err == nil {
		t.Fatal("SealBack with empty rand succeeded")
	}
}

func TestKeyCommitment(t *testing.T) {
	f := newFixture(t)
	kc := KeyCommitment(f.k, f.sid, f.ch, f.ep)
	if kc != KeyCommitment(f.k, f.sid, f.ch, f.ep) {
		t.Fatal("not deterministic")
	}
	otherK := f.k
	otherK[0] ^= 1
	otherSID := f.sid
	otherSID[15] ^= 1
	for name, got := range map[string][32]byte{
		"key":    KeyCommitment(otherK, f.sid, f.ch, f.ep),
		"server": KeyCommitment(f.k, otherSID, f.ch, f.ep),
		"chan":   KeyCommitment(f.k, f.sid, f.ch+1, f.ep),
		"epoch":  KeyCommitment(f.k, f.sid, f.ch, f.ep+1),
	} {
		if got == kc {
			t.Errorf("kc unchanged when %s differs", name)
		}
	}
}

func TestBoxRoundTrip(t *testing.T) {
	f := newFixture(t)
	eph, err := NewEphemeral(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ephPub := [32]byte(eph.PublicKey().Bytes())
	ad := f.boxAD()
	ct, err := SealKey(eph, f.bob.X25519Pub, ad, f.k)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenKey(f.bob.X25519, ephPub, ad, ct[:])
	if err != nil || got != f.k {
		t.Fatalf("open: %v", err)
	}

	// Same eph to a second recipient must give an unrelated box.
	adA := ad
	adA.Recipient = f.alice.ID
	ctA, err := SealKey(eph, f.alice.X25519Pub, adA, f.k)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ctA[:], ct[:]) {
		t.Fatal("boxes to different recipients are equal")
	}
	if got, err := OpenKey(f.alice.X25519, ephPub, adA, ctA[:]); err != nil || got != f.k {
		t.Fatalf("open second recipient: %v", err)
	}

	swaps := map[string]func(*BoxAD){
		"serverID":     func(a *BoxAD) { a.ServerID[0] ^= 1 },
		"chan":         func(a *BoxAD) { a.Chan++ },
		"epoch":        func(a *BoxAD) { a.Epoch++ },
		"recipientDev": func(a *BoxAD) { a.Recipient = f.alice.ID },
		"byDev":        func(a *BoxAD) { a.By = f.bob.ID },
	}
	for name, mut := range swaps {
		a := ad
		mut(&a)
		if _, err := OpenKey(f.bob.X25519, ephPub, a, ct[:]); !errors.Is(err, ErrDecrypt) {
			t.Errorf("AD %s swapped: %v", name, err)
		}
	}
	if _, err := OpenKey(f.alice.X25519, ephPub, ad, ct[:]); !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrong recipient key: %v", err)
	}
	if _, err := OpenKey(f.bob.X25519, f.alice.X25519Pub, ad, ct[:]); !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrong eph pub: %v", err)
	}
	for i := range len(ct) * 8 {
		c := ct
		c[i/8] ^= 1 << (i % 8)
		if _, err := OpenKey(f.bob.X25519, ephPub, ad, c[:]); err == nil {
			t.Fatalf("bit %d flip opened", i)
		}
	}
	for n := range BoxCTSize + 2 {
		if n == BoxCTSize {
			continue
		}
		in := make([]byte, n)
		copy(in, ct[:])
		if _, err := OpenKey(f.bob.X25519, ephPub, ad, in); !errors.Is(err, ErrMalformed) {
			t.Fatalf("len %d: %v", n, err)
		}
	}
	if _, err := SealKey(nil, f.bob.X25519Pub, ad, f.k); err == nil {
		t.Fatal("nil eph sealed")
	}
	if _, err := OpenKey(nil, ephPub, ad, ct[:]); err == nil {
		t.Fatal("nil recipient opened")
	}
}

func TestBoxRejectsLowOrder(t *testing.T) {
	f := newFixture(t)
	eph, _ := NewEphemeral(rand.Reader)
	// Small-order X25519 points (RFC 7748 / libsodium blocklist), all give ss = 0.
	low := [][32]byte{
		{},  // 0
		{1}, // 1
		{0xe0, 0xeb, 0x7a, 0x7c, 0x3b, 0x41, 0xb8, 0xae, 0x16, 0x56, 0xe3, 0xfa, 0xf1, 0x9f, 0xc4, 0x6a,
			0xda, 0x09, 0x8d, 0xeb, 0x9c, 0x32, 0xb1, 0xfd, 0x86, 0x62, 0x05, 0x16, 0x5f, 0x49, 0xb8, 0x00},
		{0x5f, 0x9c, 0x95, 0xbc, 0xa3, 0x50, 0x8c, 0x24, 0xb1, 0xd0, 0xb1, 0x55, 0x9c, 0x83, 0xef, 0x5b,
			0x04, 0x44, 0x5c, 0xc4, 0x58, 0x1c, 0x8e, 0x86, 0xd8, 0x22, 0x4e, 0xdd, 0xd0, 0x9f, 0x11, 0x57},
	}
	for i, p := range low {
		if _, err := SealKey(eph, p, f.boxAD(), f.k); !errors.Is(err, ErrLowOrder) {
			t.Errorf("seal to low-order point %d: %v", i, err)
		}
		if _, err := OpenKey(f.bob.X25519, p, f.boxAD(), make([]byte, BoxCTSize)); !errors.Is(err, ErrLowOrder) {
			t.Errorf("open from low-order eph %d: %v", i, err)
		}
	}
}

func TestBackRoundTrip(t *testing.T) {
	f := newFixture(t)
	prev, _ := NewEpochKey(rand.Reader)
	back, err := SealBack(rand.Reader, f.k, prev, f.sid, f.ch, f.ep)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenBack(f.k, f.sid, f.ch, f.ep, back[:])
	if err != nil || got != prev {
		t.Fatalf("open: %v", err)
	}
	otherSID := f.sid
	otherSID[0] ^= 1
	wrongK := f.k
	wrongK[31] ^= 1
	cases := map[string]func() (Key, error){
		"serverID": func() (Key, error) { return OpenBack(f.k, otherSID, f.ch, f.ep, back[:]) },
		"chan":     func() (Key, error) { return OpenBack(f.k, f.sid, f.ch+1, f.ep, back[:]) },
		"epoch":    func() (Key, error) { return OpenBack(f.k, f.sid, f.ch, f.ep-1, back[:]) },
		"key":      func() (Key, error) { return OpenBack(wrongK, f.sid, f.ch, f.ep, back[:]) },
		"prev key": func() (Key, error) { return OpenBack(prev, f.sid, f.ch, f.ep, back[:]) },
	}
	for name, open := range cases {
		if _, err := open(); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s swapped: %v", name, err)
		}
	}
	for i := range len(back) * 8 {
		b := back
		b[i/8] ^= 1 << (i % 8)
		if _, err := OpenBack(f.k, f.sid, f.ch, f.ep, b[:]); err == nil {
			t.Fatalf("bit %d flip opened", i)
		}
	}
	for n := range BackSize {
		if _, err := OpenBack(f.k, f.sid, f.ch, f.ep, back[:n]); !errors.Is(err, ErrMalformed) {
			t.Fatalf("len %d: %v", n, err)
		}
	}
}

func TestEncryptRoundTrip(t *testing.T) {
	f := newFixture(t)
	for _, n := range []int{0, 1, 254, 255, 256, 257, 1000, MaxPlainSize} {
		plain := make([]byte, n)
		rand.Read(plain)
		enc := f.encrypt(t, plain)
		wantCT := (n+1+PadBlock-1)/PadBlock*PadBlock + TagSize
		if len(enc) != EncHeaderSize+wantCT+SigSize || len(enc) > MaxEncSize {
			t.Fatalf("n=%d: enc len %d", n, len(enc))
		}
		h, err := ParseEnc(enc)
		if err != nil {
			t.Fatalf("n=%d: parse: %v", n, err)
		}
		if h.Epoch != f.ep || h.EH != f.eh || h.Dev != f.alice.ID || len(h.CT) != wantCT ||
			!bytes.Equal(h.Nonce[:], enc[37:61]) || !bytes.Equal(h.Sig[:], enc[len(enc)-SigSize:]) {
			t.Fatalf("n=%d: header %+v", n, h)
		}
		got, err := Decrypt(f.k, f.alice.Ed25519Pub, f.sid, f.ch, enc)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("n=%d: decrypt: %v", n, err)
		}
	}
	if _, err := Encrypt(rand.Reader, f.k, f.alice.Ed25519, f.sid, f.ch, f.ep, f.eh, f.alice.ID,
		make([]byte, MaxPlainSize+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized plaintext: %v", err)
	}
	if _, err := Encrypt(rand.Reader, f.k, nil, f.sid, f.ch, f.ep, f.eh, f.alice.ID, nil); !errors.Is(err, ErrBadKey) {
		t.Fatalf("nil signer: %v", err)
	}
	if bytes.Equal(f.encrypt(t, []byte("x")), f.encrypt(t, []byte("x"))) {
		t.Fatal("two encryptions of the same plaintext are equal")
	}
}

func TestDecryptBitFlips(t *testing.T) {
	f := newFixture(t)
	plain := []byte("ship it")
	enc := f.encrypt(t, plain)
	for i := range len(enc) * 8 {
		e := bytes.Clone(enc)
		e[i/8] ^= 1 << (i % 8)
		if got, err := Decrypt(f.k, f.alice.Ed25519Pub, f.sid, f.ch, e); err == nil {
			t.Fatalf("bit %d (byte %d) flip decrypted to %q", i, i/8, got)
		}
	}
}

func TestDecryptADSwaps(t *testing.T) {
	f := newFixture(t)
	enc := f.encrypt(t, []byte("hello"))
	otherSID := f.sid
	otherSID[3] ^= 1

	// Caller-supplied scope: the signature already fails.
	if _, err := Decrypt(f.k, f.alice.Ed25519Pub, otherSID, f.ch, enc); !errors.Is(err, ErrBadSignature) {
		t.Errorf("serverID swap: %v", err)
	}
	if _, err := Decrypt(f.k, f.alice.Ed25519Pub, f.sid, f.ch+1, enc); !errors.Is(err, ErrBadSignature) {
		t.Errorf("chan swap: %v", err)
	}

	// A member re-signs with their own valid key: the AEAD must still refuse.
	re := resign(t, enc, f.alice.Ed25519, otherSID, f.ch)
	if _, err := Decrypt(f.k, f.alice.Ed25519Pub, otherSID, f.ch, re); !errors.Is(err, ErrDecrypt) {
		t.Errorf("serverID swap (re-signed): %v", err)
	}
	re = resign(t, enc, f.alice.Ed25519, f.sid, f.ch+1)
	if _, err := Decrypt(f.k, f.alice.Ed25519Pub, f.sid, f.ch+1, re); !errors.Is(err, ErrDecrypt) {
		t.Errorf("chan swap (re-signed): %v", err)
	}
	headerSwaps := map[string]func([]byte){
		"epoch": func(e []byte) { binary.BigEndian.PutUint32(e[1:5], f.ep+1) },
		"eh":    func(e []byte) { e[5] ^= 1 },
		"dev":   func(e []byte) { copy(e[21:37], f.bob.ID[:]) },
		"nonce": func(e []byte) { e[37] ^= 1 },
	}
	for name, mut := range headerSwaps {
		e := bytes.Clone(enc)
		mut(e)
		if _, err := Decrypt(f.k, f.alice.Ed25519Pub, f.sid, f.ch, e); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s swap: %v", name, err)
		}
		e = resign(t, e, f.alice.Ed25519, f.sid, f.ch)
		if _, err := Decrypt(f.k, f.alice.Ed25519Pub, f.sid, f.ch, e); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s swap (re-signed): %v", name, err)
		}
	}
}

func TestDecryptWrongKeys(t *testing.T) {
	f := newFixture(t)
	enc := f.encrypt(t, []byte("hello"))
	wrong := f.k
	wrong[0] ^= 1
	if _, err := Decrypt(wrong, f.alice.Ed25519Pub, f.sid, f.ch, enc); !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrong K: %v", err)
	}
	if _, err := Decrypt(f.k, f.bob.Ed25519Pub, f.sid, f.ch, enc); !errors.Is(err, ErrBadSignature) {
		t.Errorf("wrong verify key: %v", err)
	}
	if _, err := Decrypt(f.k, nil, f.sid, f.ch, enc); !errors.Is(err, ErrBadSignature) {
		t.Errorf("nil verify key: %v", err)
	}
	// Signature is checked before decryption: bad sig + wrong K reports the signature.
	e := bytes.Clone(enc)
	e[len(e)-1] ^= 1
	if _, err := Decrypt(wrong, f.alice.Ed25519Pub, f.sid, f.ch, e); !errors.Is(err, ErrBadSignature) {
		t.Errorf("bad sig + wrong K: %v", err)
	}
	// Bob holds K and signs as himself while claiming Alice's dev: verifying as Alice fails.
	forged := bytes.Clone(enc)
	forged = resign(t, forged, f.bob.Ed25519, f.sid, f.ch)
	if _, err := Decrypt(f.k, f.alice.Ed25519Pub, f.sid, f.ch, forged); !errors.Is(err, ErrBadSignature) {
		t.Errorf("forged by other member: %v", err)
	}
}

func TestTruncatedAndExtended(t *testing.T) {
	f := newFixture(t)
	enc := f.encrypt(t, bytes.Repeat([]byte("a"), 300))
	for n := range len(enc) {
		// ParseEnc checks framing only; a prefix that happens to end on a pad block is well-framed.
		wellFramed := n >= MinEncSize && (n-MinEncSize)%PadBlock == 0
		if _, err := ParseEnc(enc[:n]); (err == nil) != wellFramed {
			t.Fatalf("ParseEnc(%d-byte prefix): %v", n, err)
		}
		if _, err := Decrypt(f.k, f.alice.Ed25519Pub, f.sid, f.ch, enc[:n]); err == nil {
			t.Fatalf("Decrypt accepted %d-byte prefix", n)
		}
	}
	for _, extra := range []int{1, 16, PadBlock} {
		e := append(bytes.Clone(enc), make([]byte, extra)...)
		if _, err := Decrypt(f.k, f.alice.Ed25519Pub, f.sid, f.ch, e); err == nil {
			t.Fatalf("Decrypt accepted %d extra bytes", extra)
		}
	}
}

func TestParseEncRejects(t *testing.T) {
	ok := make([]byte, MinEncSize)
	ok[0] = EncVersion
	if _, err := ParseEnc(ok); err != nil {
		t.Fatalf("minimal well-framed enc: %v", err)
	}
	v2 := bytes.Clone(ok)
	v2[0] = 2
	if _, err := ParseEnc(v2); !errors.Is(err, ErrVersion) {
		t.Errorf("version 2: %v", err)
	}
	if _, err := ParseEnc(append(bytes.Clone(ok), 0)); !errors.Is(err, ErrMalformed) {
		t.Errorf("partial pad block: %v", err)
	}
	big := make([]byte, MaxEncSize+1)
	big[0] = EncVersion
	if _, err := ParseEnc(big); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized: %v", err)
	}
	if _, err := ParseEnc(nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("nil: %v", err)
	}
}

func TestPad(t *testing.T) {
	for _, n := range []int{0, 1, 255, 256, 511, 512, 4000} {
		plain := bytes.Repeat([]byte{0}, n) // zeros in plaintext must survive
		p := pad256(plain)
		if len(p)%PadBlock != 0 || len(p) < n+1 || len(p) > n+PadBlock {
			t.Fatalf("n=%d: padded len %d", n, len(p))
		}
		got, err := unpad(p)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("n=%d: unpad: %v", n, err)
		}
	}
	good := pad256([]byte("hi"))
	nonZeroAfter := bytes.Clone(good)
	nonZeroAfter[len(nonZeroAfter)-1] = 1
	wrongMarker := bytes.Clone(good)
	wrongMarker[2] = 0x81
	overlong := append(pad256([]byte("hi")), make([]byte, PadBlock)...) // marker not in last block
	rejects := map[string][]byte{
		"empty":            nil,
		"no 0x80":          make([]byte, PadBlock),
		"non-zero after":   nonZeroAfter,
		"wrong marker":     wrongMarker,
		"len 255":          good[:255],
		"len 257":          append(bytes.Clone(good), 0),
		"len 1":            {0x80},
		"overlong padding": overlong,
	}
	for name, in := range rejects {
		if _, err := unpad(in); !errors.Is(err, ErrBadPadding) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSignVerify(t *testing.T) {
	f := newFixture(t)
	b := []byte("bundle bytes")
	sig, err := SignEd25519(f.alice.Ed25519, CtxEpoch, b)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(f.alice.Ed25519Pub, append([]byte(CtxEpoch), b...), sig) {
		t.Fatal("signed message is not context || b")
	}
	if !VerifyEd25519(f.alice.Ed25519Pub, CtxEpoch, b, sig) {
		t.Fatal("verify failed")
	}
	if VerifyEd25519(f.alice.Ed25519Pub, CtxRevokeDev, b, sig) {
		t.Fatal("cross-context replay verified")
	}
	if VerifyEd25519(f.bob.Ed25519Pub, CtxEpoch, b, sig) {
		t.Fatal("wrong key verified")
	}
	if VerifyEd25519(f.alice.Ed25519Pub, CtxEpoch, []byte("bundle byteS"), sig) {
		t.Fatal("modified b verified")
	}
	if VerifyEd25519(f.alice.Ed25519Pub, CtxEpoch, b, sig[:63]) || VerifyEd25519(nil, CtxEpoch, b, sig) {
		t.Fatal("bad lengths verified")
	}
	// Context and b are concatenated, so contexts end in NUL to keep the split unambiguous.
	for _, c := range []string{CtxEpoch, CtxMsgSig, CtxRevokeDev} {
		if c[len(c)-1] != 0 {
			t.Errorf("context %q lacks NUL terminator", c)
		}
	}
	if _, err := SignEd25519(nil, CtxEpoch, b); !errors.Is(err, ErrBadKey) {
		t.Fatalf("nil priv: %v", err)
	}
}

func TestHashes(t *testing.T) {
	b := []byte("abc")
	full := sha256.Sum256(b)
	if MID(b) != MsgID(full[:16]) || EH(b) != EpochHash(full[:16]) || ChainHash(b) != full {
		t.Fatal("hash helpers disagree with sha256")
	}
}

func TestLimits(t *testing.T) {
	if EncHeaderSize != 61 || BoxCTSize != 48 || BackSize != 72 || MinEncSize != 397 {
		t.Fatal("layout constants changed")
	}
	if MaxPlainSize != 66303 || EncHeaderSize+MaxPlainSize+1+TagSize+SigSize > MaxEncSize {
		t.Fatalf("MaxPlainSize %d", MaxPlainSize)
	}
}

var _ io.Reader = (*detRand)(nil)

func TestShareRoundTripAndBinding(t *testing.T) {
	d, err := Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ad := ShareAD{ServerID: ServerID{1}, Chan: 9, Epoch: 3, Recipient: d.ID}
	k := Key{7, 7, 7}
	eph, ct, err := SealShare(rand.Reader, d.X25519Pub, ad, k)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenShare(d.X25519, eph, ad, ct[:])
	if err != nil || got != k {
		t.Fatalf("open: %v", err)
	}
	eph2, _, _ := SealShare(rand.Reader, d.X25519Pub, ad, k)
	if eph2 == eph {
		t.Fatal("ephemeral key reused")
	}
	for name, mod := range map[string]func(*ShareAD){
		"server": func(a *ShareAD) { a.ServerID[0] ^= 1 },
		"chan":   func(a *ShareAD) { a.Chan++ },
		"epoch":  func(a *ShareAD) { a.Epoch++ },
		"dev":    func(a *ShareAD) { a.Recipient[0] ^= 1 },
	} {
		bad := ad
		mod(&bad)
		if _, err := OpenShare(d.X25519, eph, bad, ct[:]); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s swapped: %v", name, err)
		}
	}
	// A share is not an epoch box: the domains differ.
	if _, err := OpenKey(d.X25519, eph, BoxAD{ServerID: ad.ServerID, Chan: 9, Epoch: 3, Recipient: d.ID}, ct[:]); err == nil {
		t.Fatal("share opened as an epoch box")
	}
	flipped := ct
	flipped[5] ^= 1
	if _, err := OpenShare(d.X25519, eph, ad, flipped[:]); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("bit flip: %v", err)
	}
	if _, err := OpenShare(d.X25519, eph, ad, ct[:10]); !errors.Is(err, ErrMalformed) {
		t.Fatalf("short: %v", err)
	}
}

func TestVerifyEnc(t *testing.T) {
	f := newFixture(t)
	enc := f.encrypt(t, []byte("hi"))
	if h, err := VerifyEnc(f.alice.Ed25519Pub, f.sid, f.ch, enc); err != nil || h.Dev != f.alice.ID {
		t.Fatalf("valid enc: %v", err)
	}
	other, _ := Generate(rand.Reader)
	if _, err := VerifyEnc(other.Ed25519Pub, f.sid, f.ch, enc); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong signer: %v", err)
	}
	if _, err := VerifyEnc(f.alice.Ed25519Pub, f.sid, f.ch+1, enc); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("other channel: %v", err)
	}
	e := bytes.Clone(enc)
	e[EncHeaderSize] ^= 1
	if _, err := VerifyEnc(f.alice.Ed25519Pub, f.sid, f.ch, e); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("flipped ct: %v", err)
	}
	if _, err := VerifyEnc(f.alice.Ed25519Pub, f.sid, f.ch, enc[:10]); !errors.Is(err, ErrMalformed) {
		t.Fatalf("short: %v", err)
	}
}
