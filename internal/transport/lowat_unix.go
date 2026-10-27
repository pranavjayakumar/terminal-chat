//go:build linux || darwin

package transport

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// unsentLimit caps the bytes a TCP socket holds that the network hasn't
// taken yet (TCP_NOTSENT_LOWAT); a write blocks beyond it. Without it the
// kernel happily buffers seconds of output on a slow link, where the
// session's own queue can no longer put chat first or drop late voice
// (PROTOCOL.md §14.4). Data in flight isn't counted, so throughput is
// unaffected. Linux blocks writes at the mark (the tc server's side);
// macOS only delays writability, so there it merely helps.
const unsentLimit = 8 << 10

// limitUnsent is a net.Dialer / ListenConfig Control function.
func limitUnsent(_, _ string, c syscall.RawConn) error {
	// Best effort: an older kernel without the option still works.
	_ = c.Control(func(fd uintptr) {
		_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, unsentLimit)
	})
	return nil
}
