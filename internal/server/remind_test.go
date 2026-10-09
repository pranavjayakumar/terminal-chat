package server

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)} }

// startServerAt starts a server on dir with a fake clock. shutdown stops
// Serve and closes the database, so the same dir can be started again.
func startServerAt(t *testing.T, dir string, clk *fakeClock) (ts *testServer, shutdown func()) {
	t.Helper()
	srv, err := New(Config{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.Listen("127.0.0.1:0", srv.Cert())
	if err != nil {
		t.Fatal(err)
	}
	srv.cfg.PublicAddr = ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.Serve(ctx, ln)
		close(done)
	}()
	var once sync.Once
	shutdown = func() {
		once.Do(func() {
			cancel()
			<-done
			srv.Close()
		})
	}
	t.Cleanup(shutdown)
	return &testServer{Server: srv, addr: ln.Addr().String(), kind: "tcp", stop: shutdown}, shutdown
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (ts *testServer) remindCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := ts.st.db.QueryRow(`SELECT COUNT(*) FROM reminders`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// remindMsgs lists delivered reminder bodies (messages starting with the
// reminder prefix) in id order.
func (ts *testServer) remindMsgs(t *testing.T) []string {
	t.Helper()
	rows, err := ts.st.db.Query(`SELECT body FROM messages WHERE body LIKE ? ORDER BY id`, proto.RemindPrefix+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		rows.Scan(&b)
		out = append(out, b)
	}
	return out
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func TestRemindCapAdvertised(t *testing.T) {
	ts := startServer(t, "tcp")
	c := ts.register(t, "alice", false)
	if !slices.Contains(c.welcome.Caps, proto.CapRemind) {
		t.Fatalf("caps %v lack remind", c.welcome.Caps)
	}
}

func TestRemindCreateValidation(t *testing.T) {
	clk := newClock()
	ts, _ := startServerAt(t, t.TempDir(), clk)
	alice := ts.register(t, "alice", false)
	bob := ts.register(t, "bob", false)
	alice.syncFresh()
	bob.syncFresh()
	var cr proto.ChanRes
	bob.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "secret", Private: true}, &cr)

	at := ms(clk.Now().Add(time.Hour))
	bad := []struct {
		name string
		a    proto.RemindCreateArgs
		code string
	}{
		{"empty text", proto.RemindCreateArgs{At: at, Text: "  "}, proto.ErrBadRequest},
		{"control chars", proto.RemindCreateArgs{At: at, Text: "a\x1b[2Jb"}, proto.ErrBadRequest},
		{"too long", proto.RemindCreateArgs{At: at, Text: strings.Repeat("x", proto.MaxRemindText+1)}, proto.ErrTooLarge},
		{"zero time", proto.RemindCreateArgs{At: 0, Text: "x"}, proto.ErrBadRequest},
		{"negative time", proto.RemindCreateArgs{At: -5, Text: "x"}, proto.ErrBadRequest},
		{"over a year", proto.RemindCreateArgs{At: ms(clk.Now().Add(proto.MaxRemindAhead + time.Hour)), Text: "x"}, proto.ErrBadRequest},
		{"unreadable chan", proto.RemindCreateArgs{At: at, Text: "x", Chan: cr.Chan.ID}, proto.ErrForbidden},
		{"unknown chan", proto.RemindCreateArgs{At: at, Text: "x", Chan: 99999}, proto.ErrForbidden},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) { alice.fail(proto.OpRemindCreate, tc.a, tc.code) })
	}
	if n := ts.remindCount(t); n != 0 {
		t.Fatalf("%d reminders stored after failures", n)
	}

	// Exactly 4 KiB and exactly one year are fine.
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: at, Text: strings.Repeat("x", proto.MaxRemindText)}, nil)
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(clk.Now().Add(proto.MaxRemindAhead)), Text: "far"}, nil)
	// A member may attach a channel they can read.
	bob.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: at, Text: "x", Chan: cr.Chan.ID}, nil)
}

