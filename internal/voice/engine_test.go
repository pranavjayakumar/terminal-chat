package voice

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// fakeCodec "encodes" a frame as its first sample; decoding fills the frame
// with it. Enough to check routing, ordering and mixing without libopus.
type fakeCodec struct{}

type fakeEnc struct{}
type fakeDec struct{ last int16 }

func (fakeCodec) NewEncoder() (Encoder, error) { return fakeEnc{}, nil }
func (fakeCodec) NewDecoder() (Decoder, error) { return &fakeDec{}, nil }

func (fakeEnc) Encode(pcm []int16, out []byte) (int, error) {
	binary.BigEndian.PutUint16(out, uint16(pcm[0]))
	return 2, nil
}

func (d *fakeDec) Decode(data []byte, pcm []int16) (int, error) {
	d.last = int16(binary.BigEndian.Uint16(data))
	for i := range FrameSamples {
		pcm[i] = d.last
	}
	return FrameSamples, nil
}

func (d *fakeDec) DecodeFEC(data []byte, pcm []int16) (int, error) {
	return d.Conceal(pcm)
}

func (d *fakeDec) Conceal(pcm []int16) (int, error) {
	for i := range FrameSamples {
		pcm[i] = d.last
	}
	return FrameSamples, nil
}

func frameOf(v int16) []int16 {
	f := make([]int16, FrameSamples)
	for i := range f {
		f[i] = v
	}
	return f
}

type wire struct {
	mu  sync.Mutex
	got [][]byte
}

