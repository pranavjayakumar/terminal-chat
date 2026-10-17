package voice

import (
	"math"
	"sync"
)

// In-huddle sound cues (docs/research/NOTIFICATIONS-2026-10.md §4.3):
// synthesized once into PCM and mixed into playback below the voices — no
// audio files, nothing in the binary. Only audible where a device is open,
// i.e. in a huddle of a voice build.

// Cue is an in-huddle sound.
type Cue uint8

const (
	CueJoin   Cue = iota + 1 // someone joined our huddle
	CueLeave                 // someone left it
	CueMicOn                 // our mic opened
	CueMicOff                // our mic closed
)

// maxCueSamples bounds queued cue audio (1 s): cues never pile up.
const maxCueSamples = SampleRate

var (
	cueOnce sync.Once
	cuePCM  map[Cue][]int16
)

// tone is a sum of partials (Hz) with a linear attack and release, peak
// -12 dBFS.
func tone(ms, attack, release int, hz ...float64) []int16 {
	n := SampleRate * ms / 1000
	a, r := SampleRate*attack/1000, SampleRate*release/1000
	out := make([]int16, n)
	const peak = 0.25 * 32767
	for i := range out {
		env := 1.0
		if i < a {
			env = float64(i) / float64(a)
		}
		if left := n - i; left < r {
			env = math.Min(env, float64(left)/float64(r))
		}
		var v float64
		for _, f := range hz {
			v += math.Sin(2 * math.Pi * f * float64(i) / SampleRate)
		}
		out[i] = int16(peak * env * v / float64(len(hz)))
	}
	return out
}

func cues() map[Cue][]int16 {
	cueOnce.Do(func() {
		cuePCM = map[Cue][]int16{
			CueJoin:   append(tone(80, 10, 60, 660), tone(80, 10, 60, 880)...),
			CueLeave:  append(tone(80, 10, 60, 880), tone(80, 10, 60, 660)...),
			CueMicOn:  tone(20, 2, 10, 1200),
			CueMicOff: tone(20, 2, 10, 700),
		}
	})
	return cuePCM
}

// PlayCue queues c at volume 0–100 for playback; it never blocks on the
// audio thread for long (one lock, a bounded copy).
func (e *Engine) PlayCue(c Cue, volume int) {
	pcm := cues()[c]
	if len(pcm) == 0 || volume <= 0 {
		return
	}
	volume = min(volume, 100)
	e.mu.Lock()
	defer e.mu.Unlock()
	// Mix over what's queued rather than after it: a burst of cues
	// overlaps instead of queueing seconds of clicks.
	if len(e.cue) < len(pcm) {
		e.cue = append(e.cue, make([]int32, len(pcm)-len(e.cue))...)
	}
	for i, v := range pcm {
		e.cue[i] += int32(v) * int32(volume) / 100
	}
	if len(e.cue) > maxCueSamples {
		e.cue = e.cue[:maxCueSamples]
	}
}
