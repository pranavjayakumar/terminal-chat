package transport

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"slices"
	"sync"
)

// ALPNHTTP is the ALPN protocol for webhook requests (PROTOCOL.md §12).
const ALPNHTTP = "http/1.1"

// ListenOpts configures ListenWith.
type ListenOpts struct {
	// HTTP also accepts ALPN http/1.1 (or no ALPN) on the TLS-over-TCP
	// listener. Those connections are not returned by Accept but by
	// HTTPListener, for a net/http server. QUIC stays tc/1 only.
	HTTP bool
	// HTTPCert is presented to HTTP clients instead of the tc certificate
	// (see LoadOrCreateHTTPCert). Zero means the tc certificate.
	HTTPCert tls.Certificate
}

// tcpConfig returns the TLS-over-TCP server config. With HTTP enabled, a
// ClientHello offering tc/1 gets the tc config (tc/1 only); anything else
// gets the HTTP config (http/1.1 only, HTTP certificate), so a client
// offering neither fails the handshake and a client offering no ALPN gets
// HTTP.
func tcpConfig(tc *tls.Config, o ListenOpts) *tls.Config {
	if !o.HTTP {
		return tc
	}
	hc := tc.Clone()
	hc.NextProtos = []string{ALPNHTTP}
	if len(o.HTTPCert.Certificate) > 0 {
		hc.Certificates = []tls.Certificate{o.HTTPCert}
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			if slices.Contains(hello.SupportedProtos, tc.NextProtos[0]) {
				return tc, nil
			}
			return hc, nil
		},
	}
}

// HTTPListener returns the listener of HTTPS connections of a Listener
// created with ListenOpts{HTTP: true}, or nil. Its connections are
// *tls.Conn with the handshake done. Closing it doesn't close ln.
func HTTPListener(ln Listener) net.Listener {
	if l, ok := ln.(*listener); ok && l.http != nil {
		return l.http
	}
	return nil
}

// connListener is an in-memory net.Listener fed by the TCP accept loop.
type connListener struct {
	addr      net.Addr
	conns     chan net.Conn
	parent    <-chan struct{} // the transport listener's shutdown
	done      chan struct{}
	closeOnce sync.Once
}

func newConnListener(addr net.Addr, parent <-chan struct{}) *connListener {
	return &connListener{addr: addr, conns: make(chan net.Conn), parent: parent, done: make(chan struct{})}
}

// deliver hands c to Accept, or closes it if either listener closes first.
func (l *connListener) deliver(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.done:
		c.Close()
	case <-l.parent:
		c.Close()
	}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
	case <-l.parent:
	}
	return nil, net.ErrClosed
}

func (l *connListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

func (l *connListener) Addr() net.Addr { return l.addr }

// CurlPin renders a pin (base64url SPKI SHA-256) in the form curl's
// --pinnedpubkey expects: "sha256//" + standard padded base64.
func CurlPin(pin string) (string, error) {
	sum, err := base64.RawURLEncoding.DecodeString(pin)
	if err != nil || len(sum) != 32 {
		return "", errors.New("invalid pin")
	}
	return "sha256//" + base64.StdEncoding.EncodeToString(sum), nil
}

// HTTPSConfig is a client TLS config for the webhook endpoint that trusts
// exactly the pinned server key (for Go HTTP clients and tests).
func HTTPSConfig(pin string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{ALPNHTTP},
		InsecureSkipVerify: true,
		VerifyConnection:   verifyPin(pin),
	}
}
