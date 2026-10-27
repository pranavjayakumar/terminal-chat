package transport

import "net"

// Backlogged reports whether c's socket already holds more output than the
// network has taken, beyond what voice on the stream may wait behind
// (PROTOCOL.md §14.4). Voice checks it before every frame it writes: a
// frame written into a deep kernel buffer can't be dropped any more, it can
// only arrive late. Always false for QUIC (voice uses datagrams there) and
// where the OS can't tell.
func Backlogged(c Conn) bool {
	n, known := Unsent(c)
	return known && n > backlogLimit
}

// Unsent is how many bytes c's socket holds that the network hasn't taken
// (Linux: not yet sent; macOS: not yet acknowledged), and whether the OS
// could tell (never for QUIC).
func Unsent(c Conn) (n int, known bool) {
	t, ok := c.(*tcpConn)
	if !ok {
		return 0, false
	}
	tc, ok := t.NetConn().(*net.TCPConn)
	if !ok {
		return 0, false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return 0, false
	}
	_ = rc.Control(func(fd uintptr) { n, known = unsentBytes(fd) })
	return n, known
}
