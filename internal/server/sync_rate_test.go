package server

import (
	"io"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// startTrickleSync opens a second session for alice and asks it for a
// ~12 MiB catch-up that the caller reads (or doesn't) from c.conn.
func startTrickleSync(t *testing.T, transport string) (*session, *tclient) {
	t.Helper()
	ts := startServer(t, transport)
	alice := ts.register(t, "alice", true)
	snap, sr := alice.syncFresh()
	bulkMessages(t, ts.Server, generalID(t, snap), alice.user.ID, 200, 60<<10)
	before := sessionsNow(ts.Server)
	c, bye := ts.handshake(t, alice.signer, "", "")
	if bye != "" {
		t.Fatal(bye)
	}
	if c.conn.Kind() != transport {
		t.Fatalf("transport %s, want %s", c.conn.Kind(), transport)
	}
	sess := otherSession(t, ts.Server, alice.user.ID, before)
	c.codec.Write(proto.TReq, proto.Req{ID: 1, Op: proto.OpSync,
		Args: proto.MustRaw(proto.SyncArgs{Since: sr.Head, ServerID: alice.welcome.ServerID})})
	for deadline := time.Now().Add(5 * time.Second); sess.writing.Load() == 0 || !sess.syncing.Load(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no sync write in progress")
		}
	}
	return sess, c
}

func isClosed(sess *session) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.closed
}

// A client that reads a sliver of its catch-up every few seconds is never
// stuck for syncStuck (the socket keeps draining), so it used to hold its
// sync, and the live events buffered behind it, forever. Below syncMinRate
// over a syncRateWindow it is a slow consumer. (The window is simulated:
// the checks run with now a whole window apart.)
func TestSyncTrickleIsSlowConsumer(t *testing.T) {
	for _, tr := range []string{"tcp", "quic"} {
		t.Run(tr, func(t *testing.T) {
			sess, c := startTrickleSync(t, tr)
			// Let the socket buffers fill, so the writer waits on us.
			for last := int64(-1); last != sess.wrote.Load(); time.Sleep(300 * time.Millisecond) {
				last = sess.wrote.Load()
			}
			t0 := time.Now()
			sess.checkSyncRate(t0)
			buf := make([]byte, 1<<10)
			for i := range 5 { // a trickle: what one window's reads would be
				if _, err := io.ReadFull(c.conn, buf); err != nil {
					t.Fatal(err)
				}
				sess.checkSyncRate(t0.Add(time.Duration(i+1) * 5 * time.Second))
				sess.checkStuck(time.Now(), syncStuck) // not stuck: moving
			}
			if isClosed(sess) {
				t.Fatal("closed before a whole window passed")
			}
			sess.checkSyncRate(t0.Add(syncRateWindow + time.Second))
			if !isClosed(sess) {
				t.Fatalf("a sync moving %d bytes in %v is still open", sess.wrote.Load()-sess.rateWrote, syncRateWindow)
			}
		})
	}
}

// A slow but steady reader keeps its sync: #182's ~11 KB/s QUIC link moves
// about 20× syncMinRate. Here a reader drains 4 KiB every 5 ms and the
// check runs a simulated window later.
func TestSyncSteadyReaderKeepsSync(t *testing.T) {
	for _, tr := range []string{"tcp", "quic"} {
		t.Run(tr, func(t *testing.T) {
			sess, c := startTrickleSync(t, tr)
			stop := make(chan struct{})
			defer close(stop)
			t0 := time.Now()
			sess.checkSyncRate(t0)
			go func() {
				buf := make([]byte, 4<<10)
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := io.ReadFull(c.conn, buf); err != nil {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			// syncMinRate × the window, plus a chunk, must reach the transport.
			need := int64(syncMinRate*(syncRateWindow+time.Second).Seconds()) + progressChunk
			for deadline := time.Now().Add(10 * time.Second); sess.wrote.Load()-sess.rateWrote < need; time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("reader too slow for the test: %d bytes", sess.wrote.Load()-sess.rateWrote)
				}
			}
			if sess.writing.Load() == 0 {
				t.Skip("the sync finished early")
			}
			sess.checkSyncRate(t0.Add(syncRateWindow + time.Second))
			if isClosed(sess) {
				t.Fatal("a steadily read sync was closed as a slow consumer")
			}
		})
	}
}

// Live events buffered behind a sync were capped at proto.MaxQueue frames
// only: 1024 frames of up to 64 KiB each is 64 MiB per syncing session.
// They are capped at maxQueueBytes too.
func TestSyncPendingBytesCapped(t *testing.T) {
	sess, _ := startTrickleSync(t, "tcp")
	payload := make([]byte, 64<<10)
	n := 0
	for ; n < proto.MaxQueue && !isClosed(sess); n++ {
		sess.srv.mu.Lock()
		sess.pushLive(outFrame{t: proto.TEvent, payload: payload, seq: uint64(1 << 40)})
		sess.srv.mu.Unlock()
	}
	if !isClosed(sess) {
		t.Fatalf("%d live frames (%d MiB) buffered behind a sync", n, n*len(payload)>>20)
	}
	if n*len(payload) > maxQueueBytes+len(payload) {
		t.Fatalf("closed only after %d bytes", n*len(payload))
	}
}
