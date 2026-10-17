package voice

import (
	"encoding/binary"
	"math/rand/v2"
	"sort"
	"testing"
	"time"
)

func TestWindow(t *testing.T) {
	var w window
	accept := func(seq uint32) bool {
		if !w.check(seq) {
			return false
		}
		w.accept(seq)
		return true
	}
	for _, c := range []struct {
		seq  uint32
		want bool
	}{
		{10, true}, {10, false}, {9, true}, {12, true}, {11, true}, {9, false},
		{100, true}, {37, true}, {36, false}, {99, true}, {100, false}, {1000, true}, {999, true}, {937, true}, {936, false},
	} {
		if got := accept(c.seq); got != c.want {
			t.Fatalf("seq %d: got %v want %v", c.seq, got, c.want)
		}
	}
	// check alone never moves the window (forgeries are checked, not accepted).
	var w2 window
	w2.accept(5)
	w2.check(500)
	if !w2.check(4) {
		t.Fatal("check moved the window")
	}
}

func seqData(seq uint32) []byte { return binary.BigEndian.AppendUint32(nil, seq) }

type arrival struct {
	at  time.Duration
	seq uint32
}

// simulate sends n frames through a lossy, jittery, reordering network and
// plays them out on a 20 ms clock.
func simulate(t *testing.T, n int, loss float64, maxDelay time.Duration, seed uint64) (played []uint32, fec, conceal int) {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, 1))
	var arr []arrival
	dropped := map[uint32]bool{}
	for i := range n {
		if r.Float64() < loss {
			dropped[uint32(i)] = true
			continue
		}
		d := time.Duration(r.Int64N(int64(maxDelay) + 1))
		arr = append(arr, arrival{time.Duration(i)*FrameDur + d, uint32(i)})
	}
	sort.Slice(arr, func(a, b int) bool { return arr[a].at < arr[b].at })
	j := newJitter()
	base := time.Unix(0, 0)
	k := 0
	for tick := time.Duration(0); tick < time.Duration(n+40)*FrameDur; tick += FrameDur {
		for k < len(arr) && arr[k].at <= tick {
			// Duplicates must be harmless too.
			j.push(arr[k].seq, seqData(arr[k].seq), false, base.Add(arr[k].at))
			if r.IntN(20) == 0 {
				j.push(arr[k].seq, seqData(arr[k].seq), false, base.Add(arr[k].at))
			}
			k++
		}
		kind, data := j.pop()
		switch kind {
		case OutFrame:
			s := binary.BigEndian.Uint32(data)
			if len(played) > 0 && s <= played[len(played)-1] {
				t.Fatalf("played %d after %d", s, played[len(played)-1])
			}
			if dropped[s] {
				t.Fatalf("played dropped frame %d", s)
			}
			played = append(played, s)
		case OutFEC:
			fec++
		case OutConceal:
			conceal++
		}
	}
	return played, fec, conceal
}

func TestJitterCleanNetwork(t *testing.T) {
	played, fec, conceal := simulate(t, 200, 0, 0, 1)
	// The only concealment is the sender going quiet at the end.
	if len(played) != 200 || fec != 0 || conceal > maxMissing {
		t.Fatalf("played %d fec %d conceal %d", len(played), fec, conceal)
	}
}

func TestJitterLossAndReorder(t *testing.T) {
	for seed := range uint64(20) {
		// 10% loss, up to 50 ms of jitter (reorders by up to 2 frames).
		played, fec, conceal := simulate(t, 500, 0.10, 50*time.Millisecond, seed)
		if len(played) < 500*80/100 {
			t.Fatalf("seed %d: played only %d/500", seed, len(played))
		}
		if fec == 0 || fec+conceal == 0 {
			t.Fatalf("seed %d: losses never concealed (fec %d, plc %d)", seed, fec, conceal)
		}
	}
}

func TestJitterAdapts(t *testing.T) {
	j := newJitter()
	base := time.Unix(0, 0)
	r := rand.New(rand.NewPCG(3, 3))
	for i := range 200 {
		j.push(uint32(i), seqData(uint32(i)), false, base.Add(time.Duration(i)*FrameDur+time.Duration(r.Int64N(int64(80*time.Millisecond)))))
		for range 3 {
			j.pop()
		}
	}
	j.reset()
	j.pop()
	if j.target <= initialTarget {
		t.Fatalf("target %d didn't grow under 80 ms jitter (est %.1f ms)", j.target, j.jit)
	}
	if j.target > maxTarget {
		t.Fatalf("target %d over max", j.target)
	}
}

func TestJitterSpurts(t *testing.T) {
	j := newJitter()
	now := time.Unix(0, 0)
	// A one-frame spurt shorter than the target still plays.
	j.push(0, seqData(0), true, now)
	if k, _ := j.pop(); k != OutFrame {
		t.Fatalf("short spurt: %v", k)
	}
	if k, _ := j.pop(); k != OutSilence {
		t.Fatalf("after end: %v", k)
	}
	// A stale duplicate of the finished spurt never plays.
	j.push(0, seqData(0), true, now)
	if k, _ := j.pop(); k != OutSilence {
		t.Fatalf("stale replay played: %v", k)
	}
	// The next spurt starts after the target delay.
	for i := uint32(50); i < 53; i++ {
		j.push(i, seqData(i), false, now)
	}
	if k, d := j.pop(); k != OutFrame || binary.BigEndian.Uint32(d) != 50 {
		t.Fatalf("next spurt: %v", k)
	}
	// The sender vanishes: a few concealed frames, then silence.
	j.pop()
	j.pop()
	var kinds []OutKind
	for range 8 {
		k, _ := j.pop()
		kinds = append(kinds, k)
	}
	if kinds[0] != OutConceal || kinds[len(kinds)-1] != OutSilence {
		t.Fatalf("vanished sender: %v", kinds)
	}
}
