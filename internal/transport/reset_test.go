package transport

import (
	"io"
	"testing"
	"time"
)

// QA v0.6.4: after the server crashed (SIGKILL, OOM, host reboot) and came
// back on the same port, QUIC clients kept sending into the dead connection
// for ~45 s (the client's receive timeout): the new process silently dropped
// packets of connections it didn't know. With a stateless reset key derived
// from the server's TLS key, the restarted server resets them at once.
func TestQUICResetAfterCrash(t *testing.T) {
	ln, cert := newServer(t, listenOpts{})
	c, _ := connect(t, ln, Pin(cert), DialQUIC)
	addr := ln.Addr().String()

	// Crash: the sockets vanish without a CONNECTION_CLOSE.
	l := ln.(*listener)
	l.udp.Close()
	l.tcp.Close()

	var ln2 Listener
	var err error
	for range 50 { // the old transport may hold the port briefly
		if ln2, err = listen(addr, cert, listenOpts{}); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln2.Close() })

	errc := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(c, make([]byte, 1))
		errc <- err
	}()
	start := time.Now()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-errc:
			if err == nil {
				t.Fatal("read data from a crashed server")
			}
			t.Logf("connection ended after %v: %v", time.Since(start).Round(time.Millisecond), err)
			return
		case <-tick.C:
			c.Write([]byte("x")) // like a queued send after the crash
		case <-deadline:
			t.Fatal("client still thinks the crashed server's connection is alive after 5 s")
		}
	}
}
