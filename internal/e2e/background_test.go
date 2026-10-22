package e2e

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/alert"
	"github.com/skailama/tc/internal/background"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/proto"
)

type noteLog struct {
	mu    sync.Mutex
	notes []notify.Note
}

func (l *noteLog) Send(n notify.Note) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.notes = append(l.notes, n)
	return true
}

func (l *noteLog) get() []notify.Note {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]notify.Note(nil), l.notes...)
}

// tc background against a real server: bob's chat window is closed, a DM
// from alice (end-to-end encrypted, decrypted locally) fires exactly one
// notification; with a chat window open it fires none; it survives a
// server restart; bob never shows as online; nothing is marked read.
func TestBackgroundNotifier(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "both online", func() bool { return alice.online() && bob.online() })
	bobID := bob.Me().ID
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dm, err := alice.OpenDM(ctx, []uint64{bobID})
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Send(dm.ID, "warm up", 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob decrypts the warm-up", func() bool { return bob.has(dm.ID, "warm up") })

	// Bob quits tc.
	bobHome := bob.home
	bob.cancel()
	bob.Close()
	eventually(t, "bob offline", func() bool { return alice.Presence(bobID).State == proto.PresOffline })

	out := &noteLog{}
	rctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- background.Run(rctx, background.Config{Home: bobHome, Version: "test", Out: out, Poll: 200 * time.Millisecond})
	}()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	online := func() bool {
		st, err := background.ReadStatus(bobHome)
		return err == nil && st.State == "online"
	}
	eventually(t, "background online", online)
	if !background.Running(bobHome) {
		t.Fatal("Running says no while Run holds the lock")
	}
	if st, _ := background.ReadStatus(bobHome); !st.Passive {
		t.Fatal("server didn't honor the passive session")
	}
	time.Sleep(300 * time.Millisecond)
	if p := alice.Presence(bobID).State; p == proto.PresOnline {
		t.Fatal("tc background made bob look online")
	}

	send := func(body string) {
		t.Helper()
		if err := alice.Send(dm.ID, body, 0); err != nil {
			t.Fatal(err)
		}
	}
	send("ping from alice")
	eventually(t, "one notification", func() bool { return len(out.get()) == 1 })
	if n := out.get()[0]; n.Title != "alice (DM)" || n.Body != "ping from alice" || n.Cue != notify.CueMention {
		t.Fatalf("notification %+v", n)
	}
	time.Sleep(alert.MentionWindow + time.Second) // the window closes with nothing more to say
	if n := len(out.get()); n != 1 {
		t.Fatalf("%d notifications for one DM", n)
	}

	// A chat window opens: the notifier lets go of its connection, the
	// window's client gets the DM.
	release, err := background.HoldUI(bobHome)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "background pauses", func() bool {
		st, err := background.ReadStatus(bobHome)
		return err == nil && st.State == "paused"
	})
	win, err := client.Open(bobHome)
	if err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithCancel(context.Background())
	go win.Run(wctx)
	send("while tc is open")
	eventually(t, "the window gets it", func() bool {
		for _, m := range win.Messages(dm.ID, 50) {
			if m.Body == "while tc is open" {
				return true
			}
		}
		return false
	})
	time.Sleep(1500 * time.Millisecond)
	if n := len(out.get()); n != 1 {
		t.Fatalf("%d notifications with tc open", n)
	}
	// It closes: the notifier reconnects and the next DM notifies.
	wcancel()
	win.Close()
	release()
	eventually(t, "background resumes", online)
	send("after tc closed")
	eventually(t, "notified after the window closed", func() bool { return len(out.get()) == 2 })
	if n := out.get()[1]; n.Body != "after tc closed" {
		t.Fatalf("after close: %+v", n)
	}

	// The server restarts: it reconnects and keeps notifying.
	s.stop()
	eventually(t, "background notices", func() bool { return !online() })
	startServer(t, dir, addr)
	eventually(t, "background reconnects", online)
	eventually(t, "alice reconnects", alice.online)
	time.Sleep(alert.Every)
	send("after the restart")
	eventually(t, "notified after the restart", func() bool { return len(out.get()) == 3 })

	// Nothing was marked read, here or on the server.
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	done <- nil // for the cleanup
	c, err := client.Open(bobHome)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	unread := func() (int, uint64) {
		v, _ := c.Channel(dm.ID)
		return v.Unread, v.Read
	}
	if n, r := unread(); n != 5 || r != 0 {
		t.Fatalf("after tc background: %d unread, read marker %d; want 5, 0", n, r)
	}
	rc, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	go c.Run(rc)
	eventually(t, "bob online", func() bool { return c.Status().State == client.Online })
	if n, r := unread(); n != 5 || r != 0 {
		t.Fatalf("server's read marker moved: %d unread, read %d", n, r)
	}
}

// A chat window that opens while a channel's notification window is still
// pending (the user clicked the popup and opened tc) must not leave that
// window open with its timer gone: after tc closes again, the next DM in
// that channel must still notify.
func TestBackgroundPauseMidWindow(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "both online", func() bool { return alice.online() && bob.online() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dm, err := alice.OpenDM(ctx, []uint64{bob.Me().ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Send(dm.ID, "warm up", 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob decrypts the warm-up", func() bool { return bob.has(dm.ID, "warm up") })
	bobHome := bob.home
	bob.cancel()
	bob.Close()

	out := &noteLog{}
	rctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- background.Run(rctx, background.Config{Home: bobHome, Version: "test", Out: out,
			Poll: 200 * time.Millisecond, UIPoll: 50 * time.Millisecond})
	}()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	state := func(want string) func() bool {
		return func() bool {
			st, err := background.ReadStatus(bobHome)
			return err == nil && st.State == want
		}
	}
	eventually(t, "background online", state("online"))
	send := func(body string) {
		t.Helper()
		if err := alice.Send(dm.ID, body, 0); err != nil {
			t.Fatal(err)
		}
	}
	send("first")
	eventually(t, "first notification", func() bool { return len(out.get()) == 1 })

	// tc opens inside the DM's 5 s window, and closes again.
	release, err := background.HoldUI(bobHome)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "background pauses", state("paused"))
	release()
	eventually(t, "background resumes", state("online"))
	time.Sleep(alert.MentionWindow + alert.Every)

	send("second")
	eventually(t, "notified after tc closed", func() bool { return len(out.get()) == 2 })
}
