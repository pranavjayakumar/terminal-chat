package e2e

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/transport"
	"github.com/skailama/tc/internal/voice"
)

// Voice on the TCP fallback (PROTOCOL.md §14.4).

// loginVia logs a user in with TC_TRANSPORT=kind through addr (the
// server's own address, or a proxy in front of it) and waits until online.
func loginVia(t *testing.T, s *srv, addr, kind, name string, admin bool) *user {
	t.Helper()
	t.Setenv("TC_TRANSPORT", kind)
	u := loginQUIC(t, s, addr, name, admin)
	eventually(t, name+" online", u.online)
	if got := u.Status().Transport; got != kind {
		t.Fatalf("%s on %q, want %q", name, got, kind)
	}
	return u
}

// TestHuddleMixedTCPAndQUIC: one participant on the TCP fallback, one on
// QUIC: both hear each other, and /huddle stats says which is which.
func TestHuddleMixedTCPAndQUIC(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	ana := loginVia(t, s, addr, "tcp", "ana", true)
	bo := loginVia(t, s, addr, "quic", "bo", false)
	gen := ana.channel("general")
	eventually(t, "bo sees #general", func() bool { return bo.channel("general") == gen })
	fa, fb := &fakeAudio{}, &fakeAudio{}
	ana.SetVoicePlatform(fa.platform)
	bo.SetVoicePlatform(fb.platform)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, u := range []*user{ana, bo} {
		if err := u.JoinHuddle(ctx, gen); err != nil {
			t.Fatalf("%s join: %v", u.Me().Name, err)
		}
	}
	eventually(t, "both in the roster", func() bool { return len(ana.Huddle(gen)) == 2 && len(bo.Huddle(gen)) == 2 })
	if st, _ := ana.HuddleStats(); !st.TCP {
		t.Fatal("ana's stats don't say tcp")
	}
	if st, _ := bo.HuddleStats(); st.TCP {
		t.Fatal("bo's stats say tcp")
	}
	ana.SetTalking(true)
	if h := talk(fa, []*fakeAudio{fb}, 7, 40)[0]; len(h) < 20 {
		t.Fatalf("bo (quic) heard %d frames from ana (tcp)", len(h))
	}
	ana.SetTalking(false)
	bo.SetTalking(true)
	if h := talk(fb, []*fakeAudio{fa}, 9, 40)[0]; len(h) < 20 {
		t.Fatalf("ana (tcp) heard %d frames from bo (quic)", len(h))
	}
}

// bigCodec is the fake codec with 400-byte frames: enough voice to
// saturate the throttled link below.
type bigCodec struct{ fCodec }
type bigEnc struct{}

func (bigCodec) NewEncoder() (voice.Encoder, error) { return bigEnc{}, nil }
func (bigEnc) Encode(pcm []int16, out []byte) (int, error) {
	binary.BigEndian.PutUint16(out, uint16(pcm[0]))
	clear(out[2:400])
	return 400, nil
}

func (f *fakeAudio) bigPlatform() (voice.Platform, error) {
	p, err := f.platform()
	p.Codec = bigCodec{}
	return p, err
}

// slowConn is a TCP session whose link carries rate bytes/s: each write
// takes as long as sending it would, with no buffer to hide in (what
// TCP_NOTSENT_LOWAT gives a Linux server; macOS loopback buffers ~350 KB
// whatever the socket options, so a proxy can't emulate this).
type slowConn struct {
	transport.Conn
	rate int
}

func (c slowConn) Write(p []byte) (int, error) {
	time.Sleep(time.Duration(len(p)) * time.Second / time.Duration(c.rate))
	return c.Conn.Write(p)
}

// slowTCP wraps a listener: its TCP connections become slowConns.
type slowTCP struct {
	transport.Listener
	rate int
}

func (l slowTCP) Accept(ctx context.Context) (transport.Conn, error) {
	c, err := l.Listener.Accept(ctx)
	if err != nil || c.Kind() != "tcp" {
		return c, err
	}
	return slowConn{c, l.rate}, nil
}

// startSlowServer is startServer whose TCP sessions get rate bytes/s
// from the server.
func startSlowServer(t *testing.T, rate int) (*srv, string) {
	t.Helper()
	s, err := server.New(server.Config{DataDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln := listenRetry(t, "127.0.0.1:0", s)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx, slowTCP{ln, rate}); close(done) }()
	t.Cleanup(func() { cancel(); <-done; s.Close() })
	return &srv{s, func() {}}, ln.Addr().String()
}

// TestHuddleTCPSlowLink: bo (QUIC) talks more voice than ana's TCP link
// can carry. The server drops what doesn't fit instead of queueing it, so
// ana's voice latency stays bounded and a chat message sent mid-call
// still arrives promptly. Unbounded, the backlog would grow by ~0.8 s per
// second of call.
func TestHuddleTCPSlowLink(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time")
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	// ~22 KB/s of voice into a 12 KB/s link.
	s, addr := startSlowServer(t, 12000)
	bo := loginVia(t, s, addr, "quic", "bo", true)
	ana := loginVia(t, s, addr, "tcp", "ana", false)
	gen := bo.channel("general")
	eventually(t, "ana sees #general", func() bool { return ana.channel("general") == gen })
	fa, fb := &fakeAudio{}, &fakeAudio{}
	ana.SetVoicePlatform(fa.bigPlatform)
	bo.SetVoicePlatform(fb.bigPlatform)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, u := range []*user{ana, bo} {
		if err := u.JoinHuddle(ctx, gen); err != nil {
			t.Fatalf("%s join: %v", u.Me().Name, err)
		}
	}
	eventually(t, "both in the roster", func() bool { return len(ana.Huddle(gen)) == 2 })
	bo.SetTalking(true)

	const frames = 400 // 8 s
	var mu sync.Mutex
	sentAt := make([]time.Time, frames+1)
	var lat []time.Duration // per frame ana played, after the first second
	var chatSent, chatGot time.Time
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { // ana's device: plays out in real time
		defer close(done)
		tk := time.NewTicker(voice.FrameDur)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-tk.C:
				v := fa.tick(0)
				mu.Lock()
				if v > 0 && int(v) <= frames && !sentAt[v].IsZero() && v > 50 {
					lat = append(lat, now.Sub(sentAt[v]))
				}
				if chatGot.IsZero() && !chatSent.IsZero() && ana.has(gen, "mid-call") {
					chatGot = now
				}
				mu.Unlock()
			}
		}
	}()
	tk := time.NewTicker(voice.FrameDur)
	for i := 1; i <= frames; i++ {
		<-tk.C
		mu.Lock()
		sentAt[i] = time.Now()
		mu.Unlock()
		fb.tick(int16(i))
		if i == frames/2 {
			mu.Lock()
			chatSent = time.Now()
			mu.Unlock()
			if err := bo.Send(gen, "mid-call", 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	tk.Stop()
	eventually(t, "chat arrives", func() bool { mu.Lock(); defer mu.Unlock(); return !chatGot.IsZero() })
	close(stop)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if d := chatGot.Sub(chatSent); d > time.Second {
		t.Errorf("chat took %v behind voice", d)
	}
	if len(lat) < 50 {
		t.Fatalf("ana played only %d frames", len(lat))
	}
	var worst time.Duration
	for _, d := range lat {
		worst = max(worst, d)
	}
	t.Logf("chat %v; voice: %d frames played, latency first %v last %v worst %v",
		chatGot.Sub(chatSent), len(lat), lat[0], lat[len(lat)-1], worst)
	if worst > 2500*time.Millisecond {
		t.Errorf("voice latency reached %v", worst)
	}
}
