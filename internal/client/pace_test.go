package client

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// After a rate_limited send the outbox waits out the server's retry_after_ms
// hint, then spaces the rest of the backlog by sendPace instead of tripping
// the limit again; without a hint it waits the plain retry first. Order and
// exactly-once hold throughout.
func TestOutboxPacesAfterRateLimit(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	c.timing.sendPace = 150 * time.Millisecond
	c.timing.retry = 400 * time.Millisecond
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))
	if h := fs.hello.Load().(proto.Hello); !slices.Contains(h.Caps, proto.CapRetryAfter) {
		t.Fatalf("HELLO caps %v lack %s", h.Caps, proto.CapRetryAfter)
	}

	const n = 6
	for i := range n {
		if err := c.Send(general, fmt.Sprint("m", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	next := uint64(40)
	ack := func(r proto.Req, a proto.SendArgs) {
		next++
		fc.event(next, proto.KMsgNew, general, alice, proto.Message{ID: next, Chan: general, Author: alice, Body: a.Body})
		fc.ok(r.ID, proto.SendRes{Msg: next})
	}
	recv := func(want string) (proto.Req, proto.SendArgs, time.Time) {
		t.Helper()
		var a proto.SendArgs
		r := fc.expectReq(proto.OpSend, &a)
		if a.Body != want {
			t.Fatalf("send = %q, want %q", a.Body, want)
		}
		return r, a, time.Now()
	}

	r, a, _ := recv("m0")
	ack(r, a)
	r, _, _ = recv("m1")
	refused := time.Now()
	fc.write(proto.TRes, proto.Res{ID: r.ID, Err: proto.RateLimited(300*time.Millisecond, "slow down")})
	r, a, at := recv("m1") // same message again, after the hint
	if d := at.Sub(refused); d < 280*time.Millisecond {
		t.Fatalf("retried after %v, want the 300 ms hint", d)
	}
	ack(r, a)
	prev := at
	for i := 2; i < 4; i++ {
		r, a, at := recv(fmt.Sprint("m", i))
		if d := at.Sub(prev); d < 120*time.Millisecond {
			t.Fatalf("m%d sent %v after the previous one, want paced at ~150 ms", i, d)
		}
		ack(r, a)
		prev = at
	}

	// No hint (an older server): the plain retry, then paced as before.
	r, _, _ = recv("m4")
	refused = time.Now()
	fc.fail(r.ID, proto.ErrRateLimited)
	r, a, at = recv("m4")
	if d := at.Sub(refused); d < 380*time.Millisecond {
		t.Fatalf("retried after %v without a hint, want the 400 ms retry", d)
	}
	ack(r, a)
	r, a, at2 := recv("m5")
	if d := at2.Sub(at); d < 120*time.Millisecond {
		t.Fatalf("m5 sent %v after m4, want paced", d)
	}
	ack(r, a)

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := bodies(c.Messages(general, 50)); got != "first,mine,m0,m1,m2,m3,m4,m5" {
		t.Fatalf("messages = %q", got)
	}
}

// A hostile or confused hint can't stall the outbox for longer than
// proto.MaxRetryAfter.
func TestRetryAfterClamped(t *testing.T) {
	e := &proto.Error{Code: proto.ErrRateLimited, RetryAfterMs: 1 << 31}
	if got := e.RetryAfter(); got != proto.MaxRetryAfter {
		t.Fatalf("RetryAfter = %v, want %v", got, proto.MaxRetryAfter)
	}
	if got := proto.RateLimited(time.Hour, "x").RetryAfterMs; got != uint32(proto.MaxRetryAfter/time.Millisecond) {
		t.Fatalf("RateLimited(1h) hint = %d", got)
	}
	if got := proto.RateLimited(0, "x").RetryAfterMs; got != 1 {
		t.Fatalf("RateLimited(0) hint = %d, want 1", got)
	}
}