func (w *wire) send(b []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, bytes.Clone(b))
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newPair(t *testing.T, enc bool, keys Keys) (*Engine, *Engine, *wire, *clock) {
	t.Helper()
	w := &wire{}
	c := &clock{time.Unix(1_800_000_000, 0)}
	a, err := NewEngine(Config{Src: 1, Enc: enc, Keys: keys, Codec: fakeCodec{}, Send: w.send, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewEngine(Config{Src: 2, Enc: enc, Keys: keys, Codec: fakeCodec{}, Send: func([]byte) error { return nil }, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	a.SetRoster(map[uint32]uint64{1: 10, 2: 20})
	b.SetRoster(map[uint32]uint64{1: 10, 2: 20})
	return a, b, w, c
}

func TestEnginePlaintext(t *testing.T) {
	a, b, w, c := newPair(t, false, nil)
	a.Capture(frameOf(5)) // not talking: nothing sent
	if len(w.got) != 0 {
		t.Fatal("sent while muted")
	}
	a.SetTalking(true)
	for v := int16(1); v <= 5; v++ {
		a.Capture(frameOf(v)[:500]) // odd periods are re-framed
		a.Capture(frameOf(v)[500:])
	}
	a.SetTalking(false)
	a.Capture(frameOf(6))
	a.Capture(frameOf(7))
	if len(w.got) != 6 {
		t.Fatalf("sent %d frames", len(w.got))
	}
	last, _ := proto.ParseDatagram(w.got[5])
	if last.Flags&proto.DgFlagEnd == 0 || last.Seq != 5 {
		t.Fatalf("last frame %+v", last)
	}
	// Deliver reordered with a duplicate.
	for _, i := range []int{1, 0, 2, 2, 4, 3, 5} {
		b.Receive(w.got[i])
	}
	if s := b.Stats(); s.Received != 6 || s.Replay != 1 {
		t.Fatalf("stats %+v", s)
	}
	var heard []int16
	for range 10 {
		c.t = c.t.Add(FrameDur)
		if f := b.Playout(); f[0] != 0 {
			heard = append(heard, f[0])
		}
	}
	if want := []int16{1, 2, 3, 4, 5, 6}; !equal(heard, want) {
		t.Fatalf("heard %v want %v", heard, want)
	}
	b.Receive(w.got[3]) // replay after playout
	if s := b.Stats(); s.Replay != 2 {
		t.Fatalf("late replay not caught: %+v", s)
	}
	// Unknown src and garbage.
	b.Receive(proto.Datagram{Kind: proto.DgAudio, Src: 9, Payload: []byte{0, 1}}.Marshal())
	b.Receive([]byte{1, 2, 3})
	b.Receive(proto.Datagram{Kind: proto.DgAudioEnc, Src: 1, Payload: make([]byte, 20)}.Marshal())
	if s := b.Stats(); s.Unknown != 1 || s.BadFrame != 2 {
		t.Fatalf("stats %+v", s)
	}
}

func TestEngineMixAndSpeaking(t *testing.T) {
	c := &clock{time.Unix(1_800_000_000, 0)}
	var ws [2]wire
	var senders []*Engine
	for i := range 2 {
		e, _ := NewEngine(Config{Src: uint32(i + 1), Codec: fakeCodec{}, Send: ws[i].send, Now: c.now})
		e.SetTalking(true)
		senders = append(senders, e)
	}
	r, _ := NewEngine(Config{Src: 3, Codec: fakeCodec{}, Send: func([]byte) error { return nil }, Now: c.now})
	r.SetRoster(map[uint32]uint64{1: 10, 2: 20, 3: 30})
	for range 4 {
		senders[0].Capture(frameOf(30000))
		senders[1].Capture(frameOf(10000))
	}
	for i := range 2 {
		for _, b := range ws[i].got {
			r.Receive(b)
		}
	}
	f := r.Playout()
	if f[0] != 32767 {
		t.Fatalf("mix should clip to 32767, got %d", f[0])
	}
	if sp := r.Speaking(); !sp[10] || !sp[20] || sp[30] {
		t.Fatalf("speaking %v", sp)
	}
	c.t = c.t.Add(time.Second)
	if sp := r.Speaking(); len(sp) != 0 {
		t.Fatalf("still speaking %v", sp)
	}
	// Leaving the roster drops the stream.
	r.SetRoster(map[uint32]uint64{2: 20, 3: 30})
	r.Receive(ws[0].got[3])
	if r.Stats().Unknown != 1 {
		t.Fatal("frame from a departed src accepted")
	}
}

// fakeKeys: one sender key per (src, epoch); epochs listed in ok accepted.
type fakeKeys struct {
	vk   map[uint32]e2ecrypto.Key
	dev  map[uint32]e2ecrypto.DevID
	st   [16]byte
	send uint32
}

func (k *fakeKeys) sender(src, ep uint32) *e2ecrypto.VoiceSender {
	s, _ := e2ecrypto.NewVoiceSender(k.vk[ep], k.dev[src], k.st)
	return s
}

func (k *fakeKeys) Send() (uint32, *e2ecrypto.VoiceSender, bool) {
	if _, ok := k.vk[k.send]; !ok {
		return 0, nil, false
	}
	return k.send, k.sender(1, k.send), true
}

func (k *fakeKeys) Recv(src, ep uint32) (*e2ecrypto.VoiceSender, bool) {
	if _, ok := k.vk[ep]; !ok || k.dev[src] == (e2ecrypto.DevID{}) {
		return nil, false
	}
	return k.sender(src, ep), true
}

func newKeys() *fakeKeys {
	var sid e2ecrypto.ServerID
	k1, k2 := e2ecrypto.Key{1}, e2ecrypto.Key{2}
	vk1, _ := e2ecrypto.VoiceKey(k1, sid, 5, 1)
	vk2, _ := e2ecrypto.VoiceKey(k2, sid, 5, 2)
	return &fakeKeys{
		vk:   map[uint32]e2ecrypto.Key{1: vk1, 2: vk2},
		dev:  map[uint32]e2ecrypto.DevID{1: {1}, 2: {2}},
		send: 1,
	}
}

func TestEngineEncrypted(t *testing.T) {
	keys := newKeys()
	a, b, w, c := newPair(t, true, keys)
	a.SetTalking(true)
	for v := int16(1); v <= 4; v++ {
		a.Capture(frameOf(v))
	}
	keys.send = 2 // rotation mid-spurt
	a.Capture(frameOf(5))
	if len(w.got) != 5 {
		t.Fatalf("sent %d", len(w.got))
	}
	for _, g := range w.got {
		if d, _ := proto.ParseDatagram(g); d.Kind != proto.DgAudioEnc || bytes.Contains(g[proto.DgEncHeaderLen:], []byte{0, 3}) {
			t.Fatalf("frame not encrypted: %x", g)
		}
	}
	d4, _ := proto.ParseDatagram(w.got[4])
	if d4.Epoch != 2 {
		t.Fatalf("sender didn't move to the new epoch: %d", d4.Epoch)
	}
	for _, g := range w.got[:5] {
		b.Receive(g)
	}
	if s := b.Stats(); s.Received != 5 {
		t.Fatalf("stats %+v", s)
	}

	forge := func(name string, mut func([]byte) []byte, field func(Stats) uint64) {
		t.Helper()
		before := field(b.Stats())
		b.Receive(mut(bytes.Clone(w.got[0])))
		if field(b.Stats()) != before+1 {
			t.Fatalf("%s not rejected: %+v", name, b.Stats())
		}
	}
	auth := func(s Stats) uint64 { return s.Auth }
	// A replay is caught by the window before any crypto.
	forge("replay", func(g []byte) []byte { return g }, func(s Stats) uint64 { return s.Replay })
	// Fresh seq with a re-used ciphertext: AEAD binds seq.
	forge("seq", func(g []byte) []byte { g[9] = 100; return g }, auth)
	forge("flags", func(g []byte) []byte { g[1] ^= 1; g[9] = 101; return g }, auth)
	forge("ts", func(g []byte) []byte { g[17]++; g[9] = 102; return g }, auth)
	forge("ct", func(g []byte) []byte { g[len(g)-1] ^= 1; g[9] = 103; return g }, auth)
	forge("epoch", func(g []byte) []byte { g[13] = 2; g[9] = 104; return g }, auth)
	forge("unknown epoch", func(g []byte) []byte { g[13] = 9; g[9] = 105; return g }, func(s Stats) uint64 { return s.Key })
	forge("plaintext downgrade", func(g []byte) []byte {
		return proto.Datagram{Kind: proto.DgAudio, Src: 1, Seq: 106, Payload: []byte{0, 7}}.Marshal()
	}, func(s Stats) uint64 { return s.BadFrame })
	// Another member's src label on our frame: different sender key.
	b.SetRoster(map[uint32]uint64{1: 10, 2: 20, 3: 30})
	keys.dev[3] = e2ecrypto.DevID{3}
	forge("relabel", func(g []byte) []byte { g[5] = 3; return g }, auth)

	// A frame recorded now but delivered two minutes later (never seen, so
	// the window can't catch it) fails the ts check.
	a.Capture(frameOf(6))
	late := w.got[len(w.got)-1]
	c.t = c.t.Add(2 * time.Minute)
	before := b.Stats().Skew
	b.Receive(late)
	if b.Stats().Skew != before+1 {
		t.Fatal("old recording accepted")
	}

	// No verified key to send with: nothing goes out, never plaintext.
	keys.send = 7
	n := len(w.got)
	a.Capture(frameOf(9))
	if len(w.got) != n {
		t.Fatal("sent without a key")
	}
}

func TestSessionCallback(t *testing.T) {
	var cb func(out, in []int16)
	w := &wire{}
	s, err := Start(Config{Src: 1, Send: w.send}, Platform{Codec: fakeCodec{}, Open: func(f func(out, in []int16)) (Duplex, error) {
		cb = f
		return closer{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.SetTalking(true)
	out := make([]int16, 441) // periods needn't match frames
	for range 10 {
		cb(out, frameOf(3)[:441])
	}
	if len(w.got) != 4 {
		t.Fatalf("sent %d frames from 4410 samples", len(w.got))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

type closer struct{}

func (closer) Close() error { return nil }

func equal(a, b []int16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A roster that drops and re-adds a src must not reset its replay window
// (protocol-guardian/security review: server-driven replay).
func TestEngineReplayAcrossRosterChange(t *testing.T) {
	keys := newKeys()
	a, b, w, _ := newPair(t, true, keys)
	a.SetTalking(true)
	a.Capture(frameOf(1))
	b.Receive(w.got[0])
	b.SetRoster(map[uint32]uint64{2: 20})
	b.SetRoster(map[uint32]uint64{1: 10, 2: 20})
	b.Receive(w.got[0])
	if s := b.Stats(); s.Received != 1 || s.Replay != 1 {
		t.Fatalf("replay after roster churn accepted: %+v", s)
	}
}
