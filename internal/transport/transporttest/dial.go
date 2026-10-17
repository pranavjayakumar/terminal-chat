// Package transporttest dials test servers over loopback reliably.
package transporttest

import (
	"context"
	"testing"
	"time"

	"github.com/skailama/tc/internal/transport"
)

// DialFunc is transport.DialQUIC, transport.DialTCP or transport.Dial.
type DialFunc func(ctx context.Context, addr, pin string) (transport.Conn, error)

const (
	attemptTimeout = 10 * time.Second
	totalTimeout   = 30 * time.Second
)

// Dial connects with dial or fails t, retrying with a fresh socket.
//
// A loopback QUIC dial can hang until the 5 s handshake idle timeout on
// macOS: the client's UDP socket binds 0.0.0.0 with an ephemeral port, and
// the kernel may hand out a port that another socket holds on 127.0.0.1 (any
// test server's QUIC listener, in this process or a parallel package). The
// more specific socket then receives the server's replies. A retry gets a new
// port. Slow -race runners also need more than a few seconds per handshake.
// Tests that assert a dial fails call the dialer directly.
func Dial(t testing.TB, dial DialFunc, addr, pin string) transport.Conn {
	t.Helper()
	deadline := time.Now().Add(totalTimeout)
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), min(attemptTimeout, time.Until(deadline)))
		c, err := dial(ctx, addr, pin)
		cancel()
		if err == nil {
			return c
		}
		if time.Until(deadline) < 100*time.Millisecond {
			t.Fatalf("dial %s: %d attempts in %v: %v", addr, attempt, totalTimeout, err)
		}
		t.Logf("dial %s attempt %d: %v (retrying)", addr, attempt, err)
		time.Sleep(50 * time.Millisecond)
	}
}
