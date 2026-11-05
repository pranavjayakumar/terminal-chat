package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitPending(t *testing.T, ln Listener, n int) {
	t.Helper()
	g := ln.(*listener).gate
	for deadline := time.Now().Add(5 * time.Second); g.pending() != n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("pending = %d, want %d", g.pending(), n)
		}
	}
}

// Unauthenticated TCP connections are capped before any TLS work, per
// source and in total, and a slot frees when one closes or authenticates.
func TestPendingCapTCP(t *testing.T) {
	for _, o := range []ListenOpts{{MaxPendingPerSource: 3}, {MaxPending: 3, MaxPendingPerSource: 100}} {
		ln, cert := newServer(t, listenOpts{ListenOpts: o, noQUIC: true, firstByteTimeout: time.Minute})
		addr, pin := ln.Addr().String(), Pin(cert)
		var raws []net.Conn
		for range 3 { // TCP up, no ClientHello: holds a slot
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			raws = append(raws, c)
		}
		waitPending(t, ln, 3)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if c, err := DialTCP(ctx, addr, pin); err == nil {
			c.Close()
			t.Fatalf("%+v: a 4th unauthenticated connection was accepted", o)
		}
		cancel()

		raws[0].Close()
		waitPending(t, ln, 2)
		c, err := DialTCP(testCtx(t), addr, pin)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		srv, err := ln.Accept(testCtx(t))
		if err != nil {
			t.Fatal(err)
		}
		waitPending(t, ln, 3)
		Authenticated(srv)
		Authenticated(srv) // idempotent
		waitPending(t, ln, 2)
		srv.Close()
		waitPending(t, ln, 2)
	}
}

// QUIC connections count from Accept until they authenticate; beyond the
// cap they are closed without ever reaching Accept.
func TestPendingCapQUIC(t *testing.T) {
	ln, cert := newServer(t, listenOpts{ListenOpts: ListenOpts{MaxPendingPerSource: 2}})
	addr, pin := ln.Addr().String(), Pin(cert)
	for range 2 {
		c, err := DialQUIC(testCtx(t), addr, pin) // never writes: its stream stays unseen
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
	}
	waitPending(t, ln, 2)
	c, err := DialQUIC(testCtx(t), addr, pin)
	if err == nil {
		t.Cleanup(func() { c.Close() })
		c.Write([]byte{0})
		accepted := make(chan Conn, 1)
		go func() {
			if s, err := ln.Accept(testCtx(t)); err == nil {
				accepted <- s
			}
		}()
		select {
		case s := <-accepted:
			s.Close()
			t.Fatal("a 3rd unauthenticated QUIC connection was accepted")
		case <-time.After(time.Second):
		}
	}
	waitPending(t, ln, 2)
}

func TestPendingKeys(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"192.0.2.1:1", "192.0.2.200:2"},
		{"[2001:db8::1]:1", "[2001:db8::ffff:1]:2"},
		{"[::ffff:192.0.2.1]:1", "192.0.2.9:2"},
	} {
		ka, kb := pendingKeys(tcpAddr(tc.a)), pendingKeys(tcpAddr(tc.b))
		if !slices.Equal(ka, kb) || !ka[0].IsValid() {
			t.Errorf("%s and %s: keys %v, %v", tc.a, tc.b, ka, kb)
		}
	}
	if pendingKeys(tcpAddr("192.0.2.1:1"))[0] == pendingKeys(tcpAddr("192.0.3.1:1"))[0] {
		t.Error("different /24s share a key")
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("2001:db8:1:2::/64"),
		netip.MustParsePrefix("2001:db8:1::/56"),
		netip.MustParsePrefix("2001:db8:1::/48"),
	}
	if got := pendingKeys(tcpAddr("[2001:db8:1:2::9]:1")); !slices.Equal(got, want) {
		t.Errorf("IPv6 keys %v, want %v", got, want)
	}
}

// fakeConn is a pending connection in a gate-only test.
type fakeConn struct {
	addr    net.Addr
	release func()
	killed  atomic.Bool
}

func admitFake(g *gate, addr string) (*fakeConn, bool) {
	c := &fakeConn{addr: tcpAddr(addr)}
	var ok bool
	c.release, ok = g.admit(c.addr, func() { c.killed.Store(true); c.release() })
	return c, ok
}

// waitKilled waits for an eviction's kill (it runs on its own goroutine).
func waitKilled(t *testing.T, c *fakeConn) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !c.killed.Load(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%v was not closed", c.addr)
		}
	}
}

