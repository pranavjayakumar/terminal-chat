package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/skailama/tc/internal/proto"
)

const (
	handshakeTimeout = 10 * time.Second
	keepAlivePeriod  = 20 * time.Second
	maxIdleTimeout   = 60 * time.Second
)

// ErrListenerClosed is returned by Accept after Close.
var ErrListenerClosed = errors.New("listener closed")

func quicConfig() *quic.Config {
	return &quic.Config{
		// quic-go aborts a handshake after twice this, so the total is handshakeTimeout.
		HandshakeIdleTimeout: handshakeTimeout / 2,
		MaxIdleTimeout:       maxIdleTimeout,
		KeepAlivePeriod:      keepAlivePeriod,
		Allow0RTT:            false,
		// Voice (PROTOCOL.md §14.3). A transport parameter: peers without
		// it just never get datagrams.
		EnableDatagrams: true,
	}
}

type listenOpts struct {
	ListenOpts
	noQUIC           bool // tests: simulate blocked UDP
	handshakeTimeout time.Duration
}

type listener struct {
	tcp     net.Listener
	quic    *quic.Listener // nil if disabled
	tlsConf *tls.Config
	timeout time.Duration
	http    *connListener // nil unless ListenOpts.HTTP

	conns     chan Conn
	ctx       context.Context // cancelled on Close
	cancel    context.CancelFunc
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// Listen binds QUIC (UDP) and TLS-over-TCP on the same port. With port 0 the
// TCP port is chosen first and UDP binds the same number.
func Listen(addr string, cert tls.Certificate) (Listener, error) {
	return listen(addr, cert, listenOpts{})
}

// ListenWith is Listen with options.
func ListenWith(addr string, cert tls.Certificate, o ListenOpts) (Listener, error) {
	return listen(addr, cert, listenOpts{ListenOpts: o})
}

// listen binds TCP and UDP on the same port. With port 0 the kernel picks a
// free TCP port, but that UDP port may be taken by something else; then try
// another port rather than fail (this made tests flaky under load).
func listen(addr string, cert tls.Certificate, o listenOpts) (Listener, error) {
	_, port, _ := net.SplitHostPort(addr)
	for attempt := 0; ; attempt++ {
		ln, err := listenOnce(addr, cert, o)
		if err == nil || port != "0" || attempt == 20 || !errors.Is(err, syscall.EADDRINUSE) {
			return ln, err
		}
	}
}

func listenOnce(addr string, cert tls.Certificate, o listenOpts) (Listener, error) {
	if o.handshakeTimeout == 0 {
		o.handshakeTimeout = handshakeTimeout
	}
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{proto.ALPN},
	}
	tcpConf := tcpConfig(tlsConf, o.ListenOpts)
	tcpLn, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on tcp %s: %w", addr, err)
	}
	l := &listener{
		tcp:     tcpLn,
		tlsConf: tcpConf,
		timeout: o.handshakeTimeout,
		conns:   make(chan Conn),
	}
	l.ctx, l.cancel = context.WithCancel(context.Background())
	if o.HTTP {
		l.http = newConnListener(tcpLn.Addr(), l.ctx.Done())
	}
	if !o.noQUIC {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			l.cancel()
			tcpLn.Close()
			return nil, fmt.Errorf("parsing listen address: %w", err)
		}
		port := tcpLn.Addr().(*net.TCPAddr).Port
		qc := quicConfig()
		qc.HandshakeIdleTimeout = o.handshakeTimeout / 2
		// Clients open exactly one bidirectional stream (PROTOCOL.md §2).
		qc.MaxIncomingStreams = 1
		qc.MaxIncomingUniStreams = -1
		udpAddr := net.JoinHostPort(host, strconv.Itoa(port))
		if l.quic, err = quic.ListenAddr(udpAddr, tlsConf, qc); err != nil {
			l.cancel()
			tcpLn.Close()
			return nil, fmt.Errorf("listening on udp %s: %w", udpAddr, err)
		}
		l.wg.Add(1)
		go l.acceptQUIC()
	}
	l.wg.Add(1)
	go l.acceptTCP()
	return l, nil
}

func (l *listener) Addr() net.Addr { return l.tcp.Addr() }

func (l *listener) Accept(ctx context.Context) (Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.ctx.Done():
		return nil, ErrListenerClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *listener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		l.cancel()
		err = l.tcp.Close()
		if l.quic != nil {
			err = errors.Join(err, l.quic.Close())
		}
		l.wg.Wait()
	})
	return err
}

// deliver hands c to Accept, or closes it if the listener shuts down first.
func (l *listener) deliver(c Conn) {
	select {
	case l.conns <- c:
	case <-l.ctx.Done():
		c.Close()
	}
}

func (l *listener) acceptTCP() {
	defer l.wg.Done()
	for {
		raw, err := l.tcp.Accept()
		if err != nil {
			select {
			case <-l.ctx.Done():
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			// Transient errors (e.g. EMFILE): back off rather than spin.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if tc, ok := raw.(*net.TCPConn); ok {
			if rc, err := tc.SyscallConn(); err == nil {
				_ = limitUnsent("", "", rc)
			}
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			c, err := l.handshakeTCP(raw)
			if err != nil {
				raw.Close()
				return
			}
			if c != nil {
				l.deliver(c)
			}
		}()
	}
}

func (l *listener) handshakeTCP(raw net.Conn) (Conn, error) {
	ctx, cancel := l.handshakeCtx()
	defer cancel()
	tc := tls.Server(raw, l.tlsConf)
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	switch p := tc.ConnectionState().NegotiatedProtocol; {
	case p == proto.ALPN:
		return &tcpConn{Conn: tc}, nil
	case l.http != nil && (p == ALPNHTTP || p == ""):
		// No ALPN can't be a tc client (they always offer tc/1), and many
		// HTTP clients don't send any.
		l.http.deliver(tc)
		return nil, nil
	}
	return nil, errors.New("client did not negotiate ALPN " + proto.ALPN)
}

func (l *listener) acceptQUIC() {
	defer l.wg.Done()
	for {
		// quic-go only returns connections whose handshake completed, and runs
		// handshakes concurrently in the background.
		qc, err := l.quic.Accept(l.ctx)
		if err != nil {
			return // only fails once the listener is closed
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			ctx, cancel := l.handshakeCtx()
			defer cancel()
			// The stream becomes visible only once the client writes to it;
			// clients always send HELLO first, so this doesn't deadlock.
			st, err := qc.AcceptStream(ctx)
			if err != nil {
				qc.CloseWithError(0, "handshake_timeout")
				return
			}
			l.deliver(&quicConn{Stream: st, conn: qc})
		}()
	}
}

// handshakeCtx is bounded by the handshake timeout and cancelled on Close.
func (l *listener) handshakeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(l.ctx, l.timeout)
}
