package transport

import (
	"context"
	"net"
	"net/netip"
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
		ln, cert := newServer(t, listenOpts{ListenOpts: o, noQUIC: true})
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

func TestPendingKey(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"192.0.2.1:1", "192.0.2.200:2"},
		{"[2001:db8::1]:1", "[2001:db8::ffff:1]:2"},
		{"[::ffff:192.0.2.1]:1", "192.0.2.9:2"},
	} {
		ka, kb := pendingKey(tcpAddr(tc.a)), pendingKey(tcpAddr(tc.b))
		if ka != kb || !ka.IsValid() {
			t.Errorf("%s and %s: keys %v, %v", tc.a, tc.b, ka, kb)
		}
	}
	if pendingKey(tcpAddr("192.0.2.1:1")) == pendingKey(tcpAddr("192.0.3.1:1")) {
		t.Error("different /24s share a key")
	}
}

func tcpAddr(s string) net.Addr {
	return net.TCPAddrFromAddrPort(netip.MustParseAddrPort(s))
}