func TestRemindListDeleteIsolation(t *testing.T) {
	clk := newClock()
	ts, _ := startServerAt(t, t.TempDir(), clk)
	alice := ts.register(t, "alice", false)
	bob := ts.register(t, "bob", false)
	alice.syncFresh()
	bob.syncFresh()

	var a2, a1, b1 proto.RemindCreateRes
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(clk.Now().Add(2 * time.Hour)), Text: "later"}, &a2)
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(clk.Now().Add(time.Hour)), Text: "sooner"}, &a1)
	bob.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(clk.Now().Add(time.Hour)), Text: "bobs"}, &b1)
	if a1.ID == 0 || a1.ID == a2.ID || a1.ID == b1.ID {
		t.Fatalf("ids %d %d %d", a1.ID, a2.ID, b1.ID)
	}

	var l proto.RemindListRes
	alice.ok(proto.OpRemindList, struct{}{}, &l)
	if len(l.Reminders) != 2 || l.Reminders[0].ID != a1.ID || l.Reminders[1].ID != a2.ID || l.Reminders[0].Text != "sooner" {
		t.Fatalf("alice's list: %+v", l.Reminders)
	}

	// Someone else's id looks like a missing one and survives.
	bob.fail(proto.OpRemindDelete, proto.RemindDeleteArgs{ID: a1.ID}, proto.ErrNotFound)
	bob.fail(proto.OpRemindDelete, proto.RemindDeleteArgs{ID: 99999}, proto.ErrNotFound)
	alice.ok(proto.OpRemindDelete, proto.RemindDeleteArgs{ID: a1.ID}, nil)
	alice.fail(proto.OpRemindDelete, proto.RemindDeleteArgs{ID: a1.ID}, proto.ErrNotFound)
	alice.ok(proto.OpRemindList, struct{}{}, &l)
	if len(l.Reminders) != 1 || l.Reminders[0].ID != a2.ID {
		t.Fatalf("after delete: %+v", l.Reminders)
	}
	bob.ok(proto.OpRemindList, struct{}{}, &l)
	if len(l.Reminders) != 1 || l.Reminders[0].ID != b1.ID {
		t.Fatalf("bob's list: %+v", l.Reminders)
	}
}

func TestRemindPendingLimit(t *testing.T) {
	clk := newClock()
	ts, _ := startServerAt(t, t.TempDir(), clk)
	alice := ts.register(t, "alice", false)
	bob := ts.register(t, "bob", false)
	alice.syncFresh()
	bob.syncFresh()
	ctx := context.Background()
	at := ms(clk.Now().Add(time.Hour))
	for i := 0; i < proto.MaxReminders; i++ {
		if _, err := ts.opRemindCreate(ctx, alice.user, proto.RemindCreateArgs{At: at, Text: "x"}); err != nil {
			t.Fatalf("reminder %d: %v", i, err)
		}
	}
	alice.fail(proto.OpRemindCreate, proto.RemindCreateArgs{At: at, Text: "x"}, proto.ErrConflict)
	// The limit is per user, and deleting frees a slot.
	bob.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: at, Text: "x"}, nil)
	var l proto.RemindListRes
	alice.ok(proto.OpRemindList, struct{}{}, &l)
	alice.ok(proto.OpRemindDelete, proto.RemindDeleteArgs{ID: l.Reminders[0].ID}, nil)
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: at, Text: "x"}, nil)
}

func TestRemindDeliversAtTimeFakeClock(t *testing.T) {
	clk := newClock()
	ts, _ := startServerAt(t, t.TempDir(), clk)
	alice := ts.register(t, "alice", false)
	alice.syncFresh()
	var notes proto.ChanRes
	alice.ok(proto.OpDMOpen, proto.DMOpenArgs{}, &notes)
	alice.event(proto.KChanNew)

	var id proto.RemindCreateRes
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(clk.Now().Add(10 * time.Minute)), Text: "call mom"}, &id)

	// Not yet due, however often the scheduler looks.
	clk.Advance(9 * time.Minute)
	ts.wakeReminders()
	alice.quiet(150 * time.Millisecond)
	if n := ts.remindCount(t); n != 1 {
		t.Fatalf("%d pending, want 1", n)
	}

	clk.Advance(2 * time.Minute)
	ts.wakeReminders()
	ev := alice.event(proto.KMsgNew)
	var m proto.Message
	proto.Unmarshal(ev.Data, &m)
	if m.Chan != notes.Chan.ID || m.Author != alice.user.ID || m.Body != "⏰ call mom" || m.ID != ev.Seq {
		t.Fatalf("delivered %+v (notes %d)", m, notes.Chan.ID)
	}
	if m.Time != ms(clk.Now()) {
		t.Fatalf("time %d, want the delivery time", m.Time)
	}
	if n := ts.remindCount(t); n != 0 {
		t.Fatalf("%d pending after delivery", n)
	}
	alice.quiet(100 * time.Millisecond)

	// The message is an ordinary one: it is in history and the notes DM.
	var h proto.HistoryRes
	alice.ok(proto.OpHistory, proto.HistoryArgs{Chan: notes.Chan.ID, Limit: 10}, &h)
	if len(h.Msgs) != 1 || h.Msgs[0].Body != "⏰ call mom" {
		t.Fatalf("history %+v", h.Msgs)
	}
}

