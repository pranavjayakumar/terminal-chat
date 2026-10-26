package voice

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

// TestJitterTrimsBacklog: a stall followed by a burst (the TCP fallback
// delivers late, never lost) must not add the stall to the delay for the
// rest of the spurt: frames beyond target+maxExcess are dropped and
// playout catches up to the newest.
func TestJitterTrimsBacklog(t *testing.T) {
	j := newJitter()
	now := time.Unix(0, 0)
	seq := uint32(0)
	// Steady start.
	for range 10 {
		j.push(seq, seqData(seq), false, now)
		seq++
		now = now.Add(FrameDur)
		j.pop()
	}
	// 300 ms stall: playout conceals meanwhile (5 slots → quiet, reset).
	for range 15 {
		j.pop()
	}
	// The stalled 15 frames and the next ones arrive at once.
	for range 15 {
		j.push(seq, seqData(seq), false, now)
		seq++
	}
	newest := seq - 1
	var first uint32
	for range 20 {
		k, d := j.pop()
		if k == OutFrame {
			first = binary.BigEndian.Uint32(d)
			break
		}
	}
	if lag := newest - first; lag > uint32(j.target) {
		t.Fatalf("playing seq %d with %d newer frames queued (target %d): backlog kept", first, lag, j.target)
	}
	if j.trimmed == 0 {
		t.Fatal("nothing counted as trimmed")
	}
	// Mid-spurt too: a burst on a running stream is trimmed to the target.
	j2 := newJitter()
	for s := range uint32(4) {
		j2.push(s, seqData(s), false, now)
	}
	j2.pop()
	for s := uint32(4); s < 16; s++ {
		j2.push(s, seqData(s), false, now)
	}
	_, d := j2.pop()
	if got := binary.BigEndian.Uint32(d); 15-got >= uint32(j2.target+maxExcess) {
		t.Fatalf("played %d, 15 queued: not trimmed", got)
	}
}

// TestJitterTargetShrinksSlowly: a delay that a late frame just grew is
// not thrown away at the next spurt (which would conceal and stretch
// again), only shrunk one frame per spurt.
func TestJitterTargetShrinksSlowly(t *testing.T) {
	j := newJitter()
	j.target = 8
	j.jit = 0 // a quiet network says 1 frame
	j.push(0, seqData(0), true, time.Unix(0, 0))
	j.pop()
	if j.target != 7 {
		t.Fatalf("target %d after one spurt, want 7", j.target)
	}
	for i := uint32(1); i < 20; i++ {
		j.push(i, seqData(i), true, time.Unix(int64(i), 0))
		j.pop()
	}
	if j.target != minTarget {
		t.Fatalf("target %d, want floor %d", j.target, minTarget)
	}
}

// lossEnc records what FEC was told.
type lossEnc struct {
	fakeEnc
	mu   sync.Mutex
	loss []int
}

func (l *lossEnc) SetLoss(p int) {
	l.mu.Lock()
	l.loss = append(l.loss, p)
	l.mu.Unlock()
}

type lossCodec struct{ enc *lossEnc }

func (c lossCodec) NewEncoder() (Encoder, error) { return c.enc, nil }
func (lossCodec) NewDecoder() (Decoder, error)   { return &fakeDec{}, nil }

// TestFECFollowsLoss: our FEC follows the loss we measure on what we hear:
// off on a clean link, about the loss rate on a lossy one, and always off
// on the TCP fallback.
func TestFECFollowsLoss(t *testing.T) {
	run := func(reliable bool, drop func(int) bool) (*Engine, *lossEnc) {
		le := &lossEnc{}
		w := &wire{}
		far, _ := NewEngine(Config{Src: 2, Codec: fakeCodec{}, Send: w.send})
		far.SetTalking(true)
		e, _ := NewEngine(Config{Src: 1, Reliable: reliable, Codec: lossCodec{le}, Send: func([]byte) error { return nil }})
		e.SetRoster(map[uint32]uint64{2: 20})
		e.SetTalking(true)
		for i := range 600 {
			far.Capture(frameOf(1000))
			w.mu.Lock()
			b := w.got[len(w.got)-1]
			w.mu.Unlock()
			if !drop(i) {
				e.Receive(b)
			}
			e.Playout()
			e.Capture(frameOf(500))
		}
		return e, le
	}
	_, clean := run(false, func(int) bool { return false })
	if len(clean.loss) == 0 || clean.loss[len(clean.loss)-1] != 0 {
		t.Fatalf("clean link: FEC told %v, want ending at 0", clean.loss)
	}
	e, lossy := run(false, func(i int) bool { return i%10 == 5 })
	got := lossy.loss[len(lossy.loss)-1]
	if got < 8 || got > 13 {
		t.Fatalf("10%% loss: FEC told %v", lossy.loss)
	}
	if st := e.Stats(); st.Lost == 0 || st.LossPct < 7 || st.LossPct > 13 {
		t.Fatalf("stats %+v", st)
	}
	_, tcp := run(true, func(i int) bool { return i%10 == 5 })
	for _, p := range tcp.loss {
		if p != 0 {
			t.Fatalf("reliable transport: FEC told %v", tcp.loss)
		}
	}
}

// TestGateKeepsOnsets: with nothing playing, an open mic's first words
// pass untouched from the first sample; after remote audio, only the
// documented hangover (200 ms) and release ramp (20 ms) are attenuated.
func TestGateKeepsOnsets(t *testing.T) {
	g := newGate()
	silence := make([]int32, FrameSamples)
	g.far(silence)
	g.reset()
	near := tone440(FrameSamples, 3000)
	want := append([]int16(nil), near...)
	g.apply(near)
	if !equal(near, want) {
		t.Fatal("onset attenuated with nothing playing")
	}
	// Far talk, then silence: count attenuated near samples from the moment
	// the far end goes quiet.
	g.far(widen(tone440(FrameSamples, 3000)))
	attenuated := 0
	for range 30 {
		g.far(silence)
		near := tone440(FrameSamples, 3000)
		want := append([]int16(nil), near...)
		g.apply(near)
		for i := range near {
			if near[i] != want[i] {
				attenuated++
			}
		}
	}
	if limit := gateHang + SampleRate/50 + FrameSamples; attenuated > limit {
		t.Fatalf("%d samples attenuated, documented hangover+ramp is %d", attenuated, limit)
	}
}