// One IPv6 subscriber prefix can't take more than 4× the per-source cap
// (/48) or 2× (/56), however many /64s it dials from.
func TestGateIPv6Hierarchy(t *testing.T) {
	for _, tc := range []struct {
		addr func(i int) string
		want int
	}{
		// 2001:db8:1:0::/64 … :ff::/64 are one /56.
		{func(i int) string { return fmt.Sprintf("[2001:db8:1:%x::1]:1", i%256) }, 2 * 32},
		// :0::/64 … :ff00::/64: one /48, a different /56 each.
		{func(i int) string { return fmt.Sprintf("[2001:db8:1:%x::1]:1", (i%256)<<8) }, 4 * 32},
	} {
		g := newGate(10000, 32)
		var held []*fakeConn
		for i := range 256 * 40 {
			if c, ok := admitFake(g, tc.addr(i)); ok {
				held = append(held, c)
			}
		}
		if g.pending() != tc.want {
			t.Errorf("%s…: %d slots, want %d", tc.addr(0), g.pending(), tc.want)
		}
	}
}

// One host filling its /48 from a few /64s (32 each) doesn't lock out the
// other subscribers there: a newcomer from a quiet /64 evicts the flooder's
// oldest, and the flooder's re-dials don't evict the newcomer.
func TestGateSharedIPv6Prefix(t *testing.T) {
	g := newGate(10000, 32)
	flood := func(i int) string { return fmt.Sprintf("[2001:db8:1:%x::1]:1", (i%4)<<8) } // 4 /56s, a /64 each
	var held []*fakeConn
	for i := range 4 * 32 {
		c, ok := admitFake(g, flood(i))
		if !ok {
			t.Fatal("refused while filling")
		}
		held = append(held, c)
	}
	honest, ok := admitFake(g, "[2001:db8:1:ff01::7]:1")
	if !ok {
		t.Fatal("a neighbour in the same /48 was locked out")
	}
	waitKilled(t, held[0])
	for i := range 1000 {
		admitFake(g, flood(i))
	}
	time.Sleep(50 * time.Millisecond)
	if honest.killed.Load() {
		t.Fatal("the flood evicted the neighbour")
	}
	if g.pending() != 4*32 {
		t.Fatalf("pending = %d", g.pending())
	}
}

// The attack: a flooder with addresses in many prefixes (16 IPv4 /24s × 32,
// or 4 IPv6 /48s × 128) takes every pending slot and keeps re-dialling. A
// fresh client still gets in, by evicting the flooder's oldest, and the
// flooder's re-dials only ever evict the flooder.
func TestGateFloodCantBlockFreshClient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		flood  func(i int) string // i-th flood address
		honest string
	}{
		{"ipv4", func(i int) string { return fmt.Sprintf("10.0.%d.%d:1", i%16, i/16%256) }, "192.0.2.7:1"},
		{"ipv6", func(i int) string { return fmt.Sprintf("[2001:db8:%x:%x::1]:1", i%4, (i/4%4)<<8|(i/16%4)) }, "[2001:db8:ffff::1]:1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGate(DefaultMaxPending, DefaultMaxPendingPerSource)
			var flood []*fakeConn
			for i := 0; g.pending() < DefaultMaxPending; i++ {
				if i > 10*DefaultMaxPending {
					t.Fatalf("flood stuck at %d slots", g.pending())
				}
				if c, ok := admitFake(g, tc.flood(i)); ok {
					flood = append(flood, c)
				}
			}
			if g.evictions() != 0 {
				t.Fatalf("filling up evicted %d", g.evictions())
			}
			// Refused before the fix: every slot is taken.
			honest, ok := admitFake(g, tc.honest)
			if !ok {
				t.Fatal("a fresh client was refused while the flood holds every slot")
			}
			waitKilled(t, flood[0]) // the oldest of a heaviest group
			// The flooder re-dials hard, from all its prefixes.
			for i := range 20 * DefaultMaxPending {
				admitFake(g, tc.flood(i))
			}
			if g.evictions() < 2 {
				t.Fatalf("re-dials evicted %d", g.evictions())
			}
			if honest.killed.Load() {
				t.Fatal("the flood evicted the fresh client")
			}
			if g.pending() != DefaultMaxPending {
				t.Fatalf("pending = %d", g.pending())
			}
			// A second fresh client from yet another prefix gets in too.
			if _, ok := admitFake(g, "198.51.100.1:1"); !ok {
				t.Fatal("a second fresh client was refused")
			}
			time.Sleep(50 * time.Millisecond)
			if honest.killed.Load() {
				t.Fatal("the second client evicted the first")
			}
		})
	}
}

