package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
)

// Pane sharing in a huddle (PROTOCOL.md §14.5), real clients through a
// real server.

// fakePane is a capture function whose contents the test sets.
type fakePane struct {
	mu   sync.Mutex
	text string
	err  error
	n    int
}

func (f *fakePane) set(s string) {
	f.mu.Lock()
	f.text = s
	f.mu.Unlock()
}

func (f *fakePane) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakePane) capture(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return f.text, f.err
}

func (f *fakePane) captures() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// watchText is what u sees of the pane it watches ("" before a picture).
func watchText(u *user) string {
	f, ok := u.Watching(0)
	if !ok || f.Grid == nil {
		return ""
	}
	return f.Grid.Text()
}

// joinBoth puts every user in ch's huddle with fake audio.
func joinAll(t *testing.T, ctx context.Context, ch uint64, us ...*user) {
	t.Helper()
	for _, u := range us {
		u.SetVoicePlatform((&fakeAudio{}).platform)
		if err := u.JoinHuddle(ctx, ch); err != nil {
			t.Fatalf("%s join: %v", u.Me().Name, err)
		}
	}
	eventually(t, "full roster", func() bool {
		for _, u := range us {
			if len(u.Huddle(ch)) != len(us) {
				return false
			}
		}
		return true
	})
}

const paneSecret = "ghp_" + "Zx9Yw8Vu7Ts6Rq5Po4Nm3Lk2Ji1Hg0FeDcBaZy"

