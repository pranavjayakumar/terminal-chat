package transport

import "golang.org/x/sys/unix"

// backlogLimit: unsent bytes (tcpi_notsent_bytes; data in flight isn't
// counted) beyond which voice is dropped: ~0.5 s of one talker.
const backlogLimit = 4 << 10

func unsentBytes(fd uintptr) (int, bool) {
	ti, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		return 0, false
	}
	return int(ti.Notsent_bytes), true
}
