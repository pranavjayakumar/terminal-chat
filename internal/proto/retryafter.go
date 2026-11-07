package proto

import "time"

// CapRetryAfter (negotiated: in both HELLO and WELCOME, PROTOCOL.md §5, §9):
// a `rate_limited` RES error carries `retry_after_ms`, how long until the
// bucket that refused the request holds a token again, so a client draining
// a backlog can pace itself to the refill rate instead of guessing.
const CapRetryAfter = "retry_after"

// MaxRetryAfter bounds a retry_after_ms hint a client honours: a larger one
// (a confused or hostile server) is clamped, so the outbox never stalls.
const MaxRetryAfter = 30 * time.Second

// RateLimited is a rate_limited error hinting that the request may succeed
// after wait (rounded up to whole ms, at least 1).
func RateLimited(wait time.Duration, msg string) *Error {
	ms := (wait + time.Millisecond - 1) / time.Millisecond
	ms = max(1, min(ms, MaxRetryAfter/time.Millisecond))
	return &Error{Code: ErrRateLimited, Msg: msg, RetryAfterMs: uint32(ms)}
}

// RetryAfter is e's hint (0 when it has none), clamped to MaxRetryAfter.
func (e *Error) RetryAfter() time.Duration {
	return min(time.Duration(e.RetryAfterMs)*time.Millisecond, MaxRetryAfter)
}
