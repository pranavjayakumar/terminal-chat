package transport

import (
	"net"
	"net/netip"
	"sync"
)

// Connections that haven't authenticated yet (TLS handshake, then the wait
// for HELLO) are capped globally and per source prefix, so unauthenticated
// peers can't hold every goroutine, TLS state and file descriptor
// (PROTOCOL.md §9). A slot is taken before the TLS handshake (TCP) or when
// quic-go hands over a connection, and freed by Authenticated or Close.
const (
	DefaultMaxPending          = 512
	DefaultMaxPendingPerSource = 32
)

// SourcePrefix groups remoteAddr (host:port or host) by the prefix one
// host typically controls: the address itself for IPv4, its /64 for IPv6.
// Invalid input yields the zero prefix, one shared group.
func SourcePrefix(remoteAddr string) netip.Prefix {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Prefix{}
	}
	a = a.Unmap()
	bits := 32
	if a.Is6() {
		bits = 64
	}
	p, _ := a.Prefix(bits)
	return p
}

// pendingKey is SourcePrefix widened to /24 for IPv4: pre-auth slots are
// cheap to take from a neighbouring address, unlike rate-limit buckets.
func pendingKey(addr net.Addr) netip.Prefix {
	p := SourcePrefix(addr.String())
	if p.Addr().Is4() {
		p, _ = p.Addr().Prefix(24)
	}
	return p
}

type gate struct {
	max, perSource int

	mu sync.Mutex
	n  int
	by map[netip.Prefix]int
}

func newGate(max, perSource int) *gate {
	if max <= 0 {
		max = DefaultMaxPending
	}
	if perSource <= 0 {
		perSource = DefaultMaxPendingPerSource
	}
	return &gate{max: max, perSource: perSource, by: map[netip.Prefix]int{}}
}

// admit takes a slot for a connection from addr. release is idempotent.
func (g *gate) admit(addr net.Addr) (release func(), ok bool) {
	key := pendingKey(addr)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n >= g.max || g.by[key] >= g.perSource {
		return nil, false
	}
	g.n++
	g.by[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.n--
			if g.by[key]--; g.by[key] <= 0 {
				delete(g.by, key)
			}
		})
	}, true
}

func (g *gate) pending() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

// Authenticated tells the listener that c finished the handshake (HELLO
// verified): it no longer counts as pending. Safe to call more than once,
// and for connections from Dial (a no-op).
func Authenticated(c Conn) {
	switch c := c.(type) {
	case *tcpConn:
		c.releasePending()
	case *quicConn:
		c.releasePending()
	}
}
