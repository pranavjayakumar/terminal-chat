package server

import (
	"crypto/rand"
	"slices"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// A rate_limited send carries retry_after_ms (cap retry_after) saying when
// the send bucket holds a token again; a session without the cap gets the
// plain error. Waiting exactly the hint is enough.
func TestRateLimitedRetryAfter(t *testing.T) {
	clk := newClock()
	ts := startServerCfg(t, "tcp", Config{Now: clk.Now})
	hint := ts.registerCaps(t, "alice", true, []string{proto.CapRetryAfter})
	if !slices.Contains(hint.welcome.Caps, proto.CapRetryAfter) {
		t.Fatalf("WELCOME caps %v lack %s", hint.welcome.Caps, proto.CapRetryAfter)
	}
	snap, _ := hint.syncFresh()
	gen := generalID(t, snap)
	plain := ts.register(t, "bob", false)
	plain.syncFresh()

	sendRes := func(c *tclient) proto.Res {
		nonce := make([]byte, 16)
		rand.Read(nonce)
		return c.req(proto.OpSend, proto.SendArgs{Chan: gen, Body: "x", Nonce: nonce})
	}
	for _, c := range []*tclient{hint, plain} {
		for i := range 30 { // the burst
			if r := sendRes(c); !r.OK {
				t.Fatalf("%s send %d: %v", c.user.Name, i, r.Err)
			}
		}
	}
	r := sendRes(hint)
	if r.OK || r.Err.Code != proto.ErrRateLimited || r.Err.RetryAfterMs == 0 || r.Err.RetryAfterMs > 334 {
		t.Fatalf("31st send = ok %v err %+v, want rate_limited with 1..334 ms", r.OK, r.Err)
	}
	if r := sendRes(plain); r.OK || r.Err.Code != proto.ErrRateLimited || r.Err.RetryAfterMs != 0 {
		t.Fatalf("without the cap: ok %v err %+v, want rate_limited without a hint", r.OK, r.Err)
	}

	clk.Advance(time.Duration(r.Err.RetryAfterMs)*time.Millisecond - time.Millisecond)
	if r := sendRes(hint); r.OK {
		t.Fatal("send before the hint ran out succeeded")
	}
	clk.Advance(time.Millisecond)
	if r := sendRes(hint); !r.OK {
		t.Fatalf("send after the hint: %v", r.Err)
	}

	// The other buckets hint too: read markers (burst 60, refill 5/s).
	for range 60 {
		hint.req(proto.OpRead, proto.ReadArgs{Chan: gen, Seq: 1})
	}
	if r := hint.req(proto.OpRead, proto.ReadArgs{Chan: gen, Seq: 1}); r.OK || r.Err.RetryAfterMs == 0 || r.Err.RetryAfterMs > 200 {
		t.Fatalf("61st read = ok %v err %+v, want rate_limited with 1..200 ms", r.OK, r.Err)
	}
}
