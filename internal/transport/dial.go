package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/skailama/tc/internal/proto"
)

// quicHeadStart is how long Dial waits for QUIC before also trying TCP.
const quicHeadStart = 300 * time.Millisecond

func clientTLS(pin string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{proto.ALPN},
		// Identity is the pinned SPKI, not a CA chain or hostname (PROTOCOL.md §2).
		InsecureSkipVerify: true,
		VerifyConnection:   verifyPin(pin),
	}
}

// Dial connects to addr, racing QUIC against TLS-over-TCP: QUIC starts at
// once, TCP after a 300 ms head start (or as soon as QUIC fails). The first to
// finish wins and the other is cancelled. TC_TRANSPORT=quic|tcp forces one.
func Dial(ctx context.Context, addr, pin string) (Conn, error) {
	switch os.Getenv("TC_TRANSPORT") {
	case "quic":
		return DialQUIC(ctx, addr, pin)
	case "tcp":
		return DialTCP(ctx, addr, pin)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		c   Conn
		err error
	}
	results := make(chan result, 2)
	quicFailed := make(chan struct{})
	go func() {
		c, err := DialQUIC(ctx, addr, pin)
		if err != nil {
			close(quicFailed)
		}
		results <- result{c, err}
	}()
	go func() {
		t := time.NewTimer(quicHeadStart)
		defer t.Stop()
		select {
		case <-t.C:
		case <-quicFailed:
		case <-ctx.Done():
			results <- result{nil, ctx.Err()}
			return
		}
		c, err := DialTCP(ctx, addr, pin)
		results <- result{c, err}
	}()

	var errs []error
	for i := range 2 {
		r := <-results
		if r.err == nil {
			if i == 0 {
				// The loser may still complete before noticing cancellation.
				go func() {
					if l := <-results; l.c != nil {
						l.c.Close()
					}
				}()
			}
			return r.c, nil
		}
		errs = append(errs, r.err)
	}
	return nil, errors.Join(errs...)
}

// DialQUIC connects over QUIC and opens the single bidirectional frame stream.
// The server only learns of the stream once data is written on it, which is
// fine because the client always sends HELLO first.
func DialQUIC(ctx context.Context, addr, pin string) (Conn, error) {
	qc, err := quic.DialAddr(ctx, addr, clientTLS(pin), quicConfig())
	if err != nil {
		return nil, fmt.Errorf("quic dial %s: %w", addr, err)
	}
	st, err := qc.OpenStreamSync(ctx)
	if err != nil {
		qc.CloseWithError(0, "")
		return nil, fmt.Errorf("quic open stream: %w", err)
	}
	return &quicConn{Stream: st, conn: qc}, nil
}

// DialTCP connects over TLS-over-TCP.
func DialTCP(ctx context.Context, addr, pin string) (Conn, error) {
	d := tls.Dialer{Config: clientTLS(pin), NetDialer: &net.Dialer{Control: limitUnsent}}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tcp dial %s: %w", addr, err)
	}
	tc := nc.(*tls.Conn)
	if tc.ConnectionState().NegotiatedProtocol != proto.ALPN {
		tc.Close()
		return nil, fmt.Errorf("tcp dial %s: server did not negotiate ALPN %s", addr, proto.ALPN)
	}
	return &tcpConn{Conn: tc}, nil
}
