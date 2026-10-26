//go:build voice

package voice

import (
	"math"
	"math/cmplx"
	"math/rand/v2"
	"testing"
	"time"
)

// speechLike is a synthetic talker: a glottal pulse train (f0 gliding
// 110–220 Hz) shaped by four formants, with fricative noise bursts above
// 4 kHz. Not speech, but it has speech's spectrum, including the high band
// that bandwidth decisions cut.
func speechLike(seconds float64) []int16 {
	n := int(seconds * SampleRate)
	out := make([]int16, n)
	r := rand.New(rand.NewPCG(1, 2))
	formants := []struct{ f, bw, g float64 }{{700, 130, 1}, {1220, 70, 0.6}, {2600, 160, 0.3}, {3500, 250, 0.15}}
	type res struct{ a1, a2, b0, y1, y2 float64 }
	rs := make([]res, len(formants))
	for i, f := range formants {
		rr := math.Exp(-math.Pi * f.bw / SampleRate)
		th := 2 * math.Pi * f.f / SampleRate
		rs[i] = res{a1: 2 * rr * math.Cos(th), a2: -rr * rr, b0: f.g * (1 - rr)}
	}
	var phase, hp1, hpx float64
	for i := range n {
		t := float64(i) / SampleRate
		f0 := 165 + 55*math.Sin(2*math.Pi*0.7*t)
		phase += f0 / SampleRate
		var src float64
		if phase >= 1 {
			phase--
			src = 1
		}
		var v float64
		for k := range rs {
			y := rs[k].b0*src + rs[k].a1*rs[k].y1 + rs[k].a2*rs[k].y2
			rs[k].y2, rs[k].y1 = rs[k].y1, y
			v += y
		}
		// Fricatives: 80 ms of high-passed noise every 400 ms.
		if math.Mod(t, 0.4) < 0.08 {
			x := r.NormFloat64()
			hp1 = 0.6*(hp1+x-hpx) - 0.2*hp1
			hpx = x
			v += 0.02 * hp1
		}
		out[i] = int16(max(-32767, min(32767, v*60000)))
	}
	return out
}

// codecRun encodes and decodes pcm frame by frame, dropping frames where
// lost(i) (FEC from the next frame if it arrived, else PLC).
func codecRun(t *testing.T, c opusCodec, pcm []int16, lost func(int) bool) (dec []int16, bytes int, bw string, perFrame time.Duration) {
	enc, err := c.NewEncoder()
	if err != nil {
		t.Fatal(err)
	}
	d, _ := opusCodec{}.NewDecoder()
	frames := len(pcm) / FrameSamples
	pkts := make([][]byte, frames)
	start := time.Now()
	for i := range frames {
		buf := make([]byte, maxOpus)
		n, err := enc.Encode(pcm[i*FrameSamples:(i+1)*FrameSamples], buf)
		if err != nil {
			t.Fatal(err)
		}
		pkts[i] = buf[:n]
		bytes += n
	}
	perFrame = time.Since(start) / time.Duration(frames)
	bw = packetBandwidth(pkts[frames/2])
	out := make([]int16, FrameSamples)
	for i := range frames {
		switch {
		case !lost(i):
			_, err = d.Decode(pkts[i], out)
		case i+1 < frames && !lost(i+1):
			_, err = d.DecodeFEC(pkts[i+1], out)
		default:
			_, err = d.Conceal(out)
		}
		if err != nil {
			t.Fatal(err)
		}
		dec = append(dec, out...)
	}
	return dec, bytes, bw, perFrame
}

// align finds the codec delay (lag maximizing correlation).
func align(ref, got []int16) int {
	best, lag := math.Inf(-1), 0
	for l := 0; l < 1500; l++ {
		var s float64
		for i := 4800; i < 4800+24000 && i+l < len(got); i++ {
			s += float64(ref[i]) * float64(got[i+l])
		}
		if s > best {
			best, lag = s, l
		}
	}
	return lag
}

// segSNR is the mean per-20 ms SNR in dB, each clamped to [-10, 35].
func segSNR(ref, got []int16, lag int) float64 {
	var sum float64
	var k int
	for f := 5; (f+1)*FrameSamples+lag < len(got) && (f+1)*FrameSamples < len(ref); f++ {
		var sig, noise float64
		for i := f * FrameSamples; i < (f+1)*FrameSamples; i++ {
			x, y := float64(ref[i]), float64(got[i+lag])
			sig += x * x
			noise += (x - y) * (x - y)
		}
		if sig < 1e4*FrameSamples/100 {
			continue // near-silent frame
		}
		sum += max(-10, min(35, 10*math.Log10(sig/max(noise, 1))))
		k++
	}
	return sum / float64(k)
}

func fft(a []complex128) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		w := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for i := 0; i < n; i += size {
			wk := complex(1, 0)
			for k := 0; k < size/2; k++ {
				u, v := a[i+k], a[i+k+size/2]*wk
				a[i+k], a[i+k+size/2] = u+v, u-v
				wk *= w
			}
		}
	}
}

