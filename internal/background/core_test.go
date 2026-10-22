package background

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/alert"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/proto"
)

// fakeSource is an in-memory cache: me (1) and bob (2) share DM 20 and
// #general (10).
type fakeSource struct {
	mu    sync.Mutex
	prefs client.NotifyPrefs
}

func (f *fakeSource) Me() proto.User { return proto.User{ID: 1, Name: "alice"} }
func (f *fakeSource) Channels() []client.ChannelView {
	return []client.ChannelView{
		{Channel: proto.Channel{ID: 10, Name: "general", Kind: proto.ChanPublic, Members: []uint64{1, 2}}, Member: true},
		{Channel: proto.Channel{ID: 20, Kind: proto.ChanDM, Members: []uint64{1, 2}}, Display: "bob", Member: true},
	}
}
func (f *fakeSource) Users() []proto.User {
	return []proto.User{{ID: 1, Name: "alice"}, {ID: 2, Name: "bob"}, {ID: 3, Name: "ci-bot", Bot: true}}
}
func (f *fakeSource) Groups() []proto.Group                 { return nil }
func (f *fakeSource) Highlights() []string                  { return nil }
func (f *fakeSource) InMyThread(proto.Message) bool         { return false }
func (f *fakeSource) Messages(uint64, int) []client.Message { return nil }
func (f *fakeSource) Thread(uint64) []client.Message        { return nil }
func (f *fakeSource) NotifyPrefs() client.NotifyPrefs {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prefs.Chans == nil {
		return alert.DefaultPrefs()
	}
	return f.prefs
}

type fakeOut struct {
	mu    sync.Mutex
	notes []notify.Note
}

func (o *fakeOut) Send(n notify.Note) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.notes = append(o.notes, n)
	return true
}

func (o *fakeOut) n() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.notes)
}

func (o *fakeOut) last() notify.Note {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.notes[len(o.notes)-1]
}

func dm(id uint64, body string) client.Update {
	return client.Update{New: []client.Message{{Message: proto.Message{ID: id, Chan: 20, Author: 2, Body: body}}}}
}

// While a chat window holds the UI lock the background is silent, a window
// left open when the chat window opened never shows, and once the window
// closes (crash included: the lock is the OS's) it notifies again.
func TestLockHandover(t *testing.T) {
	home := t.TempDir()
	probe, err := NewUIProbe(home)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	out := &fakeOut{}
	c := NewCore(&fakeSource{}, out, alert.Env{}, probe.Open, clock)

	c.Handle(dm(1, "one"))
	if out.n() != 1 {
		t.Fatalf("closed window: %d notifications, want 1", out.n())
	}
	// A second DM opens a window (counted, shown when it closes)...
	now = now.Add(time.Second)
	ts := c.Handle(dm(2, "two"))
	if out.n() != 1 || len(ts) != 0 || c.Eng.Notes[20] == nil || c.Eng.Notes[20].N != 1 {
		t.Fatalf("second DM: %d notes, timers %v, pending %+v", out.n(), ts, c.Eng.Notes[20])
	}
	gen := c.Eng.Notes[20].Gen

	// ...but the chat window opens before it closes.
	release, err := HoldUI(home)
	if err != nil {
		t.Fatal(err)
	}
	if !UIOpen(home) {
		t.Fatal("UIOpen false while held")
	}
	now = now.Add(10 * time.Second)
	c.Fire(alert.Timer{Kind: alert.FlushTimer, Ch: 20, Gen: gen})
	c.Handle(dm(3, "three"))
	c.Handle(client.Update{HuddleStarted: []client.HuddleStart{{Chan: 20, By: 2}}})
	if out.n() != 1 {
		t.Fatalf("chat window open: %d notifications, want still 1", out.n())
	}

	// A second window, then both close.
	release2, err := HoldUI(home)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if !UIOpen(home) {
		t.Fatal("one of two windows closed: should still count as open")
	}
	release2()
	if UIOpen(home) {
		t.Fatal("UIOpen true after every window closed")
	}
	now = now.Add(10 * time.Second)
	c.Handle(dm(4, "four"))
	if out.n() != 2 || out.last().Body != "four" {
		t.Fatalf("after the window closed: %d notes, last %+v", out.n(), out.last())
	}
}

// The gate is checked right before every notification: a window that
// closes after the chat window opened shows nothing.
func TestGateChecksEverySend(t *testing.T) {
	open := false
	out := &fakeOut{}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	c := NewCore(&fakeSource{}, out, alert.Env{}, func() bool { return open }, func() time.Time { return now })
	c.Handle(dm(1, "one"))
	now = now.Add(time.Second)
	c.Handle(dm(2, "two"))
	open = true
	// Straight into the engine, as a timer that slipped past Fire's check.
	c.Eng.Flush(20, c.Eng.Notes[20].Gen)
	if out.n() != 1 {
		t.Fatalf("%d notifications while the window is open", out.n())
	}
}

func TestQuietHoursAndDND(t *testing.T) {
	now := time.Date(2026, 10, 22, 23, 30, 0, 0, time.Local)
	src := &fakeSource{}
	out := &fakeOut{}
	c := NewCore(src, out, alert.Env{}, func() bool { return false }, func() time.Time { return now })
	set := func(f func(p *client.NotifyPrefs)) {
		p := alert.DefaultPrefs()
		f(&p)
		src.mu.Lock()
		src.prefs = p
		src.mu.Unlock()
	}
	id := uint64(0)
	send := func() int {
		id++
		now = now.Add(time.Minute) // past every window and cap
		for ch := range c.Eng.Notes {
			c.Fire(alert.Timer{Kind: alert.FlushTimer, Ch: ch, Gen: c.Eng.Notes[ch].Gen})
		}
		before := out.n()
		c.Handle(dm(id, fmt.Sprint("msg ", id)))
		return out.n() - before
	}
	set(func(p *client.NotifyPrefs) { p.Quiet = "22:00-08:00" })
	if n := send(); n != 0 {
		t.Fatalf("quiet hours: %d notifications", n)
	}
	// The env override (TC_QUIET=off) wins over the stored setting.
	c.env = alert.Env{Quiet: "off"}
	if n := send(); n != 1 {
		t.Fatalf("TC_QUIET=off: %d notifications", n)
	}
	c.env = alert.Env{}
	set(func(p *client.NotifyPrefs) { p.DNDUntil = now.Add(time.Hour).UnixMilli() })
	if n := send(); n != 0 {
		t.Fatalf("dnd: %d notifications", n)
	}
	// A ring is silenced by DND too.
	c.Handle(client.Update{HuddleStarted: []client.HuddleStart{{Chan: 20, By: 2}}})
	if c.Eng.Ring.Ch != 0 {
		t.Fatal("rang during dnd")
	}
	set(func(p *client.NotifyPrefs) { p.DNDUntil = now.Add(-time.Minute).UnixMilli() })
	if n := send(); n != 1 {
		t.Fatalf("dnd over: %d notifications", n)
	}
	set(func(p *client.NotifyPrefs) { p.Content = "none" })
	send()
	if n := out.last(); n.Title != "tc" || n.Body != "new message" {
		t.Fatalf("content none: %+v", n)
	}
	// Bots never notify.
	before := out.n()
	now = now.Add(time.Minute)
	c.Handle(client.Update{New: []client.Message{{Message: proto.Message{ID: 999, Chan: 20, Author: 3, Body: "@alice build failed"}}}})
	if out.n() != before {
		t.Fatal("a bot notified")
	}
}
