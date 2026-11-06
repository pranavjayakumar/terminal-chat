package client

import (
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// QA v0.6.4: the TUI marks the open channel read on every message that
// arrives, and each mark went to the server at once: in a busy channel that
// is several read REQs (and read.update events) per second per reader,
// enough to hit the server's rate limit and drop the latest marker. Marks are
// coalesced per channel; the newest always goes out.
func TestMarkReadCoalesced(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	for seq := uint64(20); seq < 120; seq++ {
		if err := c.MarkRead(general, seq); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	var reads []uint64
	deadline := time.After(2500 * time.Millisecond)
loop:
	for {
		select {
		case f := <-fc.in:
			var r proto.Req
			if f.Type != proto.TReq || f.Decode(&r) != nil || r.Op != proto.OpRead {
				continue
			}
			var a proto.ReadArgs
			proto.Unmarshal(r.Args, &a)
			reads = append(reads, a.Seq)
		case <-deadline:
			break loop
		}
	}
	if len(reads) == 0 || reads[len(reads)-1] != 119 {
		t.Fatalf("read markers sent %v: the newest (119) must go out", reads)
	}
	if len(reads) > 3 {
		t.Fatalf("%d read REQs for 100 marks within ~0.6 s: %v", len(reads), reads)
	}
}
