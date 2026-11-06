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

// A frame far behind the rest of a spurt that hasn't started yet makes the
// trim span up to 2^31 seqs. trim clears by ring slot: it used to walk the
// span one seq at a time (seconds per pop, which the fuzzer reported as a
// hung worker). Playout jumps to the newest frames, in order.
func TestJitterTrimFarBehind(t *testing.T) {
	j := newJitter()
	now := time.Unix(1_800_000_000, 0)
	const high = 1 << 31
	for seq := uint32(high - 8); seq <= high; seq++ {
		j.push(seq, binary.BigEndian.AppendUint32(nil, seq), false, now)
	}
	j.push(1, binary.BigEndian.AppendUint32(nil, 1), false, now)
	// The fixed trim is 16 slot checks; the old walk took 2–20 s. A second
	// is far from both, even on a loaded -race runner.
	start := time.Now()
	k, d := j.pop()
	if el := time.Since(start); el > time.Second {
		t.Fatalf("one pop took %v", el)
	}
	if k != OutFrame || binary.BigEndian.Uint32(d) != high-uint32(j.target)+1 {
		t.Fatalf("pop %v %x, want the oldest of the newest %d", k, d, j.target)
	}
	if j.trimmed != uint64(1+9-j.target) {
		t.Fatalf("trimmed %d", j.trimmed)
	}
}
