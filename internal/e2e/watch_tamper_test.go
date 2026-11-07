//go:build tctamper

package e2e

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/screen"
	"github.com/skailama/tc/internal/server"
)

// TestWatchTamper: a malicious server relaying an encrypted huddle's
// shared pane can neither read it nor make the viewer show anything the
// streamer didn't send: altered, forged, downgraded and replayed chunks
// are all dropped. Over QUIC datagrams and the TCP fallback.
func TestWatchTamper(t *testing.T) {
	for _, tr := range []string{"quic", "tcp"} {
		t.Run(tr, func(t *testing.T) { testWatchTamper(t, tr) })
	}
}

func testWatchTamper(t *testing.T, tr string) {
	s, _, us := huddlersSrv(t, tr, "alice", "bob")
	alice, bob := us[0], us[1]
	dm := encryptedDM(t, alice, bob)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	joinAll(t, ctx, dm, alice, bob)

	var (
		mu   sync.Mutex
		seen [][]byte
		mode atomic.Int32 // 0 honest+record, 1 flip, 2 forge, 3 replay
	)
	const (
		honest = iota
		flip
		forge
		replay
	)
	// A plaintext update the server writes itself: a valid keyframe,
	// as kind 0x03 (downgrade) and as a sealed-looking 0x04 it can't seal.
	var e screen.Encoder
	fake, _, _ := e.Encode(screen.Parse("FORGED BY SERVER", 40, 1), "evil", false)
	fakeChunks, _ := screen.Chunks(1<<30, fake)
	s.SetTamper(&server.Tamper{Datagram: func(user uint64, b []byte) [][]byte {
		if user != bob.Me().ID || !proto.ScreenKind(b[0]) {
			return [][]byte{b}
		}
		mu.Lock()
		seen = append(seen, bytes.Clone(b))
		mu.Unlock()
		switch mode.Load() {
		case flip:
			c := bytes.Clone(b)
			c[len(c)-1] ^= 1 // the tag
			h := bytes.Clone(b)
			h[1] ^= 0x80 // a header bit (AD)
			return [][]byte{c, h}
		case forge:
			var out [][]byte
			for i, ch := range fakeChunks {
				d := proto.Datagram{Src: uint32(b[2])<<24 | uint32(b[3])<<16 | uint32(b[4])<<8 | uint32(b[5]), Seq: 1<<31 + uint32(i), Payload: ch}
				d.Kind = proto.DgScreen
				out = append(out, d.Marshal())
				d.Kind, d.Epoch, d.TS = proto.DgScreenEnc, 1, uint32(time.Now().Unix())
				d.Payload = append(bytes.Clone(ch), make([]byte, 16)...)
				out = append(out, d.Marshal())
			}
			return out
		case replay:
			return [][]byte{b, b, b}
		}
		return [][]byte{b}
	}})

	pane := &fakePane{}
	const marker = "PLAINTEXT-MARKER-0123456789"
	pane.set(strings.Repeat(marker+" ", 3) + "\r\n")
	if err := alice.StartShare(ctx, "build", pane.capture); err != nil {
		t.Fatal(err)
	}
	eventually(t, "live", func() bool { _, ok := bob.LiveIn(dm); return ok })
	if err := bob.Watch(ctx, alice.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "picture", func() bool { return strings.Contains(watchText(bob), marker) })

	// The server only ever relayed sealed chunks it can't read.
	mu.Lock()
	for _, b := range seen {
		if b[0] != proto.DgScreenEnc || bytes.Contains(b, []byte("PLAINTEXT")) {
			t.Fatalf("server saw a readable chunk: %x", b[:12])
		}
	}
	seen = nil
	mu.Unlock()

	// Altered chunks (ciphertext or header) never open; the picture stays.
	mode.Store(flip)
	before, _ := bob.WatchStats()
	pane.set("second picture\r\n")
	eventually(t, "flipped chunks rejected", func() bool {
		st, _ := bob.WatchStats()
		return st.Auth > before.Auth
	})
	if txt := watchText(bob); !strings.Contains(txt, marker) && !strings.Contains(txt, "second picture") {
		t.Fatalf("picture after tampering: %q", txt)
	}
	// Forged: a server-made update, in plaintext (downgrade) or "sealed"
	// without the key, is dropped.
	mode.Store(forge)
	pane.set("third picture\r\n")
	eventually(t, "forgeries rejected", func() bool {
		st, _ := bob.WatchStats()
		return st.BadFrame > before.BadFrame && st.Auth > before.Auth+2
	})
	if strings.Contains(watchText(bob), "FORGED") {
		t.Fatal("viewer shows the server's forgery")
	}
	// Honest again: the stream recovers (the next change or refresh).
	mode.Store(honest)
	pane.set("fourth picture\r\n")
	eventually(t, "recovered", func() bool { return strings.Contains(watchText(bob), "fourth picture") })
	// Replays within the stream are dropped.
	mode.Store(replay)
	pane.set("fifth picture\r\n")
	eventually(t, "replays dropped", func() bool {
		st, _ := bob.WatchStats()
		return st.Replay > before.Replay && strings.Contains(watchText(bob), "fifth picture")
	})
	if strings.Contains(watchText(bob), "FORGED") {
		t.Fatal("forgery shown")
	}
}
