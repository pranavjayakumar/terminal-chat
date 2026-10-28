package server

import (
	"container/list"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skailama/tc/internal/transport"
)

// Unauthenticated HTTP traffic limits (PROTOCOL.md §12): a token bucket per
// source address, checked before a token touches the database, and a cap on
// open HTTP connections.
const (
	ipBurst      = 30
	ipPerSec     = 5
	ipMaxEntries = 10000
	// An idle bucket refills fully in burst/perSec = 6 s; after ipTTL it's
	// indistinguishable from a new one and can go.
	ipTTL = time.Minute

	maxHTTPConns          = 256
	maxHTTPConnsPerSource = 16
	httpIdleTimeout       = 10 * time.Second
)

// ipLimiter is a bounded map of per-address token buckets, evicting the
// least recently used entry when full and entries idle for ipTTL.
type ipLimiter struct {
	now           func() time.Time
	burst, perSec float64
	max           int

	mu  sync.Mutex
	lru *list.List // of *ipEntry, most recent first
	m   map[netip.Prefix]*list.Element
}

type ipEntry struct {
	key netip.Prefix
	b   bucket
}

func newIPLimiter(now func() time.Time, burst, perSec float64, max int) *ipLimiter {
	return &ipLimiter{now: now, burst: burst, perSec: perSec, max: max, lru: list.New(), m: map[netip.Prefix]*list.Element{}}
}

// ipKey groups IPv6 sources by /64, which one host typically controls.
func ipKey(remoteAddr string) netip.Prefix { return transport.SourcePrefix(remoteAddr) }

// allow takes a token for remoteAddr's bucket, or reports how long until one
// is available.
func (l *ipLimiter) allow(remoteAddr string) (bool, time.Duration) {
	key := ipKey(remoteAddr)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for back := l.lru.Back(); back != nil && now.Sub(back.Value.(*ipEntry).b.last) > ipTTL; back = l.lru.Back() {
		delete(l.m, back.Value.(*ipEntry).key)
		l.lru.Remove(back)
	}
	el, ok := l.m[key]
	if ok {
		l.lru.MoveToFront(el)
	} else {
		if l.lru.Len() >= l.max {
			back := l.lru.Back()
			delete(l.m, back.Value.(*ipEntry).key)
			l.lru.Remove(back)
		}
		el = l.lru.PushFront(&ipEntry{key: key, b: bucket{tokens: l.burst, last: now}})
		l.m[key] = el
	}
	b := &el.Value.(*ipEntry).b
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.perSec)
	b.last = now
	if b.tokens < 1 {
		return false, wait(b, l.perSec)
	}
	b.tokens--
	return true, 0
}

func (l *ipLimiter) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lru.Len()
}

// connCap is an http.Server ConnState hook that closes connections beyond
// max open ones, or beyond perSource from one source (same key as the rate
// limiter), instead of serving them, so slow clients can't pile up
// goroutines and TLS state and one source can't lock the others out. (A
// ConnState hook rather than a listener wrapper keeps the *tls.Conn visible
// to net/http.)
type connCap struct {
	max       int64
	perSource int64 // 0: maxHTTPConnsPerSource
	active    atomic.Int64

	mu    sync.Mutex
	conns map[net.Conn]netip.Prefix // admitted connections only
	by    map[netip.Prefix]int64
}

func (c *connCap) state(conn net.Conn, st http.ConnState) {
	switch st {
	case http.StateNew:
		key := ipKey(conn.RemoteAddr().String())
		per := c.perSource
		if per == 0 {
			per = maxHTTPConnsPerSource
		}
		c.mu.Lock()
		if c.active.Load() >= c.max || c.by[key] >= per {
			c.mu.Unlock()
			conn.Close() // net/http then reports StateClosed; it was never admitted
			return
		}
		if c.conns == nil {
			c.conns, c.by = map[net.Conn]netip.Prefix{}, map[netip.Prefix]int64{}
		}
		c.conns[conn] = key
		c.by[key]++
		c.active.Add(1)
		c.mu.Unlock()
	case http.StateClosed, http.StateHijacked:
		c.mu.Lock()
		if key, ok := c.conns[conn]; ok {
			delete(c.conns, conn)
			if c.by[key]--; c.by[key] <= 0 {
				delete(c.by, key)
			}
			c.active.Add(-1)
		}
		c.mu.Unlock()
	}
}
