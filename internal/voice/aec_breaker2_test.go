package voice

import (
	"math"
	"testing"
)

func farFrame(rms float64) []int32 {
	// A square wave has RMS == amplitude.
	pcm := make([]int32, FrameSamples)
	for i := range pcm {
		if (i/20)%2 == 0 {
			pcm[i] = int32(rms)
		} else {
			pcm[i] = -int32(rms)
		}
	}
	return pcm
}

// The floor never drops below gateFarRMS², and "active" needs 4× the
// floor, so the real threshold is 2·gateFarRMS = 520 RMS (≈ −36 dBFS), not
// the documented ≈ −42 dBFS. A quiet far-end talker (laptop mic without AGC,
// −38 dBFS) never shuts the gate: their voice is echoed back to them. The
// pre-floor gate shut on this level.
func TestGate_BreakQuietFarSpeechNeverGated(t *testing.T) {
	g := newGate()
	const rms = 400 // ≈ −38 dBFS: above gateFarRMS (−42 dBFS)
	g.far(farFrame(rms))
	if !g.shut() {
		t.Fatalf("far-end speech at RMS %d (%.1f dBFS, > gateFarRMS %d) does not shut the gate",
			rms, 20*math.Log10(rms/32768.0), gateFarRMS)
	}
}

// Continuous far-end audio without pauses (music, a TV, a sustained
// tone) raises the floor ×1.01/frame until it is within 6 dB of the
// signal, then the gate opens with the far end still playing at full
// level: everything after ~5 s is echoed straight back.
func TestGate_BreakContinuousFarAudioLeaksAfterSeconds(t *testing.T) {
	g := newGate()
	const rms = 2000         // ≈ −24 dBFS: ordinary speech/music level
	for i := range 50 * 10 { // 10 s
		g.far(farFrame(rms))
		if !g.shut() {
			t.Fatalf("gate opened after %.2f s of continuous far-end audio at RMS %d", float64(i+1)/50, rms)
		}
	}
}
