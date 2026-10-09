package transport

import (
	"crypto/tls"
	"net"
	"sync"

	"github.com/quic-go/quic-go"
)

type quicConn struct {
	*quic.Stream
	conn *quic.Conn
	once sync.Once
	err  error
}

func (c *quicConn) Kind() string         { return "quic" }
func (c *quicConn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }
func (c *quicConn) Close() error         { return c.CloseWithCode("") }

func (c *quicConn) Exporter() ([]byte, error) {
	cs := c.conn.ConnectionState().TLS
	return cs.ExportKeyingMaterial(ExporterLabel, nil, 32)
}

func (c *quicConn) CloseWithCode(code string) error {
	c.once.Do(func() { c.err = c.conn.CloseWithError(0, code) })
	return c.err
}

type tcpConn struct {
	*tls.Conn
	once sync.Once
	err  error
}

func (c *tcpConn) Kind() string { return "tcp" }

func (c *tcpConn) Exporter() ([]byte, error) {
	cs := c.Conn.ConnectionState()
	return cs.ExportKeyingMaterial(ExporterLabel, nil, 32)
}

// TLS-over-TCP has no close reason; the code travels only in the BYE frame.
func (c *tcpConn) CloseWithCode(string) error { return c.Close() }

func (c *tcpConn) Close() error {
	c.once.Do(func() { c.err = c.Conn.Close() })
	return c.err
}