func TestRemindWakesForNewEarlierReminder(t *testing.T) {
	clk := newClock()
	ts, _ := startServerAt(t, t.TempDir(), clk)
	alice := ts.register(t, "alice", false)
	alice.syncFresh()
	// The scheduler is asleep until the far one; a nearer one must pre-empt it.
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(clk.Now().Add(24 * time.Hour)), Text: "far"}, nil)
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(clk.Now().Add(time.Second)), Text: "near"}, nil)
	clk.Advance(2 * time.Second)
	ts.wakeReminders()
	waitFor(t, "near reminder", func() bool { return len(ts.remindMsgs(t)) == 1 })
	if got := ts.remindMsgs(t)[0]; got != "⏰ near" {
		t.Fatalf("delivered %q", got)
	}
	if n := ts.remindCount(t); n != 1 {
		t.Fatalf("%d pending, want the far one", n)
	}
}

func TestRemindRealTimerFires(t *testing.T) {
	// No fake clock, no wake-up: the scheduler's own timer must fire.
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", false)
	alice.syncFresh()
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: time.Now().Add(300 * time.Millisecond).UnixMilli(), Text: "tick"}, nil)
	ev := alice.event(proto.KMsgNew)
	var m proto.Message
	proto.Unmarshal(ev.Data, &m)
	if m.Body != "⏰ tick" {
		t.Fatalf("body %q", m.Body)
	}
}

func TestRemindCreatesNotesAndMentionsOrigin(t *testing.T) {
	clk := newClock()
	ts, _ := startServerAt(t, t.TempDir(), clk)
	alice := ts.register(t, "alice", false)
	bob := ts.register(t, "bob", true)
	snap, _ := alice.syncFresh()
	bob.syncFresh()
	gen := generalID(t, snap)
	var priv proto.ChanRes
	bob.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "secret", Private: true}, &priv)
	bob.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: priv.Chan.ID, User: alice.user.ID}, nil)
	alice.event(proto.KChanJoin)

	at := ms(clk.Now().Add(time.Minute))
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: at, Text: "in general", Chan: gen}, nil)
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: at + 1, Text: "in secret", Chan: priv.Chan.ID}, nil)
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: at + 2, Text: "plain"}, nil)
	// Alice leaves the private channel before it fires: no name leaks.
	alice.ok(proto.OpChanLeave, proto.ChanArgs{Chan: priv.Chan.ID}, nil)

	clk.Advance(time.Hour)
	ts.wakeReminders()
	waitFor(t, "three reminders", func() bool { return len(ts.remindMsgs(t)) == 3 })
	want := []string{"⏰ in general (from #general)", "⏰ in secret", "⏰ plain"}
	if got := ts.remindMsgs(t); !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	// The notes DM was created by the server, once, with only alice in it.
	var n, members int
	ts.st.db.QueryRow(`SELECT COUNT(*) FROM channels WHERE kind = 'dm'`).Scan(&n)
	ts.st.db.QueryRow(`SELECT COUNT(*) FROM membership WHERE chan IN (SELECT id FROM channels WHERE kind = 'dm')`).Scan(&members)
	if n != 1 || members != 1 {
		t.Fatalf("%d dm channels with %d members", n, members)
	}
	// dm.open with no others finds the same channel.
	var notes proto.ChanRes
	alice.ok(proto.OpDMOpen, proto.DMOpenArgs{}, &notes)
	if len(notes.Chan.Members) != 1 || notes.Chan.Members[0] != alice.user.ID {
		t.Fatalf("notes %+v", notes.Chan)
	}
	var h proto.HistoryRes
	alice.ok(proto.OpHistory, proto.HistoryArgs{Chan: notes.Chan.ID, Limit: 10}, &h)
	if len(h.Msgs) != 3 {
		t.Fatalf("notes history has %d messages", len(h.Msgs))
	}
}

func TestRemindExactlyOnceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	clk := newClock()
	ts, shutdown := startServerAt(t, dir, clk)
	alice := ts.register(t, "alice", false)
	alice.syncFresh()
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(clk.Now().Add(time.Hour)), Text: "once"}, nil)
	shutdown()

	// The server is down when the reminder falls due.
	clk.Advance(2 * time.Hour)
	ts2, shutdown2 := startServerAt(t, dir, clk)
	waitFor(t, "missed reminder", func() bool { return len(ts2.remindMsgs(t)) == 1 })
	shutdown2()

	// Restarting again, and again, never redelivers.
	for i := 0; i < 2; i++ {
		clk.Advance(time.Hour)
		ts3, shutdown3 := startServerAt(t, dir, clk)
		ts3.wakeReminders()
		time.Sleep(100 * time.Millisecond)
		if got := ts3.remindMsgs(t); len(got) != 1 || got[0] != "⏰ once" {
			t.Fatalf("restart %d: %q", i, got)
		}
		shutdown3()
	}
}

