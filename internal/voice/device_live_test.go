//go:build voice

package voice

import (
	"math"
	"os"
	"sync"
	"testing"
	"time"
)

// TestLiveDevice opens the real sound card through Native for two seconds
// (TC_VOICE_LIVE=1 only: it needs a microphone and permission to use it),
// plays a quiet tone and reports what came back. With speakers and
// TC_VOICE_AEC=off the tone's echo shows up in the mic peak; with the
// platform canceller it should mostly not.
func TestLiveDevice(t *testing.T) {
	if os.Getenv("TC_VOICE_LIVE") != "1" {
		t.Skip("set TC_VOICE_LIVE=1 to open the sound card")
	}
	p, err := Native()
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu       sync.Mutex
		calls, n int
		peak     int16
		phase    float64
	)
	d, err := p.Open(func(out, in []int16) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		n += len(out)
		for i := range out {
			out[i] = int16(2000 * math.Sin(phase))
			phase += 2 * math.Pi * 440 / SampleRate
		}
		for _, v := range in {
			peak = max(peak, v, -v)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	sys := false
	if a, ok := d.(systemAEC); ok {
		sys = a.SystemAEC()
	}
	time.Sleep(2 * time.Second)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("aec %v (system=%v): %d callbacks, %d samples (%.0f/s), mic peak %d",
		p.AEC.resolve(sys), sys, calls, n, float64(n)/2, peak)
	if calls == 0 {
		t.Fatal("the device never called back")
	}
}