func checkWatch(t *testing.T, ctx context.Context, streamer, viewer, other *user, ch uint64) {
	t.Helper()
	pane := &fakePane{}
	pane.set("\x1b[32m$ go test ./...\x1b[0m\r\nok   tc/internal/tui   28.4s\r\nexport GITHUB_TOKEN=" + paneSecret + "\r\n")
	if err := viewer.Watch(ctx, streamer.Me().ID); !errors.Is(err, client.ErrNotLive) {
		t.Fatalf("watch before anyone shares: %v", err)
	}
	if err := streamer.StartShare(ctx, "build pane", pane.capture); err != nil {
		t.Fatal(err)
	}
	eventually(t, "viewer sees the stream live", func() bool { u, ok := viewer.LiveIn(ch); return ok && u == streamer.Me().ID })
	// Nobody watches yet: nothing is captured.
	time.Sleep(300 * time.Millisecond)
	if n := pane.captures(); n != 0 {
		t.Fatalf("captured %d times with nobody watching", n)
	}
	if err := viewer.Watch(ctx, streamer.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first picture", func() bool { return strings.Contains(watchText(viewer), "28.4s") })
	got := watchText(viewer)
	if strings.Contains(got, paneSecret) || strings.Contains(got, "Zx9Yw8") || !strings.Contains(got, "export GITHUB_TOKEN=") {
		t.Fatalf("secret not scrubbed: %q", got)
	}
	if f, _ := viewer.Watching(0); f.Label != "build pane" || f.Ended || f.User != streamer.Me().ID {
		t.Fatalf("frame %+v", f)
	}
	if info, ok := streamer.Sharing(); !ok || info.Watchers != 1 || info.Scrubbed != 1 {
		t.Fatalf("share info %+v %v", info, ok)
	}
	if _, ok := other.Watching(0); ok {
		t.Fatal("a non-watcher has a picture")
	}
	// Changes arrive as diffs.
	for i := range 5 {
		pane.set(fmt.Sprintf("$ make\r\nstep %d of 5\r\n", i+1))
		want := fmt.Sprintf("step %d of 5", i+1)
		eventually(t, want, func() bool { return strings.Contains(watchText(viewer), want) })
	}
	if st, _ := viewer.WatchStats(); st.Auth+st.Key+st.BadFrame+st.BadUpdate+st.Skew != 0 || st.Applied < 6 {
		t.Fatalf("viewer stats %+v", st)
	}
	// Stopping ends the stream for the viewer, which keeps the last picture.
	streamer.StopShare()
	eventually(t, "stream ended", func() bool { f, _ := viewer.Watching(0); return f.Ended })
	n := pane.captures()
	time.Sleep(300 * time.Millisecond)
	if pane.captures() != n {
		t.Fatal("still capturing after StopShare")
	}
	viewer.StopWatch()
	if _, ok := viewer.Watching(0); ok {
		t.Fatal("still watching after StopWatch")
	}
}

func TestWatchQUIC(t *testing.T) {
	us := huddlers(t, "quic", "ana", "bo", "cy")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gen := us[0].channel("general")
	joinAll(t, ctx, gen, us...)
	checkWatch(t, ctx, us[0], us[1], us[2], gen)
}

func TestWatchTCP(t *testing.T) {
	us := huddlers(t, "tcp", "ana", "bo", "cy")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gen := us[0].channel("general")
	joinAll(t, ctx, gen, us...)
	checkWatch(t, ctx, us[0], us[1], us[2], gen)
}

// TestWatchMixed: the streamer on the TCP fallback, the viewer on QUIC, and
// the other way round.
func TestWatchMixed(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	ana := loginVia(t, s, addr, "tcp", "ana", true)
	bo := loginVia(t, s, addr, "quic", "bo", false)
	cy := loginVia(t, s, addr, "quic", "cy", false)
	gen := ana.channel("general")
	eventually(t, "all see #general", func() bool { return bo.channel("general") == gen && cy.channel("general") == gen })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	joinAll(t, ctx, gen, ana, bo, cy)
	checkWatch(t, ctx, ana, bo, cy, gen)
	checkWatch(t, ctx, bo, ana, cy, gen)
}

func TestWatchEncryptedDM(t *testing.T) {
	us := huddlers(t, "quic", "alice", "bob")
	alice, bob := us[0], us[1]
	dm := encryptedDM(t, alice, bob)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	joinAll(t, ctx, dm, alice, bob)
	pane := &fakePane{}
	pane.set("top secret build output\r\n")
	if err := alice.StartShare(ctx, "build", pane.capture); err != nil {
		t.Fatal(err)
	}
	eventually(t, "live", func() bool { _, ok := bob.LiveIn(dm); return ok })
	if err := bob.Watch(ctx, alice.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "picture", func() bool { return strings.Contains(watchText(bob), "top secret") })
	if f, _ := bob.Watching(0); !f.Enc {
		t.Fatal("encrypted DM's stream not marked encrypted")
	}
	if info, _ := alice.Sharing(); !info.Enc {
		t.Fatal("streamer doesn't say encrypted")
	}
	// One stream per huddle.
	if err := bob.StartShare(ctx, "mine", pane.capture); err == nil {
		t.Fatal("second stream in one huddle")
	}
	// The pane goes away: the stream stops by itself and says why.
	pane.fail(errors.New("no such pane"))
	eventually(t, "stream stopped", func() bool { _, ok := alice.Sharing(); return !ok })
	if err := alice.ShareStopped(); err == nil {
		t.Fatal("no reason for the stop")
	}
	eventually(t, "viewer sees it end", func() bool { f, _ := bob.Watching(0); return f.Ended })
	// Leaving the huddle ends the view.
	bob.LeaveHuddle()
	if _, ok := bob.Watching(0); ok {
		t.Fatal("view outlived the huddle")
	}
}

// encryptedDM opens a DM between a and b and waits until both hold its key.
func encryptedDM(t *testing.T, a, b *user) uint64 {
	t.Helper()
	sid := a.sid(t)
	eventually(t, "devices verified both ways", func() bool {
		return a.verifiedDevices(sid, b.Me().ID) == 1 && b.verifiedDevices(sid, a.Me().ID) == 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dm, err := a.OpenDM(ctx, []uint64{b.Me().ID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the DM", func() bool { _, ok := b.Channel(dm.ID); return ok })
	a.send(t, ctx, dm.ID, "hi")
	eventually(t, "both keyed", func() bool {
		return a.E2EStatus(dm.ID).State == client.E2EOK && b.E2EStatus(dm.ID).State == client.E2EOK
	})
	return dm.ID
}
