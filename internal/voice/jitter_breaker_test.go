package voice

import (
	"encoding/binary"
	"testing"
	"time"
)

// FuzzJitter drives one stream's jitter buffer with adversarial seq values
// (0, 2^32-1, huge jumps, duplicates, reordering) and checks that playout
// never repeats or goes back: every OutFrame carries a strictly greater seq
// than the one before.
func FuzzJitter(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 1, 1, 0xff, 0xff, 0xff, 0xff, 0xfe, 2})
	f.Add([]byte{0xff, 0xff, 0xff, 0xfe, 0, 0, 0, 0, 0, 0, 0x10, 0, 0, 0, 1})
	f.Fuzz(func(t *testing.T, ops []byte) {
		j := newJitter()
		now := time.Unix(1_800_000_000, 0)
		var last uint32
		have := false
		for len(ops) >= 5 {
			op, seq := ops[0], binary.BigEndian.Uint32(ops[1:5])
			ops = ops[5:]
			now = now.Add(time.Duration(op&0x0f) * time.Millisecond * 5)
			data := binary.BigEndian.AppendUint32(nil, seq)
			if op&0x80 == 0 {
				j.push(seq, data, op&0x40 != 0, now)
			}
			for range int(op>>4&0x3) + 1 {
				k, d := j.pop()
				if k != OutFrame {
					continue
				}
				got := binary.BigEndian.Uint32(d)
				if have && got <= last {
					t.Fatalf("played seq %d after %d", got, last)
				}
				last, have = got, true
			}
		}
	})
}
