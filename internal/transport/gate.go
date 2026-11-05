package transport

import (
	"net"
	"net/netip"
	"sync"
)

// Connections that haven't authenticated yet (TLS handshake, then the wait
// for HELLO) are capped globally and per source prefix, so unauthenticated
// peers can't hold every goroutine, TLS state and file descriptor
// (PROTOCOL.md §4). A slot is taken before the TLS handshake (TCP) or when
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

// pendingKeys are the prefixes a pending connection from addr counts
// against, finest first: an IPv4 /24 (pre-auth slots are cheap to take from
// a neighbouring address, unlike rate-limit buckets), or an IPv6 /64, /56
// and /48, since one subscriber commonly gets a /48 or /56 and can dial from
// every /64 in it. The last one is the source's group for eviction.
func pendingKeys(addr net.Addr) []netip.Prefix {
	p := SourcePrefix(addr.String())
	a := p.Addr()
	switch {
	case a.Is4():
		p, _ = a.Prefix(24)
		return []netip.Prefix{p}
	case a.Is6():
		p56, _ := a.Prefix(56)
		p48, _ := a.Prefix(48)
		return []netip.Prefix{p, p56, p48}
	}
	return []netip.Prefix{p}
}

// pendingConn is one connection holding a slot.
type pendingConn struct {
	keys []netip.Prefix
	seq  uint64 // admission order: lower is older
	kill func() // closes the connection (eviction)
	gone bool   // released or evicted: no longer counted
}

func (pc *pendingConn) group() netip.Prefix { return pc.keys[len(pc.keys)-1] }

// gate counts pending connections. Each prefix level has its own cap:
// perSource for an IPv4 /24 or IPv6 /64, twice that for a /56, four times
// for a /48. When all max slots are taken, a newcomer doesn't wait behind
// them: the oldest pending connection of the group (IPv4 /24, IPv6 /48)
// holding the most slots is closed to make room, provided that group holds
// more than the newcomer's own. Flooding from many prefixes then only churns
// the flooder's own connections, and an honest client, whose group holds
// one or two slots, is the last to go.
type gate struct {
	max, perSource int
	keyOf          func(net.Addr) []netip.Prefix // tests replace it

	mu      sync.Mutex
	n       int
	seq     uint64
	by      map[netip.Prefix]int
	conns   map[*pendingConn]struct{}
	evicted int
}

func newGate(max, perSource int) *gate {
	if max <= 0 {
		max = DefaultMaxPending
	}
	if perSource <= 0 {
		perSource = DefaultMaxPendingPerSource
	}
	return &gate{max: max, perSource: perSource, keyOf: pendingKeys,
		by: map[netip.Prefix]int{}, conns: map[*pendingConn]struct{}{}}
}

// admit takes a slot for a connection from addr, evicting another pending
// connection if the gate is full (kill closes this one should it be evicted
// in turn; it must not block for long or call release synchronously while
// holding locks the caller needs). release is idempotent.
func (g *gate) admit(addr net.Addr, kill func()) (release func(), ok bool) {
	keys := g.keyOf(addr)
	g.mu.Lock()
	for i, k := range keys {
		if g.by[k] >= g.perSource<<i {
			g.mu.Unlock()
			return nil, false
		}
	}
	var victim *pendingConn
	if g.n >= g.max {
		if victim = g.victimLocked(keys[len(keys)-1]); victim == nil {
			g.mu.Unlock()
			return nil, false
		}
		g.dropLocked(victim)
		g.evicted++
	}
	pc := &pendingConn{keys: keys, seq: g.seq, kill: kill}
	g.seq++
	g.conns[pc] = struct{}{}
	g.n++
	for _, k := range keys {
		g.by[k]++
	}
	g.mu.Unlock()
	if victim != nil && victim.kill != nil {
		go victim.kill()
	}
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.dropLocked(pc)
	}, true
}

// victimLocked picks the oldest connection of the group holding the most
// slots, if that is more than group (the newcomer's) holds; else nil.
func (g *gate) victimLocked(group netip.Prefix) *pendingConn {
	var best *pendingConn
	bestN := g.by[group]
	for pc := range g.conns {
		n := g.by[pc.group()]
		if n > bestN || (best != nil && n == bestN && pc.seq < best.seq) {
			best, bestN = pc, n
		}
	}
	return best
}

func (g *gate) dropLocked(pc *pendingConn) {
	if pc.gone {
		return
	}
	pc.gone = true
	delete(g.conns, pc)
	g.n--
	for _, k := range pc.keys {
		if g.by[k]--; g.by[k] <= 0 {
			delete(g.by, k)
		}
	}
}

func (g *gate) pending() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

func (g *gate) evictions() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.evicted
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
