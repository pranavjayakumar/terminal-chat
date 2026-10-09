// Package transport provides tc's two interchangeable stream transports —
// QUIC (primary) and TLS-over-TCP (fallback) — behind one Conn interface,
// plus the server's self-signed certificate and SPKI pinning (PROTOCOL.md §2).
package transport

import (
	"context"
	"io"
	"net"
)

// Conn is one authenticated-ready, bidirectional frame stream.
type Conn interface {
	io.ReadWriter
	// Exporter returns the 32-byte TLS keying-material exporter for the
	// auth signature (label "EXPORTER-tc-auth", empty context).
	Exporter() ([]byte, error)
	// Kind is "quic" or "tcp".
	Kind() string
	RemoteAddr() net.Addr
	// CloseWithCode closes the connection; on QUIC the code string is sent
	// in CloseWithError (PROTOCOL.md §9). Safe to call more than once.
	CloseWithCode(code string) error
	Close() error
}

// Listener accepts connections on both QUIC and TCP.
type Listener interface {
	Accept(ctx context.Context) (Conn, error)
	// Addr is the bound address (same port for UDP and TCP).
	Addr() net.Addr
	Close() error
}

// ExporterLabel is the TLS exporter label for auth channel binding.
const ExporterLabel = "EXPORTER-tc-auth"
