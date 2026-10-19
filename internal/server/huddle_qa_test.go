package server

import (
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// A participant whose client died without closing the connection (crash,
// sleep, network gone) leaves the roster within seconds, not when the
// connection's idle timeout finally fires (#75). One that answers the
// server's PINGs stays.
func TestHuddleDeadPeerLeavesRoster(t *testing.T) {
	ts := startServer(t, "quic")
	ts.hud.pingEvery, ts.hud.deadAfter = 50*time.Millisecond, 500*time.Millisecond
	a, b := ts.voiceClient(t, "ana"), ts.voiceClient(t, "bo")
	g := a.gen
	a.join(g)
	b.join(g)
	inHuddle := func(user uint64) bool {
		ts.hud.mu.RLock()
		defer ts.hud.mu.RUnlock()
		for sess := range ts.hud.bySess {
			if sess.user.ID == user {
				return true
			}
		}
		return false
	}
	// a goes silent: it never answers a PING again. b answers them.
	start := time.Now()
	var aGone time.Duration
	for time.Since(start) < 3*time.Second {
		f := b.recv("frame")
		if f.Type == proto.TPing {
			if err := b.codec.WriteRaw(proto.TPong, f.Payload); err != nil {
				t.Fatal(err)
			}
		}
		if aGone == 0 && !inHuddle(a.user.ID) {
			aGone = time.Since(start)
		}
	}
	if aGone == 0 {
		t.Fatal("a participant that stopped answering is still in the huddle after 3 s")
	}
	if !inHuddle(b.user.ID) {
		t.Fatal("a participant that answers PINGs was dropped")
	}
	t.Logf("silent participant dropped after %v", aGone)
}
