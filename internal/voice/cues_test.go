package voice

import "testing"

func TestCuePCM(t *testing.T) {
	for c, pcm := range cues() {
		if len(pcm) == 0 || len(pcm) > SampleRate/2 {
			t.Errorf("cue %d: %d samples", c, len(pcm))
		}
		peak := 0
		for _, v := range pcm {
			peak = max(peak, int(v), -int(v))
		}
		if peak < 4000 || peak > 32767/4+1 {
			t.Errorf("cue %d: peak %d (want about -12 dBFS)", c, peak)
		}
		if pcm[0] != 0 || abs(int(pcm[len(pcm)-1])) > 200 {
			t.Errorf("cue %d: clicks at the edges (%d, %d)", c, pcm[0], pcm[len(pcm)-1])
		}
	}
}

func abs(v int) int { return max(v, -v) }

func TestPlayCueMixesAndBounds(t *testing.T) {
	e, err := NewEngine(Config{Src: 1, Codec: fakeCodec{}, Send: func([]byte) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if out := e.Playout(); out[100] != 0 {
		t.Fatal("silence expected")
	}
	e.PlayCue(CueJoin, 100)
	total := len(cues()[CueJoin])
	heard := 0
	for range total/FrameSamples + 2 {
		for _, v := range e.Playout() {
			if v != 0 {
				heard++
			}
		}
	}
	if heard < total*9/10 {
		t.Fatalf("heard %d of %d cue samples", heard, total)
	}
	// A burst overlaps instead of queueing, stays within 1 s, and clips.
	for range 200 {
		e.PlayCue(CueLeave, 100)
	}
	e.mu.Lock()
	n := len(e.cue)
	e.mu.Unlock()
	if n > maxCueSamples || n != len(cues()[CueLeave]) {
		t.Fatalf("queued %d samples", n)
	}
	for _, v := range e.Playout() {
		if v > 32767 || v < -32768 {
			t.Fatal("unreachable")
		}
	}
	e.PlayCue(CueMicOn, 0) // volume 0: nothing
	e.PlayCue(Cue(99), 100)
}
