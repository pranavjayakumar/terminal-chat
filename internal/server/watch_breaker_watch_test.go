package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// Breaker tests for pane sharing's relay (#133, PROTOCOL.md §14.5).

// slowWConn makes every write take as long as rate bytes/s would (no
// kernel buffer to hide in), so chunks wait in the server's stream queue.
// It hides the *tcpConn, so transport.Backlogged never fires.
type slowWConn struct {
	transport.Conn
	rate int
}

func (c slowWConn) Write(p []byte) (int, error) {
	time.Sleep(time.Duration(len(p)) * time.Second / time.Duration(c.rate))
	return c.Conn.Write(p)
}

// slowNextTCP slows the TCP connections accepted while slow is set.
type slowNextTCP struct {
	transport.Listener
	slow *atomic.Bool
	rate int
}

func (l slowNextTCP) Accept(ctx context.Context) (transport.Conn, error) {
	c, err := l.Listener.Accept(ctx)
	if err != nil || c.Kind() != "tcp" || !l.slow.Load() {
		return c, err
	}
	return slowWConn{c, l.rate}, nil
}

func startSlowNextServer(t *testing.T, rate int) (*testServer, *atomic.Bool) {
	t.Helper()
	srv, err := New(Config{DataDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.Listen("127.0.0.1:0", srv.Cert())
	if err != nil {
		t.Fatal(err)
	}
	srv.cfg.PublicAddr = ln.Addr().String()
	slow := &atomic.Bool{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Serve(ctx, slowNextTCP{ln, slow, rate}); close(done) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(func() { stop(); srv.Close() })
	return &testServer{Server: srv, addr: ln.Addr().String(), kind: "tcp", stop: stop}, slow
}

// TestWatch_BreakStaleChunksAfterRestart: §14.5 says queued screen
// datagrams "are discarded when the stream stops". The writers only check
// "is from still the live participant and to still watching" when a chunk
// leaves the queue (startScreen, writeScreen), so a stop followed by a
// restart by the same participant (Esc then /pair) before the queue
// drained lets the old stream's chunks through, after the rosters that
// told the viewer a new stream started. The viewer (paneRoster) has by
// then reset its replay window, reassembler and decoder for the new
// stream, so it shows the old pane again. A slow TCP viewer keeps chunks
// queued for over a second, which makes it easy to see.
func TestWatch_BreakStaleChunksAfterRestart(t *testing.T) {
	const rate = 8000 // bytes/s to the viewer: ~65 ms a chunk
	ts, slow := startSlowNextServer(t, rate)
	a := ts.watchClient(t, "ana", "tcp")
	slow.Store(true)
	b := ts.watchClient(t, "bo", "tcp")
	slow.Store(false)
	g := a.gen
	a.join(g)
	b.join(g)
	a.ok(proto.OpHuddleShare, proto.HuddleShareArgs{On: true}, nil)
	b.ok(proto.OpHuddleWatch, proto.HuddleWatchArgs{On: true}, nil)
	a.rosterWhere(g, func(p []proto.Part) bool { return partOf(p, b.user.ID).Watch })

	type got struct {
		at  time.Time
		pay string
	}
	var (
		mu   sync.Mutex
		recv []got
	)
	stopRead := make(chan struct{})
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			select {
			case f, ok := <-b.frames:
				if !ok {
					return
				}
				if f.Type == proto.TVoice && len(f.Payload) > proto.DgHeaderLen {
					mu.Lock()
					recv = append(recv, got{time.Now(), string(f.Payload[proto.DgHeaderLen:])})
					mu.Unlock()
				}
			case <-stopRead:
				return
			}
		}
	}()

	const n = 20
	for i := range n {
		a.sendVoice(a.screenChunk(uint32(i), fmt.Sprintf("old-stream-%02d%s", i, strings.Repeat(".", 400))))
	}
	time.Sleep(200 * time.Millisecond)
	// Esc, then /pair: the old stream stops, a new one starts.
	a.ok(proto.OpHuddleShare, proto.HuddleShareArgs{On: false}, nil)
	stopped := time.Now()
	a.ok(proto.OpHuddleShare, proto.HuddleShareArgs{On: true}, nil)

	time.Sleep(2500 * time.Millisecond)
	close(stopRead)
	<-readDone
	mu.Lock()
	defer mu.Unlock()
	late := 0
	for _, r := range recv {
		// One chunk may already have been mid-write at the stop.
		if r.at.After(stopped.Add(2 * time.Second * 512 / rate)) {
			late++
		}
	}
	t.Logf("viewer got %d of %d old-stream chunks, %d of them after the stream stopped", len(recv), n, late)
	if late > 0 {
		t.Fatalf("%d chunks of a stopped stream were delivered after it stopped (and a new one started)", late)
	}
}
