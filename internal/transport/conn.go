package transport

import (
	"context"
	"crypto/tls"
	"net"
	"sync"

	"github.com/quic-go/quic-go"
)

type quicConn struct {
	*quic.Stream
	conn    *quic.Conn
	once    sync.Once
	err     error
	release func() // the listener's pending slot (gate.go), nil when dialed
}

func (c *quicConn) releasePending() {
	if c.release != nil {
		c.release()
	}
}

func (c *quicConn) Kind() string         { return "quic" }
func (c *quicConn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }
func (c *quicConn) Close() error         { return c.CloseWithCode("") }

func (c *quicConn) Exporter() ([]byte, error) {
	cs := c.conn.ConnectionState().TLS
	return cs.ExportKeyingMaterial(ExporterLabel, nil, 32)
}

func (c *quicConn) SendDatagram(b []byte) error { return c.conn.SendDatagram(b) }

func (c *quicConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	return c.conn.ReceiveDatagram(ctx)
}

func (c *quicConn) datagramsOK() bool {
	return c.conn.ConnectionState().SupportsDatagrams.Remote
}

func (c *quicConn) CloseWithCode(code string) error {
	c.once.Do(func() {
		c.err = c.conn.CloseWithError(0, code)
		c.releasePending()
	})
	return c.err
}

type tcpConn struct {
	*tls.Conn
	once    sync.Once
	err     error
	release func() // the listener's pending slot (gate.go), nil when dialed
}

func (c *tcpConn) releasePending() {
	if c.release != nil {
		c.release()
	}
}

func (c *tcpConn) Kind() string { return "tcp" }

func (c *tcpConn) Exporter() ([]byte, error) {
	cs := c.Conn.ConnectionState()
	return cs.ExportKeyingMaterial(ExporterLabel, nil, 32)
}

// TLS-over-TCP has no close reason; the code travels only in the BYE frame.
func (c *tcpConn) CloseWithCode(string) error { return c.Close() }

func (c *tcpConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.releasePending()
	})
	return c.err
}
