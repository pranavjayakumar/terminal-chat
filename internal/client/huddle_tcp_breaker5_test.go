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
type slowLink struct {
	net.Conn
	rate int
}

func (c slowLink) Write(p []byte) (int, error) {
	time.Sleep(time.Duration(len(p)) * time.Second / time.Duration(c.rate))
	return c.Conn.Write(p)
}

// TestStreamVoiceUplinkLatency_Break5: voice on the TCP fallback through
// the real client send path (nonBlocking + streamDG) into an uplink slower
// than the voice bitrate: late voice is dropped, so latency stays bounded
// instead of growing for as long as the call lasts.
func TestStreamVoiceUplinkLatency_Break5(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	var mu sync.Mutex
	var lat []time.Duration
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
			sent := time.Unix(0, int64(binary.BigEndian.Uint64(f.Payload)))
			mu.Lock()
			lat = append(lat, time.Since(sent))
			mu.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	const rate = 4000 // bytes/s; the voice below is ~7.5 KB/s
	s := &session{codec: proto.NewCodec(slowLink{a, rate})}
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
	var worst time.Duration
	for _, d := range lat {
		worst = max(worst, d)
	}
	t.Logf("%d of 300 frames delivered, worst latency %v", len(lat), worst)
	if len(lat) < 50 {
		t.Fatalf("only %d frames delivered", len(lat))
	}
	if worst > tcpSendAge+150*time.Millisecond {
		t.Fatalf("voice delivered %v late on a slow TCP uplink", worst)
	}
}
