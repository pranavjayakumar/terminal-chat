package transport

import "net"

// Backlogged reports whether c's socket already holds more output than the
// network has taken, beyond what voice on the stream may wait behind
// (PROTOCOL.md §14.4). Voice checks it before every frame it writes: a
// frame written into a deep kernel buffer can't be dropped any more, it can
// only arrive late. Always false for QUIC (voice uses datagrams there) and
// where the OS can't tell.
func Backlogged(c Conn) bool {
	t, ok := c.(*tcpConn)
	if !ok {
		return false
	}
	tc, ok := t.NetConn().(*net.TCPConn)
	if !ok {
		return false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return false
	}
	var n int
	var known bool
	_ = rc.Control(func(fd uintptr) { n, known = unsentBytes(fd) })
	return known && n > backlogLimit
}
