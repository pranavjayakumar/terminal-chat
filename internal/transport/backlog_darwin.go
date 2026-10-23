package transport

import "golang.org/x/sys/unix"

// backlogLimit: bytes in the send buffer (SO_NWRITE counts data in flight
// too, so this is roomier than Linux's unsent-only count) beyond which
// voice is dropped.
const backlogLimit = 16 << 10

func unsentBytes(fd uintptr) (int, bool) {
	n, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_NWRITE)
	if err != nil {
		return 0, false
	}
	return n, true
}
