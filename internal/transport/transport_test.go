package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type dialFunc func(context.Context, string, string) (Conn, error)

var kinds = []struct {
	name string
	dial dialFunc
}{
	{"quic", DialQUIC},
	{"tcp", DialTCP},
}

func newServer(t *testing.T, o listenOpts) (Listener, tls.Certificate) {
	t.Helper()
	cert, err := LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := listen("127.0.0.1:0", cert, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln, cert
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// connect dials, writes a first byte (needed for QUIC stream visibility, like
// HELLO), and returns both ends.
func connect(t *testing.T, ln Listener, pin string, dial dialFunc) (client, server Conn) {
	t.Helper()
	ctx := testCtx(t)
	// One retry with a fresh socket: on macOS a loopback QUIC dial can get
	// an ephemeral port another socket holds on 127.0.0.1, which then
	// receives the server's replies (see transporttest.Dial).
	client, err := dial(ctx, ln.Addr().String(), pin)
	if err != nil {
		t.Logf("dial: %v (retrying)", err)
		client, err = dial(testCtx(t), ln.Addr().String(), pin)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	if _, err := client.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	server, err = ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	b := make([]byte, 1)
	if _, err := io.ReadFull(server, b); err != nil {
		t.Fatal(err)
	}
	if client.Kind() != server.Kind() {
		t.Fatalf("kind mismatch: client %s server %s", client.Kind(), server.Kind())
	}
	return client, server
}

func TestEcho(t *testing.T) {
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			ln, cert := newServer(t, listenOpts{})
			c, s := connect(t, ln, Pin(cert), k.dial)
			if c.Kind() != k.name {
				t.Fatalf("Kind = %s, want %s", c.Kind(), k.name)
			}
			go io.Copy(s, s)
			msg := bytes.Repeat([]byte("hello tc "), 10000)
			go c.Write(msg)
			got := make([]byte, len(msg))
			if _, err := io.ReadFull(c, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, msg) {
				t.Fatal("echo mismatch")
			}
			if err := c.CloseWithCode("server_shutdown"); err != nil {
				t.Fatal(err)
			}
			c.CloseWithCode("again") // idempotent
			c.Close()
		})
	}
}

func TestExporter(t *testing.T) {
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			ln, cert := newServer(t, listenOpts{})
			var prev []byte
			for range 2 {
				c, s := connect(t, ln, Pin(cert), k.dial)
				ce, err := c.Exporter()
				if err != nil {
					t.Fatal(err)
				}
				se, err := s.Exporter()
				if err != nil {
					t.Fatal(err)
				}
				if len(ce) != 32 || !bytes.Equal(ce, se) {
					t.Fatalf("exporter mismatch: client %x server %x", ce, se)
				}
				if bytes.Equal(ce, prev) {
					t.Fatal("exporter repeated across connections")
				}
				prev = ce
			}
		})
	}
}

func TestWrongPin(t *testing.T) {
	ln, _ := newServer(t, listenOpts{})
	other, err := LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range append(kinds, struct {
		name string
		dial dialFunc
	}{"race", Dial}) {
		t.Run(k.name, func(t *testing.T) {
			_, err := k.dial(testCtx(t), ln.Addr().String(), Pin(other))
			if err == nil {
				t.Fatal("dial succeeded with wrong pin")
			}
			if !errors.Is(err, ErrPinMismatch) || !strings.Contains(err.Error(), "pin mismatch") {
				t.Fatalf("want pin mismatch, got %v", err)
			}
		})
	}
}

func TestDialRace(t *testing.T) {
	t.Run("prefers quic", func(t *testing.T) {
		ln, cert := newServer(t, listenOpts{})
		c, err := Dial(testCtx(t), ln.Addr().String(), Pin(cert))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if c.Kind() != "quic" {
			t.Fatalf("Kind = %s, want quic", c.Kind())
		}
	})
	t.Run("falls back to tcp", func(t *testing.T) {
		ln, cert := newServer(t, listenOpts{noQUIC: true})
		start := time.Now()
		c, err := Dial(testCtx(t), ln.Addr().String(), Pin(cert))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if c.Kind() != "tcp" {
			t.Fatalf("Kind = %s, want tcp", c.Kind())
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("fallback took %v", d)
		}
	})
	t.Run("TC_TRANSPORT", func(t *testing.T) {
		ln, cert := newServer(t, listenOpts{})
		t.Setenv("TC_TRANSPORT", "tcp")
		c, err := Dial(testCtx(t), ln.Addr().String(), Pin(cert))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if c.Kind() != "tcp" {
			t.Fatalf("Kind = %s, want tcp", c.Kind())
		}
	})
}

func TestStalledHandshakeDoesNotBlock(t *testing.T) {
	ln, cert := newServer(t, listenOpts{handshakeTimeout: 500 * time.Millisecond})
	stalled, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()

	for _, k := range kinds {
		connect(t, ln, Pin(cert), k.dial)
	}

	// The stalled conn is dropped once the handshake timeout passes.
	stalled.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := stalled.Read(make([]byte, 1)); err == nil {
		t.Fatal("stalled conn got data")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("stalled conn was not closed by server")
	}
}

func TestListenerClose(t *testing.T) {
	ln, _ := newServer(t, listenOpts{})
	errc := make(chan error, 1)
	go func() {
		_, err := ln.Accept(context.Background())
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrListenerClosed) {
			t.Fatalf("Accept err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept not unblocked by Close")
	}
	ln.Close() // idempotent
}

func TestCertPersisted(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	if Pin(a) == "" || Pin(a) != Pin(b) {
		t.Fatalf("pins differ: %q %q", Pin(a), Pin(b))
	}
	if len(Pin(a)) != 43 {
		t.Fatalf("pin length %d, want 43", len(Pin(a)))
	}
	fi, err := os.Stat(filepath.Join(dir, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode().Perm())
	}
	if a.Leaf == nil || a.Leaf.NotAfter.Before(time.Now().AddDate(50, 0, 0)) {
		t.Fatal("cert validity too short")
	}

	os.Remove(filepath.Join(dir, "server.crt"))
	if _, err := LoadOrCreateCert(dir); err == nil {
		t.Fatal("expected error with only key present")
	}
}

func TestDatagrams(t *testing.T) {
	ln, cert := newServer(t, listenOpts{})
	c, s := connect(t, ln, Pin(cert), DialQUIC)
	cd, ok := Datagrams(c)
	sd, ok2 := Datagrams(s)
	if !ok || !ok2 {
		t.Fatal("QUIC conns should negotiate datagrams")
	}
	if err := cd.SendDatagram([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	got, err := sd.ReceiveDatagram(testCtx(t))
	if err != nil || string(got) != "hi" {
		t.Fatalf("got %q %v", got, err)
	}
	tc, ts := connect(t, ln, Pin(cert), DialTCP)
	if _, ok := Datagrams(tc); ok {
		t.Fatal("tcp client has no datagrams")
	}
	if _, ok := Datagrams(ts); ok {
		t.Fatal("tcp server conn has no datagrams")
	}
}
