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
	"github.com/skailama/tc/internal/server"
)

// TestWatchTamper_BreakReplayAcrossRestart: the viewer resets its replay
// window (and reassembler and decoder) when the streamer it watches goes
// live again under the same src (paneRoster: "live.Src != v.src ||
// v.ended" → v.win = replayWindow{}). Stopping and restarting a share
// keeps the participation, so src, stream id, epoch and the screen key are
// the same, and the streamer's seq keeps rising: every chunk of the
// previous stream still opens, its ts is within ±60 s, and with the
// window reset it is no longer a replay. A malicious server that drops
// the new stream and replays the old one makes the viewer show, as live,
// a pane the streamer stopped sharing (§14.5: "a 64-entry replay window
// per src"). The window (or a floor at the highest seq seen) must survive
// a restart within one participation.
func TestWatchTamper_BreakReplayAcrossRestart(t *testing.T) {
	s, _, us := huddlersSrv(t, "quic", "alice", "bob")
	alice, bob := us[0], us[1]
	dm := encryptedDM(t, alice, bob)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	joinAll(t, ctx, dm, alice, bob)

	var (
		mu       sync.Mutex
		recorded [][]byte
		replayed atomic.Bool
		mode     atomic.Int32 // 0 record, 1 drop new + replay old
	)
	s.SetTamper(&server.Tamper{Datagram: func(user uint64, b []byte) [][]byte {
		if user != bob.Me().ID || !proto.ScreenKind(b[0]) {
			return [][]byte{b}
		}
		if mode.Load() == 0 {
			mu.Lock()
			recorded = append(recorded, bytes.Clone(b))
			mu.Unlock()
			return [][]byte{b}
		}
		if replayed.Swap(true) {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		return recorded
	}})

	old := &fakePane{}
	old.set("OLD-PANE-the-streamer-stopped-sharing\r\n")
	if err := alice.StartShare(ctx, "old", old.capture); err != nil {
		t.Fatal(err)
	}
	eventually(t, "live", func() bool { _, ok := bob.LiveIn(dm); return ok })
	if err := bob.Watch(ctx, alice.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "old picture", func() bool { return strings.Contains(watchText(bob), "OLD-PANE") })
	alice.StopShare()
	eventually(t, "stream ended", func() bool { f, _ := bob.Watching(0); return f.Ended })

	mode.Store(1)
	cur := &fakePane{}
	cur.set("NEW-PANE\r\n")
	if err := alice.StartShare(ctx, "new", cur.capture); err != nil {
		t.Fatal(err)
	}
	eventually(t, "view restarted", func() bool { f, _ := bob.Watching(0); return !f.Ended })
	eventually(t, "server replayed", replayed.Load)
	time.Sleep(500 * time.Millisecond)
	if f, _ := bob.Watching(0); f.Grid != nil && strings.Contains(f.Grid.Text(), "OLD-PANE") && !f.Ended {
		st, _ := bob.WatchStats()
		t.Fatalf("viewer shows the replayed old stream as live (label %q, stats %+v)", f.Label, st)
	}
}
