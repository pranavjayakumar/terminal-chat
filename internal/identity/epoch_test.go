package identity

import (
	"crypto/rand"
	"errors"
	"slices"
	"testing"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

func recipientsOf(devs ...*e2ecrypto.DeviceKeys) []Recipient {
	var out []Recipient
	for _, d := range devs {
		out = append(out, Recipient{Dev: d.ID, X25519: d.X25519Pub})
	}
	return out
}

func keysOf(devs ...*e2ecrypto.DeviceKeys) map[e2ecrypto.DevID][32]byte {
	m := map[e2ecrypto.DevID][32]byte{}
	for _, d := range devs {
		m[d.ID] = d.X25519Pub
	}
	return m
}

func testParams(t *testing.T, devs ...*e2ecrypto.DeviceKeys) EpochParams {
	t.Helper()
	k, err := e2ecrypto.NewEpochKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return EpochParams{ServerID: testSID, Chan: 7, Epoch: 1, Roster: []uint64{3, 1}, Time: testNow.UnixMilli(),
		K: k, Recipients: recipientsOf(devs...)}
}

// signBundleB signs an arbitrary b, to test what the decoder accepts.
func signBundleB(t *testing.T, by *e2ecrypto.DeviceKeys, w any) Signed {
	t.Helper()
	b, err := proto.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := e2ecrypto.SignEd25519(by.Ed25519, CtxEpoch, b)
	if err != nil {
		t.Fatal(err)
	}
	return Signed{B: b, Sig: sig}
}

func TestEpochBundleRoundTrip(t *testing.T) {
	a, b := newDevice(t), newDevice(t)
	p := testParams(t, a, b)
	s, err := NewEpochBundle(rand.Reader, a, p)
	if err != nil {
		t.Fatal(err)
	}
	eb, err := s.VerifyEpochBundle(testSID, a.Ed25519Pub)
	if err != nil {
		t.Fatal(err)
	}
	if eb.Chan != 7 || eb.Epoch != 1 || !slices.Equal(eb.Roster, []uint64{1, 3}) || eb.By != a.ID ||
		eb.Time != p.Time || eb.Back != nil || len(eb.Boxes) != 2 || eb.Prev != [32]byte{} {
		t.Fatalf("decoded %+v", eb)
	}
	if eb.KC != e2ecrypto.KeyCommitment(p.K, testSID, 7, 1) {
		t.Fatal("kc mismatch")
	}
	// Each recipient opens the same K.
	for i, d := range []*e2ecrypto.DeviceKeys{a, b} {
		k, err := e2ecrypto.OpenKey(d.X25519, eb.Eph, e2ecrypto.BoxAD{ServerID: testSID, Chan: 7, Epoch: 1,
			Recipient: d.ID, By: a.ID}, eb.Boxes[i].CT[:])
		if err != nil || k != p.K {
			t.Fatalf("box %d: %v", i, err)
		}
	}
	if err := eb.CheckBoxKeys(keysOf(a, b)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(eb.BoxDevs(), []e2ecrypto.DevID{a.ID, b.ID}) {
		t.Fatal("box devs")
	}
	peek, err := PeekEpochBundle(s)
	if err != nil || peek.By != a.ID {
		t.Fatalf("peek: %v", err)
	}

	// Epoch 2 with a backlink to K_1.
	p2 := testParams(t, a)
	p2.Epoch, p2.Prev, p2.PrevK = 2, e2ecrypto.ChainHash(s.B), &p.K
	s2, err := NewEpochBundle(rand.Reader, a, p2)
	if err != nil {
		t.Fatal(err)
	}
	eb2, err := s2.VerifyEpochBundle(testSID, a.Ed25519Pub)
	if err != nil || eb2.Back == nil || eb2.Prev != e2ecrypto.ChainHash(s.B) {
		t.Fatalf("epoch 2: %v %+v", err, eb2)
	}
	if k, err := e2ecrypto.OpenBack(p2.K, testSID, 7, 2, eb2.Back[:]); err != nil || k != p.K {
		t.Fatalf("backlink: %v", err)
	}
	// The backlink is mandatory after epoch 1 and impossible on it.
	p3 := testParams(t, a)
	p3.Epoch, p3.Prev = 2, e2ecrypto.ChainHash(s.B)
	if _, err := NewEpochBundle(rand.Reader, a, p3); !errors.Is(err, e2ecrypto.ErrMalformed) {
		t.Fatalf("epoch 2 without PrevK: %v", err)
	}
	p4 := testParams(t, a)
	p4.PrevK = &p.K
	if _, err := NewEpochBundle(rand.Reader, a, p4); !errors.Is(err, e2ecrypto.ErrMalformed) {
		t.Fatalf("epoch 1 with PrevK: %v", err)
	}
}

func TestEpochBundleVerifyFailures(t *testing.T) {
	a, b := newDevice(t), newDevice(t)
	s, err := NewEpochBundle(rand.Reader, a, testParams(t, a, b))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyEpochBundle(testSID, b.Ed25519Pub); !errors.Is(err, e2ecrypto.ErrBadSignature) {
		t.Fatalf("other key: %v", err)
	}
	if _, err := s.VerifyEpochBundle(e2ecrypto.ServerID{9}, a.Ed25519Pub); !errors.Is(err, ErrWrongServer) {
		t.Fatalf("other server: %v", err)
	}
	// Signed under another context (a device revoke) doesn't verify as a bundle.
	sig, _ := e2ecrypto.SignEd25519(a.Ed25519, CtxRevokeDev, s.B)
	if _, err := (Signed{B: s.B, Sig: sig}).VerifyEpochBundle(testSID, a.Ed25519Pub); !errors.Is(err, e2ecrypto.ErrBadSignature) {
		t.Fatalf("cross-context: %v", err)
	}
	for i := range s.B {
		bb := slices.Clone(s.B)
		bb[i] ^= 1
		if _, err := (Signed{B: bb, Sig: s.Sig}).VerifyEpochBundle(testSID, a.Ed25519Pub); err == nil {
			t.Fatalf("flipped byte %d accepted", i)
		}
	}
	big := Signed{B: make([]byte, MaxBundleSize), Sig: s.Sig}
	if _, err := big.VerifyEpochBundle(testSID, a.Ed25519Pub); !errors.Is(err, e2ecrypto.ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	if _, err := PeekEpochBundle(big); !errors.Is(err, e2ecrypto.ErrTooLarge) {
		t.Fatalf("peek too large: %v", err)
	}
}

func TestEpochBundleDecodeRejects(t *testing.T) {
	a, b := newDevice(t), newDevice(t)
	good := func() epochBundleB {
		ct := make([]byte, e2ecrypto.BoxCTSize)
		return epochBundleB{V: 1, ServerID: testSID[:], Chan: 7, Epoch: 2, Prev: make([]byte, 32), Roster: []uint64{1, 2},
			By: a.ID[:], Time: 1, KC: make([]byte, 32), Eph: make([]byte, 32),
			Boxes: []epochBoxB{{Dev: a.ID[:], CT: ct}, {Dev: b.ID[:], CT: ct}}, Back: make([]byte, e2ecrypto.BackSize)}
	}
	if _, err := signBundleB(t, a, good()).VerifyEpochBundle(testSID, a.Ed25519Pub); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	type extra struct {
		epochBundleB `msgpack:",inline"`
		X            int `msgpack:"x"`
	}
	cases := map[string]struct {
		w    any
		want error
	}{
		"version":         {func() any { w := good(); w.V = 2; return w }(), e2ecrypto.ErrVersion},
		"epoch 0":         {func() any { w := good(); w.Epoch = 0; return w }(), e2ecrypto.ErrMalformed},
		"epoch 1 prev":    {func() any { w := good(); w.Epoch = 1; w.Prev[0] = 1; return w }(), e2ecrypto.ErrMalformed},
		"epoch 1 back":    {func() any { w := good(); w.Epoch = 1; return w }(), e2ecrypto.ErrMalformed},
		"epoch 2 no back": {func() any { w := good(); w.Back = nil; return w }(), e2ecrypto.ErrMalformed},
		"short back":      {func() any { w := good(); w.Back = make([]byte, 10); return w }(), e2ecrypto.ErrMalformed},
		"short prev":      {func() any { w := good(); w.Prev = w.Prev[:31]; return w }(), e2ecrypto.ErrMalformed},
		"short kc":        {func() any { w := good(); w.KC = nil; return w }(), e2ecrypto.ErrMalformed},
		"short by":        {func() any { w := good(); w.By = w.By[:8]; return w }(), e2ecrypto.ErrMalformed},
		"short ct":        {func() any { w := good(); w.Boxes[0].CT = w.Boxes[0].CT[:47]; return w }(), e2ecrypto.ErrMalformed},
		"empty roster":    {func() any { w := good(); w.Roster = []uint64{}; return w }(), e2ecrypto.ErrMalformed},
		"unsorted roster": {func() any { w := good(); w.Roster = []uint64{2, 1}; return w }(), e2ecrypto.ErrMalformed},
		"dup roster":      {func() any { w := good(); w.Roster = []uint64{1, 1}; return w }(), e2ecrypto.ErrMalformed},
		"no boxes":        {func() any { w := good(); w.Boxes = []epochBoxB{}; return w }(), e2ecrypto.ErrMalformed},
		"dup box dev":     {func() any { w := good(); w.Boxes[1].Dev = a.ID[:]; return w }(), ErrDuplicateBox},
		"extra key":       {extra{good(), 1}, e2ecrypto.ErrMalformed},
		"too many boxes": {func() any {
			w := good()
			w.Boxes = nil
			for i := range MaxBundleBoxes + 1 {
				w.Boxes = append(w.Boxes, epochBoxB{Dev: append(make([]byte, 14), byte(i>>8), byte(i)), CT: make([]byte, 48)})
			}
			return w
		}(), e2ecrypto.ErrTooLarge},
	}
	for name, c := range cases {
		s := signBundleB(t, a, c.w)
		if _, err := s.VerifyEpochBundle(testSID, a.Ed25519Pub); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
		if _, err := PeekEpochBundle(s); !errors.Is(err, c.want) {
			t.Errorf("%s: peek err = %v, want %v", name, err, c.want)
		}
	}
	// 128 boxes fit in the size limit.
	w := good()
	w.Boxes = nil
	for i := range MaxBundleBoxes {
		w.Boxes = append(w.Boxes, epochBoxB{Dev: append(make([]byte, 15), byte(i)), CT: make([]byte, 48)})
	}
	if s := signBundleB(t, a, w); len(s.B)+len(s.Sig) > MaxBundleSize {
		t.Fatalf("128 boxes = %d bytes", len(s.B)+len(s.Sig))
	} else if _, err := s.VerifyEpochBundle(testSID, a.Ed25519Pub); err != nil {
		t.Fatal(err)
	}
}

func TestEpochBundleRecipientKeys(t *testing.T) {
	a, b, c := newDevice(t), newDevice(t), newDevice(t)
	// A twin of b: another device id, the same X25519 key.
	twin := *b
	twin.ID[0] ^= 0xff
	if _, err := NewEpochBundle(rand.Reader, a, testParams(t, a, b, &twin)); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("duplicate X25519 key: %v", err)
	}
	if _, err := NewEpochBundle(rand.Reader, a, testParams(t, a, b, b)); !errors.Is(err, ErrDuplicateBox) {
		t.Fatalf("duplicate device: %v", err)
	}
	s, err := NewEpochBundle(rand.Reader, a, testParams(t, a, b))
	if err != nil {
		t.Fatal(err)
	}
	eb, _ := PeekEpochBundle(s)
	if err := eb.CheckBoxKeys(keysOf(a, c)); !errors.Is(err, ErrUnknownRecipient) {
		t.Fatalf("unknown recipient: %v", err)
	}
	keys := keysOf(a, b)
	keys[b.ID] = a.X25519Pub // the server claims b has a's key
	if err := eb.CheckBoxKeys(keys); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("shared key: %v", err)
	}
}
