package client

import (
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// A server answering every send with rate_limited and the smallest hint
// (1 ms, valid per §5) must not make the outbox resend in a tight loop: it
// stays paced at no faster than sendPace, like a hintless refusal or a paced
// backlog does.
func TestOutboxTinyRetryAfter_BreakSpin(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	c.timing.sendPace = 200 * time.Millisecond
	c.timing.retry = 400 * time.Millisecond
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))
	if err := c.Send(general, "hi", 0); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	n := 0
	for time.Since(start) < time.Second {
		var a proto.SendArgs
		r := fc.expectReq(proto.OpSend, &a)
		n++
		fc.write(proto.TRes, proto.Res{ID: r.ID, Err: &proto.Error{Code: proto.ErrRateLimited, Msg: "slow down", RetryAfterMs: 1}})
	}
	// One every 200 ms at most: ~6 in a second. A spin does hundreds.
	if n > 8 {
		t.Fatalf("%d sends in 1 s after 1 ms hints: the outbox spins instead of pacing at %v", n, c.timing.sendPace)
	}
}