// One address per prefix (a botnet): every group holds one slot, so a
// newcomer evicts the oldest connection overall, and an honest client
// keeps its slot until max-1 newer dials have come in after it.
func TestGateManySingleSources(t *testing.T) {
	const max = 64
	g := newGate(max, 32)
	addr := func(i int) string { return fmt.Sprintf("10.%d.%d.1:1", i/256, i%256) }
	var flood []*fakeConn
	for i := range max {
		c, _ := admitFake(g, addr(i))
		flood = append(flood, c)
	}
	honest, ok := admitFake(g, "192.0.2.7:1")
	if !ok {
		t.Fatal("refused")
	}
	waitKilled(t, flood[0])
	for i := range max - 1 {
		if _, ok := admitFake(g, addr(max+i)); !ok {
			t.Fatal("refused")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if honest.killed.Load() {
		t.Fatal("evicted before the older connections")
	}
}

// Honest behaviour is unchanged: below the global cap nothing is evicted,
// per-prefix caps still refuse, and when the gate is full a newcomer from
// a group already holding as many slots as any is refused rather than
// evicting someone else's connection.
func TestGateNoEvictionForHeaviest(t *testing.T) {
	g := newGate(4, 3)
	a1, _ := admitFake(g, "192.0.2.1:1")
	a2, _ := admitFake(g, "192.0.2.2:1")
	b1, _ := admitFake(g, "198.51.100.1:1")
	b2, _ := admitFake(g, "198.51.100.2:1")
	if g.evictions() != 0 || g.pending() != 4 {
		t.Fatal("evicted below the cap")
	}
	if _, ok := admitFake(g, "192.0.2.3:1"); ok {
		t.Fatal("a newcomer from a group as heavy as any evicted another")
	}
	a2.release()
	a2.release() // idempotent
	if g.pending() != 3 {
		t.Fatalf("pending = %d", g.pending())
	}
	if _, ok := admitFake(g, "192.0.2.3:1"); !ok {
		t.Fatal("refused with a free slot")
	}
	if _, ok := admitFake(g, "192.0.2.4:1"); ok {
		t.Fatal("full gate, own group heaviest: admitted")
	}
	time.Sleep(50 * time.Millisecond)
	for _, c := range []*fakeConn{a1, b1, b2} {
		if c.killed.Load() {
			t.Fatal("evicted")
		}
	}
	g = newGate(100, 3)
	for range 3 {
		admitFake(g, "192.0.2.1:1")
	}
	if _, ok := admitFake(g, "192.0.2.9:1"); ok {
		t.Fatal("per-source cap not enforced")
	}
}

// Over real sockets: with the gate full of silent TCP connections from
// other prefixes, a real client still completes its TLS handshake, and the
// oldest flooding connection is closed.
func TestPendingEvictTCP(t *testing.T) {
	var mu sync.Mutex
	seen := 0
	keyOf := func(net.Addr) []netip.Prefix { // everyone dials from 127.0.0.1
		mu.Lock()
		defer mu.Unlock()
		i := seen
		seen++
		if i < 4 { // the flood: two per /24
			return []netip.Prefix{netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 0, byte(i / 2), 0}), 24)}
		}
		return []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	}
	ln, cert := newServer(t, listenOpts{ListenOpts: ListenOpts{MaxPending: 4, MaxPendingPerSource: 2},
		noQUIC: true, firstByteTimeout: time.Minute, keyOf: keyOf})
	addr, pin := ln.Addr().String(), Pin(cert)
	var raws []net.Conn
	for i := range 4 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		raws = append(raws, c)
		waitPending(t, ln, i+1)
	}
	c, err := DialTCP(testCtx(t), addr, pin)
	if err != nil {
		t.Fatalf("a fresh client was refused while the flood holds every slot: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := ln.Accept(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	raws[0].SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := raws[0].Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the oldest flooding connection is still open: %v", err)
	}
	waitPending(t, ln, 4)
}

// A TCP connection that sends nothing is closed after the short first-byte
// timeout, not the handshake timeout; one that handshakes isn't, and its
// session reads (and sees its socket backlog) as usual afterwards.
func TestPendingSilentTCP(t *testing.T) {
	ln, cert := newServer(t, listenOpts{noQUIC: true, firstByteTimeout: 300 * time.Millisecond, handshakeTimeout: time.Minute})
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	waitPending(t, ln, 1)
	waitPending(t, ln, 0) // well before the 1-minute handshake timeout
	c, err := DialTCP(testCtx(t), ln.Addr().String(), Pin(cert))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := ln.Accept(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	time.Sleep(500 * time.Millisecond)
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(s, make([]byte, 1)); err != nil {
		t.Fatalf("read after the first-byte timeout: %v", err)
	}
	if _, known := Unsent(s); !known && (runtime.GOOS == "linux" || runtime.GOOS == "darwin") {
		t.Fatal("Unsent can't see the socket under the first-byte wrapper")
	}
}

func tcpAddr(s string) net.Addr {
	return net.TCPAddrFromAddrPort(netip.MustParseAddrPort(s))
}
