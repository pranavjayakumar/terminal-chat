package voice

import (
	"math"
	"testing"
)

// TestMixNoClipping: two loud talkers summing past int16 are limited, not
// clipped: no sample passes the ceiling, no sample changes sign (the
// waveform keeps its shape), and the gain returns to unity once the
// overload ends.
func TestMixNoClipping(t *testing.T) {
	e, _ := NewEngine(Config{Src: 1, Codec: fakeCodec{}, Send: func([]byte) error { return nil }})
	out := make([]int16, FrameSamples)
	n := 0
	for f := range 100 {
		amp := 25000.0
		if f >= 50 {
			amp = 4000 // the overload ends
		}
		for i := range e.mix {
			x := float64(n) / SampleRate
			n++
			e.mix[i] = int32(amp*math.Sin(2*math.Pi*300*x)) + int32(amp*math.Sin(2*math.Pi*470*x))
		}
		e.limitMix(out)
		var peak int
		for i, v := range out {
			peak = max(peak, int(v), -int(v))
			if v != 0 && (v > 0) != (e.mix[i] > 0) {
				t.Fatalf("frame %d sample %d: sign flipped (%d → %d)", f, i, e.mix[i], v)
			}
		}
		if peak > limitCeil {
			t.Fatalf("frame %d: peak %d over the ceiling", f, peak)
		}
	}
	if e.limit != 1 {
		t.Fatalf("gain %.3f didn't recover after the overload", e.limit)
	}
	// Recovered: quiet audio passes bit-exact.
	for i := range e.mix {
		e.mix[i] = int32(i % 1000)
	}
	e.limitMix(out)
	for i, v := range out {
		if int32(v) != e.mix[i] {
			t.Fatalf("sample %d: %d → %d at unity gain", i, e.mix[i], v)
		}
	}
}
