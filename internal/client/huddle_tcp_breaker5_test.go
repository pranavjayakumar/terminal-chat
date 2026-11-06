package client

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// slowLink is the client's side of an uplink that carries rate bytes/s:
// each write takes as long as sending it. (A loopback proxy can't emulate
// this on macOS, whose loopback absorbs ~350 KB whatever the socket
// buffers; on a real link transport.Backlogged catches the kernel's part.)
// onWrite, if set, sees each write as it enters the link.
type slowLink struct {
	net.Conn
	rate    int
	onWrite func([]byte)
}

func (c slowLink) Write(p []byte) (int, error) {
	if c.onWrite != nil {
		c.onWrite(p)
	}
	time.Sleep(time.Duration(len(p)) * time.Second / time.Duration(c.rate))
	return c.Conn.Write(p)
}

// TestStreamVoiceUplinkLatency_Break5: voice on the TCP fallback through
// the real client send path (nonBlocking + streamDG) into an uplink slower
// than the voice bitrate: late voice is dropped, so latency stays bounded
// instead of growing for as long as the call lasts.
//
// The tight bound is checked where the client controls it: how long a
// frame waited before it entered the link (tcpSendAge, plus slack for one
// preemption). Delivery adds the link's own transfer time and the reader's
// scheduling, which on a loaded -race runner (macOS CI) pushed a frame a few
// ms past a delivery-side bound of tcpSendAge+150ms; that side now only
// checks the latency doesn't grow. Without the drop the backlog here
// reaches ~3 s by the end.
func TestStreamVoiceUplinkLatency_Break5(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	var mu sync.Mutex
	var lat, waited []time.Duration
	age := func(p []byte) time.Duration {
		return time.Since(time.Unix(0, int64(binary.BigEndian.Uint64(p))))
	}
	go func() {
		codec := proto.NewCodec(b)
		codec.SetMaxFrame(proto.MaxFrame)
		for {
			f, err := codec.Read()
			if err != nil {
				return
			}
			if f.Type != proto.TVoice || len(f.Payload) < 8 {
				continue
			}
			d := age(f.Payload)
			mu.Lock()
			lat = append(lat, d)
			mu.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	const rate = 4000 // bytes/s; the voice below is ~7.5 KB/s
	link := slowLink{Conn: a, rate: rate, onWrite: func(p []byte) {
		// The codec flushes a frame in one write: 5-byte header, payload.
		if len(p) >= 13 && proto.FrameType(p[4]) == proto.TVoice {
			d := age(p[5:])
			mu.Lock()
			waited = append(waited, d)
			mu.Unlock()
		}
	}}
	s := &session{codec: proto.NewCodec(link)}
	send := nonBlocking(ctx, streamDG{s}, true)

	start := time.Now()
	for i := range 300 { // 6 s of 20 ms frames
		b := make([]byte, 150)
		binary.BigEndian.PutUint64(b, uint64(time.Now().UnixNano()))
		_ = send(b)
		time.Sleep(time.Until(start.Add(time.Duration(i+1) * 20 * time.Millisecond)))
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	var worst, worstWait time.Duration
	for _, d := range lat {
		worst = max(worst, d)
	}
	for _, d := range waited {
		worstWait = max(worstWait, d)
	}
	t.Logf("%d of 300 frames delivered, worst latency %v, worst wait before the link %v", len(lat), worst, worstWait)
	if len(lat) < 50 {
		t.Fatalf("only %d frames delivered", len(lat))
	}
	if worstWait > tcpSendAge+50*time.Millisecond {
		t.Fatalf("voice waited %v before entering a slow TCP uplink", worstWait)
	}
	if worst > time.Second {
		t.Fatalf("voice delivered %v late on a slow TCP uplink", worst)
	}
}
