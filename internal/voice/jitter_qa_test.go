package voice

import (
	"encoding/binary"
	"testing"
	"time"
)

// A loss burst longer than maxMissing (a Wi-Fi roam) resets the stream;
// playout must pick up again right after it, not stall or replay stale
// frames.
func TestJitterRecoversAfterLossBurst(t *testing.T) {
	j := newJitter()
	base := time.Unix(0, 0)
	var played []uint32
	for i := range 340 {
		if i < 300 && (i < 100 || i >= 112) {
			j.push(uint32(i), seqData(uint32(i)), false, base.Add(time.Duration(i)*FrameDur))
		}
		if kind, data := j.pop(); kind == OutFrame {
			s := binary.BigEndian.Uint32(data)
			if len(played) > 0 && s <= played[len(played)-1] {
				t.Fatalf("played %d after %d", s, played[len(played)-1])
			}
			played = append(played, s)
		}
	}
	after := 0
	for _, s := range played {
		if s >= 112 {
			after++
		}
	}
	if len(played) < 280 || after < 185 {
		t.Fatalf("played %d frames, %d of 188 after the burst", len(played), after)
	}
}