// lsd is the log-spectral distance (dB, lower is better) over 50 Hz–20 kHz,
// a codec-agnostic proxy that also catches a lost high band.
func lsd(ref, got []int16, lag int) float64 {
	const n = 1024
	var sum float64
	var k int
	a, b := make([]complex128, n), make([]complex128, n)
	for off := 4800; off+n+lag < len(got) && off+n < len(ref); off += n / 2 {
		var e float64
		for i := range n {
			w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/n)
			a[i] = complex(w*float64(ref[off+i]), 0)
			b[i] = complex(w*float64(got[off+i+lag]), 0)
			e += real(a[i]) * real(a[i])
		}
		if e < 1e6 {
			continue
		}
		fft(a)
		fft(b)
		var d float64
		var m int
		for i := n * 50 / SampleRate; i < n*20000/SampleRate; i++ {
			pa, pb := cmplx.Abs(a[i])+1, cmplx.Abs(b[i])+1
			x := 20 * math.Log10(pa/pb)
			d += x * x
			m++
		}
		sum += math.Sqrt(d / float64(m))
		k++
	}
	return sum / float64(k)
}

// TestOpusQuality compares the v0.4 encoder settings (24 kbit/s,
// complexity 5, FEC for 10 % loss) with the current ones on a speech-like
// signal: segmental SNR and log-spectral distance on a clean link and at
// 10 % loss, the coded bandwidth and the encode cost per frame.
func TestOpusQuality(t *testing.T) {
	pcm := speechLike(4)
	r := rand.New(rand.NewPCG(3, 4))
	lossy := make([]bool, len(pcm)/FrameSamples)
	for i := range lossy {
		lossy[i] = r.IntN(10) == 0
	}
	clean := func(int) bool { return false }
	lost10 := func(i int) bool { return lossy[i] }
	type result struct{ snr, lsd, lossSNR, lossLSD, kbps float64 }
	measure := func(name string, c, cLossy opusCodec) result {
		dec, bytes, bw, cost := codecRun(t, c, pcm, clean)
		lag := align(pcm, dec)
		var res result
		res.snr, res.lsd = segSNR(pcm, dec, lag), lsd(pcm, dec, lag)
		res.kbps = float64(bytes*8) / (float64(len(pcm)) / SampleRate) / 1000
		dl, _, bwLossy, _ := codecRun(t, cLossy, pcm, lost10)
		res.lossSNR, res.lossLSD = segSNR(pcm, dl, lag), lsd(pcm, dl, lag)
		t.Logf("%-6s %4.1f kbit/s %-3s clean: segSNR %5.2f dB LSD %5.2f dB · 10%% loss (%s): segSNR %5.2f dB LSD %5.2f dB · encode %v/frame",
			name, res.kbps, bw, res.snr, res.lsd, bwLossy, res.lossSNR, res.lossLSD, cost)
		return res
	}
	old := opusCodec{24000, 5, 10}
	before := measure("before", old, old)
	// The engine drives the loss setting from what it measures: 0 on a
	// clean link, the measured rate on a lossy one.
	after := measure("after", opusCodec{opusBitrate, opusComplexity, 0}, opusCodec{opusBitrate, opusComplexity, 10})
	if after.lsd >= before.lsd || after.snr <= before.snr {
		t.Errorf("clean link not better: before %+v after %+v", before, after)
	}
	if after.lossLSD >= before.lossLSD {
		t.Errorf("lossy link not better: before %+v after %+v", before, after)
	}
	if after.kbps > 40 {
		t.Errorf("%.1f kbit/s: VBR overshoot", after.kbps)
	}
}

// TestOpusEncodeCost keeps complexity affordable: an encode must stay a
// small fraction of the 20 ms frame (race detector off: it slows cgo calls
// only marginally, but CI machines are slow).
func TestOpusEncodeCost(t *testing.T) {
	pcm := speechLike(2)
	_, _, _, cost := codecRun(t, opusCodec{}, pcm, func(int) bool { return false })
	if cost > 4*time.Millisecond {
		t.Fatalf("encode %v per 20 ms frame", cost)
	}
}

// TestOpusMaxFrame: the largest packet our settings make, sealed, fits a
// datagram (§14.3) with room to spare.
func TestOpusMaxFrame(t *testing.T) {
	enc, _ := opusCodec{}.NewEncoder()
	enc.(*opusEnc).SetLoss(25)
	r := rand.New(rand.NewPCG(5, 6))
	buf := make([]byte, maxOpus)
	most := 0
	for range 200 {
		pcm := make([]int16, FrameSamples)
		for i := range pcm {
			pcm[i] = int16(r.IntN(65536) - 32768) // white noise: worst case for VBR
		}
		n, err := enc.Encode(pcm, buf)
		if err != nil {
			t.Fatal(err)
		}
		most = max(most, n)
	}
	if most > maxOpus/2 {
		t.Fatalf("largest packet %d B, limit %d", most, maxOpus)
	}
	t.Logf("largest packet %d B (limit %d)", most, maxOpus)
}
