package identity

import (
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

var (
	testSID = e2ecrypto.ServerID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	testNow = time.UnixMilli(1_760_000_000_123)
)

func newDevice(t testing.TB) *e2ecrypto.DeviceKeys {
	t.Helper()
	d, err := e2ecrypto.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDeviceCertAllKeyTypes(t *testing.T) {
	for _, ns := range allSigners(t) {
		t.Run(ns.name, func(t *testing.T) {
			dev := newDevice(t)
			pub := ns.s.PublicKey().Marshal()
			sc, err := NewDeviceCert(ns.s, testSID, 42, dev, testNow)
			if err != nil {
				t.Fatal(err)
			}
			c, err := sc.VerifyDeviceCert(testSID, pub)
			if err != nil {
				t.Fatalf("valid cert rejected: %v", err)
			}
			if c.ServerID != testSID || c.User != 42 || c.Dev != dev.ID || c.X25519 != dev.X25519Pub ||
				!c.Ed25519.Equal(dev.Ed25519Pub) || string(c.SSH) != string(pub) || c.Created != testNow.UnixMilli() {
				t.Fatalf("decoded cert mismatch: %+v", c)
			}
			if _, err := sc.VerifyDeviceCert(testSID, newSigner(t).PublicKey().Marshal()); !errors.Is(err, e2ecrypto.ErrBadSignature) {
				t.Fatalf("other key: err = %v", err)
			}
			if _, err := sc.VerifyDeviceCert(e2ecrypto.ServerID{9}, pub); !errors.Is(err, ErrWrongServer) {
				t.Fatalf("other server: err = %v", err)
			}
		})
	}
}

func TestDeviceCertSKWithoutUPRejected(t *testing.T) {
	for _, typ := range []string{ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256} {
		s := newSKSigner(t, typ, 0)
		sc, err := NewDeviceCert(s, testSID, 1, newDevice(t), testNow)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sc.VerifyDeviceCert(testSID, s.PublicKey().Marshal()); !errors.Is(err, e2ecrypto.ErrBadSignature) {
			t.Fatalf("%s without UP: err = %v", typ, err)
		}
	}
}

func TestDeviceCertTamperedB(t *testing.T) {
	s := newSigner(t)
	pub := s.PublicKey().Marshal()
	sc, err := NewDeviceCert(s, testSID, 1, newDevice(t), testNow)
	if err != nil {
		t.Fatal(err)
	}
	for i := range sc.B {
		for _, bit := range []byte{0x01, 0x80} {
			b := append([]byte(nil), sc.B...)
			b[i] ^= bit
			if _, err := (Signed{B: b, Sig: sc.Sig}).VerifyDeviceCert(testSID, pub); err == nil {
				t.Fatalf("flip at byte %d accepted", i)
			}
		}
	}
	if _, err := (Signed{B: sc.B[:len(sc.B)-1], Sig: sc.Sig}).VerifyDeviceCert(testSID, pub); err == nil {
		t.Fatal("truncated b accepted")
	}
	if _, err := (Signed{B: append(append([]byte(nil), sc.B...), 0xc0), Sig: sc.Sig}).VerifyDeviceCert(testSID, pub); err == nil {
		t.Fatal("extended b accepted")
	}
	sig := append([]byte(nil), sc.Sig...)
	sig[len(sig)-1] ^= 1
	if _, err := (Signed{B: sc.B, Sig: sig}).VerifyDeviceCert(testSID, pub); err == nil {
		t.Fatal("tampered sig accepted")
	}
}

// signB signs an arbitrary b under ctx, to build objects NewDeviceCert won't.
func signB(t *testing.T, s ssh.Signer, ctx string, v any) Signed {
	t.Helper()
	var b []byte
	switch v := v.(type) {
	case []byte:
		b = v
	default:
		var err error
		if b, err = proto.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	sig, err := SignContext(s, ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	return Signed{B: b, Sig: sig}
}

func validCertB(s ssh.Signer, dev *e2ecrypto.DeviceKeys) deviceCertB {
	return deviceCertB{
		V: 1, ServerID: testSID[:], User: 1, Dev: dev.ID[:], X25519: dev.X25519Pub[:],
		Ed25519: dev.Ed25519Pub, SSH: s.PublicKey().Marshal(), Created: 5,
	}
}

func TestDeviceCertEmbeddedKeyMustMatch(t *testing.T) {
	a, b := newSigner(t), newSigner(t)
	w := validCertB(a, newDevice(t))
	w.SSH = b.PublicKey().Marshal() // a signs a cert claiming to be b's
	sc := signB(t, a, CtxDevice, w)
	if _, err := sc.VerifyDeviceCert(testSID, a.PublicKey().Marshal()); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("verify with signer: err = %v, want ErrWrongKey", err)
	}
	if _, err := sc.VerifyDeviceCert(testSID, b.PublicKey().Marshal()); !errors.Is(err, e2ecrypto.ErrBadSignature) {
		t.Fatalf("verify with embedded key: err = %v", err)
	}
}

func TestDeviceCertVersionAndShape(t *testing.T) {
	s := newSigner(t)
	pub := s.PublicKey().Marshal()
	dev := newDevice(t)

	w := validCertB(s, dev)
	w.V = 2
	if _, err := signB(t, s, CtxDevice, w).VerifyDeviceCert(testSID, pub); !errors.Is(err, e2ecrypto.ErrVersion) {
		t.Fatalf("v=2: err = %v", err)
	}

	bad := func(name string, mod func(*deviceCertB)) {
		w := validCertB(s, dev)
		mod(&w)
		if _, err := signB(t, s, CtxDevice, w).VerifyDeviceCert(testSID, pub); !errors.Is(err, e2ecrypto.ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
	bad("short server_id", func(w *deviceCertB) { w.ServerID = w.ServerID[:15] })
	bad("long dev", func(w *deviceCertB) { w.Dev = append(w.Dev, 0) })
	bad("short x25519", func(w *deviceCertB) { w.X25519 = w.X25519[:31] })
	bad("nil ed25519", func(w *deviceCertB) { w.Ed25519 = nil })

	// Non-canonical encodings of a valid cert: same meaning to a lax decoder,
	// so they must not verify.
	type withExtra struct {
		deviceCertB `msgpack:",inline"`
		Extra       string `msgpack:"extra"`
	}
	nonCanon := map[string]any{
		"unknown key": withExtra{validCertB(s, dev), "x"},
		"wide ints":   wideInts(t, validCertB(s, dev)),
		"trailing":    append(mustMarshal(t, validCertB(s, dev)), 0xc0),
	}
	for name, v := range nonCanon {
		if _, err := signB(t, s, CtxDevice, v).VerifyDeviceCert(testSID, pub); err == nil {
			t.Errorf("%s: non-canonical b accepted", name)
		}
	}
	if _, err := signB(t, s, CtxDevice, []byte{0x80}).VerifyDeviceCert(testSID, pub); err == nil {
		t.Error("empty map accepted")
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := proto.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// wideInts encodes w without compact ints (v as a full-width integer).
func wideInts(t *testing.T, w deviceCertB) []byte {
	t.Helper()
	b, err := msgpack.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == string(mustMarshal(t, w)) {
		t.Fatal("wide encoding is identical to canonical; test proves nothing")
	}
	return b
}

func TestOversizedRejected(t *testing.T) {
	s := newSigner(t)
	pub := s.PublicKey().Marshal()
	sc, err := NewDeviceCert(s, testSID, 1, newDevice(t), testNow)
	if err != nil {
		t.Fatal(err)
	}
	pad := make([]byte, MaxSignedSize-len(sc.B)+1-len(sc.Sig))
	// A valid signature over an oversized b is still refused, before verifying.
	big := signB(t, s, CtxDevice, append(append([]byte(nil), sc.B...), pad...))
	for name, fn := range map[string]func() error{
		"cert":    func() error { _, err := big.VerifyDeviceCert(testSID, pub); return err },
		"endorse": func() error { _, err := big.VerifyKeyEndorse(testSID, pub); return err },
		"revoke":  func() error { _, err := big.VerifyDeviceRevoke(testSID, pub); return err },
		"revoke-dev": func() error {
			_, err := big.VerifyDeviceRevokeByDevice(testSID, newDevice(t).Ed25519Pub)
			return err
		},
	} {
		if err := fn(); !errors.Is(err, e2ecrypto.ErrTooLarge) {
			t.Errorf("%s: err = %v, want ErrTooLarge", name, err)
		}
	}
}

func TestKeyEndorse(t *testing.T) {
	for _, ns := range allSigners(t) {
		t.Run(ns.name, func(t *testing.T) {
			newKey := newSigner(t).PublicKey()
			se, err := NewKeyEndorse(ns.s, testSID, 7, newKey, testNow)
			if err != nil {
				t.Fatal(err)
			}
			e, err := se.VerifyKeyEndorse(testSID, ns.s.PublicKey().Marshal())
			if err != nil {
				t.Fatalf("valid endorse rejected: %v", err)
			}
			if e.ServerID != testSID || e.User != 7 || string(e.NewSSH) != string(newKey.Marshal()) || e.Time != testNow.UnixMilli() {
				t.Fatalf("decoded endorse mismatch: %+v", e)
			}
			if _, err := se.VerifyKeyEndorse(testSID, newKey.Marshal()); err == nil {
				t.Fatal("endorse verified by the endorsed (not endorsing) key")
			}
			if _, err := se.VerifyKeyEndorse(e2ecrypto.ServerID{}, ns.s.PublicKey().Marshal()); !errors.Is(err, ErrWrongServer) {
				t.Fatalf("other server: err = %v", err)
			}
		})
	}

	s := newSigner(t)
	w := keyEndorseB{V: 1, ServerID: testSID[:], User: 1, NewSSH: []byte("not a key"), Time: 1}
	if _, err := signB(t, s, CtxEndorse, w).VerifyKeyEndorse(testSID, s.PublicKey().Marshal()); !errors.Is(err, e2ecrypto.ErrMalformed) {
		t.Fatalf("garbage new_ssh: err = %v", err)
	}
}

func TestDeviceRevoke(t *testing.T) {
	s := newSigner(t)
	pub := s.PublicKey().Marshal()
	dev := newDevice(t)
	other := newDevice(t)

	sr, err := NewDeviceRevoke(s, testSID, 3, dev.ID, testNow)
	if err != nil {
		t.Fatal(err)
	}
	r, err := sr.VerifyDeviceRevoke(testSID, pub)
	if err != nil {
		t.Fatalf("valid revoke rejected: %v", err)
	}
	if r.ServerID != testSID || r.User != 3 || r.Dev != dev.ID || r.Time != testNow.UnixMilli() {
		t.Fatalf("decoded revoke mismatch: %+v", r)
	}
	if _, err := sr.VerifyDeviceRevoke(e2ecrypto.ServerID{}, pub); !errors.Is(err, ErrWrongServer) {
		t.Fatalf("other server: err = %v", err)
	}

	sd, err := NewDeviceRevokeByDevice(other, testSID, 3, dev.ID, testNow)
	if err != nil {
		t.Fatal(err)
	}
	r, err = sd.VerifyDeviceRevokeByDevice(testSID, other.Ed25519Pub)
	if err != nil {
		t.Fatalf("valid device revoke rejected: %v", err)
	}
	if r.Dev != dev.ID || r.User != 3 {
		t.Fatalf("decoded device revoke mismatch: %+v", r)
	}
	if _, err := sd.VerifyDeviceRevokeByDevice(testSID, dev.Ed25519Pub); !errors.Is(err, e2ecrypto.ErrBadSignature) {
		t.Fatalf("wrong device key: err = %v", err)
	}
	b := append([]byte(nil), sd.B...)
	b[len(b)-1] ^= 1
	if _, err := (Signed{B: b, Sig: sd.Sig}).VerifyDeviceRevokeByDevice(testSID, other.Ed25519Pub); err == nil {
		t.Fatal("tampered device revoke accepted")
	}
}

// Every signed object must verify only as its own kind, even when the same
// key could have produced the others.
func TestCrossContextReplay(t *testing.T) {
	s := newSigner(t)
	pub := s.PublicKey().Marshal()
	dev := newDevice(t)

	cert, _ := NewDeviceCert(s, testSID, 1, dev, testNow)
	endorse, _ := NewKeyEndorse(s, testSID, 1, newSigner(t).PublicKey(), testNow)
	revoke, _ := NewDeviceRevoke(s, testSID, 1, dev.ID, testNow)
	revokeDev, _ := NewDeviceRevokeByDevice(dev, testSID, 1, dev.ID, testNow)

	// The same b bytes re-signed under each other context, so only the
	// context differs (b shapes differ between kinds otherwise).
	resigned := func(b []byte, ctx string) Signed { return signB(t, s, ctx, b) }

	asCert := func(x Signed) error { _, err := x.VerifyDeviceCert(testSID, pub); return err }
	asEndorse := func(x Signed) error { _, err := x.VerifyKeyEndorse(testSID, pub); return err }
	asRevoke := func(x Signed) error { _, err := x.VerifyDeviceRevoke(testSID, pub); return err }
	asRevokeDev := func(x Signed) error { _, err := x.VerifyDeviceRevokeByDevice(testSID, dev.Ed25519Pub); return err }

	authSig := func(b []byte) Signed {
		sig, err := Sign(s, b)
		if err != nil {
			t.Fatal(err)
		}
		return Signed{B: b, Sig: sig}
	}

	cases := []struct {
		name string
		obj  Signed
		as   func(Signed) error
	}{
		{"endorse as cert", endorse, asCert},
		{"revoke as cert", revoke, asCert},
		{"cert b with endorse ctx as cert", resigned(cert.B, CtxEndorse), asCert},
		{"cert b with revoke ctx as cert", resigned(cert.B, CtxRevoke), asCert},
		{"auth sig over cert b as cert", authSig(cert.B), asCert},
		{"cert as endorse", cert, asEndorse},
		{"endorse b with device ctx as endorse", resigned(endorse.B, CtxDevice), asEndorse},
		{"auth sig over endorse b as endorse", authSig(endorse.B), asEndorse},
		{"cert as revoke", cert, asRevoke},
		{"revoke b with endorse ctx as revoke", resigned(revoke.B, CtxEndorse), asRevoke},
		{"auth sig over revoke b as revoke", authSig(revoke.B), asRevoke},
		{"ssh revoke as device revoke", revoke, asRevokeDev},
		{"device revoke as ssh revoke", revokeDev, asRevoke},
	}
	for _, c := range cases {
		if err := c.as(c.obj); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}

	// A device-cert signature must not pass the auth handshake either.
	if _, err := Verify(pub, cert.Sig, cert.B); err == nil {
		t.Error("device cert signature accepted as auth")
	}
	// Ed25519 device-revoke signature must not verify under the epoch context.
	if e2ecrypto.VerifyEd25519(dev.Ed25519Pub, e2ecrypto.CtxEpoch, revokeDev.B, revokeDev.Sig) {
		t.Error("device revoke signature verified under epoch context")
	}
}

func TestSignedMsgpackRoundTrip(t *testing.T) {
	sc, err := NewDeviceCert(newSigner(t), testSID, 1, newDevice(t), testNow)
	if err != nil {
		t.Fatal(err)
	}
	var got Signed
	if err := proto.Unmarshal(mustMarshal(t, sc), &got); err != nil {
		t.Fatal(err)
	}
	if string(got.B) != string(sc.B) || string(got.Sig) != string(sc.Sig) {
		t.Fatal("Signed did not round-trip")
	}
}

func FuzzDeviceCert(f *testing.F) {
	s := signerFromKey(f, ed25519Key(f))
	pub := s.PublicKey().Marshal()
	sc, err := NewDeviceCert(s, testSID, 1, newDevice(f), testNow)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(sc.B, sc.Sig)
	f.Add([]byte{0x80}, []byte{})
	f.Add([]byte{0xdf, 0xff, 0xff, 0xff, 0xff}, sc.Sig)
	f.Fuzz(func(t *testing.T, b, sig []byte) {
		if c, err := decodeDeviceCert(b); err == nil {
			// Anything accepted must be canonical: re-encoding gives b back.
			re := mustMarshal(t, deviceCertB{
				V: 1, ServerID: c.ServerID[:], User: c.User, Dev: c.Dev[:], X25519: c.X25519[:],
				Ed25519: c.Ed25519, SSH: c.SSH, Created: c.Created,
			})
			if string(re) != string(b) {
				t.Fatalf("accepted non-canonical b %x (canonical %x)", b, re)
			}
		}
		c, err := (Signed{B: b, Sig: sig}).VerifyDeviceCert(testSID, pub)
		if err == nil && (string(b) != string(sc.B) || c.User != 1) {
			t.Fatalf("forged cert accepted: %x", b)
		}
	})
}

func TestPeekDecodesWithoutVerifying(t *testing.T) {
	s, other := newSigner(t), newSigner(t)
	dev := newDevice(t)
	sc, err := NewDeviceCert(s, testSID, 7, dev, testNow)
	if err != nil {
		t.Fatal(err)
	}
	sc.Sig = append([]byte(nil), sc.Sig...)
	sc.Sig[len(sc.Sig)-1] ^= 1 // a broken signature doesn't matter to Peek
	c, err := PeekDeviceCert(sc)
	if err != nil || c.User != 7 || c.Dev != dev.ID || c.ServerID != testSID {
		t.Fatalf("peek cert: %+v %v", c, err)
	}
	if _, err := sc.VerifyDeviceCert(testSID, s.PublicKey().Marshal()); err == nil {
		t.Fatal("tampered cert verified")
	}

	se, err := NewKeyEndorse(s, testSID, 7, other.PublicKey(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	e, err := PeekKeyEndorse(se)
	if err != nil || e.User != 7 || string(e.NewSSH) != string(other.PublicKey().Marshal()) {
		t.Fatalf("peek endorse: %+v %v", e, err)
	}

	sr, err := NewDeviceRevokeByDevice(newDevice(t), e2ecrypto.ServerID{9}, 7, dev.ID, testNow)
	if err != nil {
		t.Fatal(err)
	}
	r, err := PeekDeviceRevoke(sr)
	if err != nil || r.User != 7 || r.Dev != dev.ID || r.ServerID != (e2ecrypto.ServerID{9}) {
		t.Fatalf("peek revoke: %+v %v", r, err)
	}

	if _, err := PeekDeviceCert(Signed{B: []byte{0x80}}); !errors.Is(err, e2ecrypto.ErrMalformed) && !errors.Is(err, e2ecrypto.ErrVersion) {
		t.Fatalf("peek garbage: %v", err)
	}
	if _, err := PeekDeviceRevoke(Signed{B: make([]byte, MaxSignedSize+1)}); !errors.Is(err, e2ecrypto.ErrTooLarge) {
		t.Fatalf("peek oversize: %v", err)
	}
}

func TestDeviceCertZeroX25519Rejected(t *testing.T) {
	signers := allSigners(t)
	dev := newDevice(t)
	dev.X25519Pub = [32]byte{}
	sc, err := NewDeviceCert(signers[0].s, testSID, 42, dev, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sc.VerifyDeviceCert(testSID, signers[0].s.PublicKey().Marshal()); err == nil {
		t.Fatal("cert with an all-zero X25519 key accepted")
	}
	if _, err := PeekDeviceCert(sc); err == nil {
		t.Fatal("peek accepted an all-zero X25519 key")
	}
}

// Every low-order point, canonical or not, is rejected: nobody could seal
// an epoch key to such a device, so it would block every rotation.
func TestDeviceCertLowOrderX25519Rejected(t *testing.T) {
	signers := allSigners(t)
	p := [32]byte{0xed, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f} // 2^255-19
	order8 := [32]byte{0xe0, 0xeb, 0x7a, 0x7c, 0x3b, 0x41, 0xb8, 0xae, 0x16, 0x56, 0xe3, 0xfa, 0xf1, 0x9f, 0xc4, 0x6a,
		0xda, 0x09, 0x8d, 0xeb, 0x9c, 0x32, 0xb1, 0xfd, 0x86, 0x62, 0x05, 0x16, 0x5f, 0x49, 0xb8, 0x00}
	pPlus1 := p
	pPlus1[0]++
	for name, k := range map[string][32]byte{
		"zero": {}, "one": {1}, "order 8": order8, "p (≡ 0)": p, "p+1 (≡ 1)": pPlus1,
	} {
		dev := newDevice(t)
		dev.X25519Pub = k
		sc, err := NewDeviceCert(signers[0].s, testSID, 42, dev, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sc.VerifyDeviceCert(testSID, signers[0].s.PublicKey().Marshal()); err == nil {
			t.Errorf("%s: cert accepted", name)
		}
		if _, err := PeekDeviceCert(sc); err == nil {
			t.Errorf("%s: peek accepted", name)
		}
	}
	if !e2ecrypto.ValidX25519(newDevice(t).X25519Pub) {
		t.Fatal("a real key is rejected")
	}
}
