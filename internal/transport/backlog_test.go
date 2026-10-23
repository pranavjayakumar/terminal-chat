package transport

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestBacklogged: a TCP connection whose peer stops reading reports a
// backlog once its socket fills (the plumbing to the OS counters works);
// an idle one doesn't.
func TestBacklogged(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no unsent-bytes counter here")
	}
	cert, err := LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listen("127.0.0.1:0", cert)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		c, err := ln.Accept(ctx)
		if err == nil {
			<-ctx.Done() // never reads
			c.Close()
		}
	}()
	c, err := DialTCP(ctx, ln.Addr().String(), Pin(cert))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if Backlogged(c) {
		t.Fatal("idle connection backlogged")
	}
	buf := make([]byte, 16<<10)
	for range 512 { // up to 8 MiB
		if Backlogged(c) {
			return
		}
		_ = c.(*tcpConn).SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := c.Write(buf); err != nil {
			break // the socket is full
		}
	}
	if !Backlogged(c) {
		t.Fatal("full socket not reported as backlogged")
	}
}
