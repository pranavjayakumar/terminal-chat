package screen

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// hostileKey is a well-formed 400x200 keyframe whose decompressed body is
// just under maxBody: every row one span of all 400 cells, its text unit
// repeated (~10 KiB a row). It compresses to a few chunks.
func hostileKey(keyID uint32, unit string) []byte {
	var b []byte
	b = append(b, version, typeKey)
	b = binary.BigEndian.AppendUint32(b, keyID)
	b = binary.BigEndian.AppendUint32(b, 0)
	b = binary.BigEndian.AppendUint16(b, MaxCols)
	b = binary.BigEndian.AppendUint16(b, MaxRows)
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, MaxRows)
	per := (maxBody - 8192) / MaxRows
	txt := strings.Repeat(unit, per/len(unit)-1)
	for y := range MaxRows {
		b = binary.BigEndian.AppendUint16(b, uint16(y))
		b = binary.BigEndian.AppendUint16(b, 0)
		b = binary.BigEndian.AppendUint16(b, MaxCols)
		b = binary.BigEndian.AppendUint16(b, 1)
		b = append(b, 0)
		b = binary.BigEndian.AppendUint16(b, uint16(len(txt)))
		b = append(b, txt...)
	}
	var z bytes.Buffer
	w, _ := flate.NewWriter(&z, flate.BestCompression)
	_, _ = w.Write(b)
	_ = w.Close()
	return z.Bytes()
}

// TestDecode_BreakHostileCost: a streamer (or, in a plaintext huddle, the
// server) can make every few chunks cost the viewer tens of milliseconds
// of CPU: a ~3 KiB update (7 chunks) inflates to 2 MiB of run text, all
// of which is sanitized and walked grapheme by grapheme (U+2028, Thai
// SARA AM and skin-tone modifiers survive sanitize; zero-width graphemes
// never fill a cell, so layText walks the whole run) although a row shows
// at most 400 cells. The span reader's cap (cells × maxCellBytes = 51 KB
// for a full row) doesn't bind: ~10 KB a row × 200 rows stays under
// maxBody. The server's screen bucket passes a burst of 96 chunks (~13
// such updates) and 48 a second after: most of a core on every viewer,
// for as long as the stream runs. Work per update should be bounded by
// the grid (e.g. a body cap of W×H×a few bytes, text cut to the span's
// cells before sanitizing), not by maxBody.
//
// Resolution: a run carries at most maxCellBytes (32) of text per cell,
// updates decode on a goroutine of their own (client/watch.go decodeLoop,
// at most 8 queued, the rest dropped), never on the receive loop that
// carries audio, and the worst case per update is bounded by the grid
// (400×200 cells × 32 B). What's left is CPU on a viewer who chose to watch
// a hostile stream (Esc ends it); this keeps that bound at ~100 ms an
// update.
func TestDecode_BreakHostileCost(t *testing.T) {
	if raceOn {
		t.Skip("timing under -race says nothing")
	}
	for _, unit := range []string{"\u2028", "\u0e33", "\U0001f3fb"} {
		upd := hostileKey(1, unit)
		chunks := (len(upd) + proto.ScreenChunkData - 1) / proto.ScreenChunkData
		burst := 96 / chunks
		var d Decoder
		start := time.Now()
		ok, err := d.Apply(upd)
		one := time.Since(start)
		t.Logf("%+q: %d bytes (%d chunks) applied=%v err=%v in %v; a 96-chunk burst is %d of them: ~%v",
			unit, len(upd), chunks, ok, err, one, burst, one*time.Duration(burst))
		if ok && one > 100*time.Millisecond {
			t.Errorf("%+q: one update costs the viewer %v of decoding", unit, one)
		}
	}
}
