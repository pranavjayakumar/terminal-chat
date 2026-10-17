//go:build voice

package voice

import (
	"math"
	"testing"
)

func sine(frame int) []int16 {
	pcm := make([]int16, FrameSamples)
	for i := range pcm {
		t := float64(frame*FrameSamples+i) / SampleRate
		pcm[i] = int16(8000 * math.Sin(2*math.Pi*440*t))
	}
	return pcm
}

func rms(pcm []int16) float64 {
	var s float64
	for _, v := range pcm {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(pcm)))
}

func TestOpusRoundTrip(t *testing.T) {
	enc, err := opusCodec{}.NewEncoder()
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := opusCodec{}.NewDecoder()
	buf := make([]byte, maxOpus)
	out := make([]int16, FrameSamples)
	var sizes, energy float64
	for f := range 50 {
		n, err := enc.Encode(sine(f), buf)
		if err != nil || n == 0 || n > maxOpus {
			t.Fatalf("encode: %d %v", n, err)
		}
		sizes += float64(n)
		m, err := dec.Decode(buf[:n], out)
		if err != nil || m != FrameSamples {
			t.Fatalf("decode: %d %v", m, err)
		}
		if f >= 10 {
			energy += rms(out)
		}
	}
	// 24 kbit/s VBR ≈ 60 B per 20 ms frame.
	if avg := sizes / 50; avg < 20 || avg > 150 {
		t.Fatalf("avg frame %.0f B", avg)
	}
	if avg := energy / 40; avg < 3000 || avg > 8000 {
		t.Fatalf("decoded rms %.0f, want near %.0f", avg, 8000/math.Sqrt2)
	}
	n, _ := enc.Encode(sine(50), buf)
	if m, err := dec.DecodeFEC(buf[:n], out); err != nil || m != FrameSamples {
		t.Fatalf("fec: %d %v", m, err)
	}
	if m, err := dec.Conceal(out); err != nil || m != FrameSamples {
		t.Fatalf("plc: %d %v", m, err)
	}
	if _, err := dec.Decode([]byte{0xff, 0xff, 0xff}, out); err == nil {
		t.Log("garbage decoded without error (opus is permissive)")
	}
}

// TestOpusEngine runs real Opus through two engines with loss.
func TestOpusEngine(t *testing.T) {
	w := &wire{}
	a, _ := NewEngine(Config{Src: 1, Codec: opusCodec{}, Send: w.send})
	b, _ := NewEngine(Config{Src: 2, Codec: opusCodec{}, Send: func([]byte) error { return nil }})
	b.SetRoster(map[uint32]uint64{1: 10})
	a.SetTalking(true)
	for f := range 100 {
		a.Capture(sine(f))
	}
	var energy float64
	for i, g := range w.got {
		if i%7 != 3 { // ~14% loss
			b.Receive(g)
		}
		if i >= 10 {
			energy += rms(b.Playout())
		}
	}
	if avg := energy / 90; avg < 2000 {
		t.Fatalf("lossy playout rms %.0f", avg)
	}
}
