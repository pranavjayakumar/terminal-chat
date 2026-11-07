package screen

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// TestReassemble_BreakSlowBigUpdate: the streamer paces chunks with its
// own bucket (client/watch.go: burst 80, 40 a second), so a large update
// sent right after another one drained the bucket arrives over
// count/40 s: 255 chunks take 6.4 s. The reassembler forgets a partial
// update partialTTL (5 s) after its first chunk, so such an update can
// never complete: a big busy pane (each picture a new keyframe of over
// 200 chunks) never gets past its first picture, and a viewer who starts
// watching just after a big keyframe waits for the next full-bucket one.
// The TTL must cover what the sender's pacing allows for MaxScreenChunks
// (or be measured from the latest chunk, not the first).
func TestReassemble_BreakSlowBigUpdate(t *testing.T) {
	const senderPerSec = 40 // client.sharePerSec
	upd := make([]byte, proto.MaxScreenChunks*proto.ScreenChunkData)
	_, _ = rand.Read(upd)
	chunks, err := Chunks(7, upd)
	if err != nil {
		t.Fatal(err)
	}
	var r Reassembler
	t0 := time.Unix(1_700_000_000, 0)
	for i, c := range chunks {
		at := t0.Add(time.Duration(i) * time.Second / senderPerSec)
		if got, ok := r.Add(c, at); ok {
			if len(got) != len(upd) {
				t.Fatalf("reassembled %d bytes, want %d", len(got), len(upd))
			}
			return
		}
	}
	t.Fatalf("a %d-chunk update sent at the streamer's own rate (%d/s, %.1f s) never reassembled (partialTTL %v)",
		len(chunks), senderPerSec, float64(len(chunks))/senderPerSec, partialTTL)
}
