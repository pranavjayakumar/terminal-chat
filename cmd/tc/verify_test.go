package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
)

// tc verify waits (bounded) while either side's key isn't pinned yet, and
// stops at once on any other error.
func TestPollSafety(t *testing.T) {
	ctx := context.Background()
	errs := []error{client.ErrNoKeys, client.ErrOwnKeyNotPinned, client.ErrNoKeys, nil}
	n := 0
	sn, err := pollSafety(ctx, func() (client.SafetyNumber, error) {
		e := errs[n]
		n++
		return client.SafetyNumber{Digits: "x"}, e
	}, time.Second, time.Millisecond)
	if err != nil || n != 4 || sn.Digits != "x" {
		t.Fatalf("polled %d times: %v", n, err)
	}

	other := errors.New("boom")
	n = 0
	if _, err := pollSafety(ctx, func() (client.SafetyNumber, error) { n++; return client.SafetyNumber{}, other }, time.Second, time.Millisecond); err != other || n != 1 {
		t.Fatalf("other error: %v after %d", err, n)
	}

	start := time.Now()
	if _, err := pollSafety(ctx, func() (client.SafetyNumber, error) { return client.SafetyNumber{}, client.ErrOwnKeyNotPinned }, 50*time.Millisecond, 5*time.Millisecond); !errors.Is(err, client.ErrOwnKeyNotPinned) || time.Since(start) > 2*time.Second {
		t.Fatalf("bounded: %v after %v", err, time.Since(start))
	}
}