func TestRemindDeliveryIsIdempotentUnderRace(t *testing.T) {
	clk := newClock()
	ts, _ := startServerAt(t, t.TempDir(), clk)
	alice := ts.register(t, "alice", false)
	alice.syncFresh()
	var id proto.RemindCreateRes
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(clk.Now().Add(time.Hour)), Text: "race"}, &id)
	clk.Advance(2 * time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ts.deliverReminder(context.Background(), id.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := ts.remindMsgs(t); len(got) != 1 {
		t.Fatalf("delivered %d times: %q", len(got), got)
	}

	// Even if the row came back, the per-reminder nonce blocks a second copy.
	ts.st.db.Exec(`INSERT INTO reminders (id, user, at, text, chan, created) VALUES (?, ?, 1, 'race', 0, 1)`, id.ID, alice.user.ID)
	if err := ts.deliverReminder(context.Background(), id.ID); err != nil {
		t.Fatal(err)
	}
	if got := ts.remindMsgs(t); len(got) != 1 {
		t.Fatalf("redelivered: %q", got)
	}
}

func TestRemindMissedOnStartupForSeveralUsers(t *testing.T) {
	dir := t.TempDir()
	clk := newClock()
	ts, shutdown := startServerAt(t, dir, clk)
	alice := ts.register(t, "alice", false)
	bob := ts.register(t, "bob", false)
	alice.syncFresh()
	bob.syncFresh()
	base := clk.Now()
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(base.Add(time.Minute)), Text: "a1"}, nil)
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(base.Add(3 * time.Minute)), Text: "a2"}, nil)
	bob.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(base.Add(2 * time.Minute)), Text: "b1"}, nil)
	bob.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(base.Add(48 * time.Hour)), Text: "b-future"}, nil)
	shutdown()

	clk.Advance(time.Hour)
	ts2, _ := startServerAt(t, dir, clk)
	waitFor(t, "missed reminders", func() bool { return len(ts2.remindMsgs(t)) == 3 })
	if got := ts2.remindMsgs(t); !slices.Equal(got, []string{"⏰ a1", "⏰ b1", "⏰ a2"}) {
		t.Fatalf("order %q", got)
	}
	if n := ts2.remindCount(t); n != 1 {
		t.Fatalf("%d pending, want the future one", n)
	}

	// A client that connects after the restart sees them in its notes.
	a2, bye := ts2.connect(t, alice.signer, "", "")
	if bye != "" {
		t.Fatal(bye)
	}
	snap, _ := a2.syncFresh()
	found := 0
	for _, m := range snap.Msgs {
		if strings.HasPrefix(m.Body, proto.RemindPrefix) && m.Author == alice.user.ID {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("alice's snapshot has %d reminders, want 2", found)
	}
}

func TestRemindDisabledUserDropped(t *testing.T) {
	clk := newClock()
	ts, _ := startServerAt(t, t.TempDir(), clk)
	admin := ts.register(t, "admin", true)
	bob := ts.register(t, "bob", false)
	admin.syncFresh()
	bob.syncFresh()
	bob.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(clk.Now().Add(time.Minute)), Text: "gone"}, nil)
	admin.ok(proto.OpUserDisable, proto.UserArgs{User: bob.user.ID}, nil)
	clk.Advance(time.Hour)
	ts.wakeReminders()
	waitFor(t, "reminder dropped", func() bool { return ts.remindCount(t) == 0 })
	if got := ts.remindMsgs(t); len(got) != 0 {
		t.Fatalf("delivered to a disabled user: %q", got)
	}
}

func TestRemindSchedulerStopsWithServe(t *testing.T) {
	clk := newClock()
	ts, shutdown := startServerAt(t, t.TempDir(), clk)
	alice := ts.register(t, "alice", false)
	alice.syncFresh()
	alice.ok(proto.OpRemindCreate, proto.RemindCreateArgs{At: ms(clk.Now().Add(time.Hour)), Text: "x"}, nil)
	done := make(chan struct{})
	go func() { shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return: the scheduler outlived its context")
	}
}
