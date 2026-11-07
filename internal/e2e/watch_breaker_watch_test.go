package e2e

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"
)

// Breaker tests for pane sharing (#133, PROTOCOL.md §14.5).

// TestWatch_BreakConcurrentStartShare: two StartShare calls at once (the
// TUI runs /pair as a tea.Cmd with no in-flight guard, so /pair twice
// quickly does this) must leave exactly one share loop. Both calls run
// stopShare before either has set p.pane.share, so neither waits for or
// cancels the other's loop: two shareLoops run at once on the same
// p.pane.enc/seq/upd (a data race and, in an encrypted huddle, nonce
// reuse), and StopShare cancels only the last one: the orphan keeps
// capturing the pane for as long as anyone watches.
func TestWatch_BreakConcurrentStartShare(t *testing.T) {
	us := huddlers(t, "quic", "ana", "bo")
	ana, bo := us[0], us[1]
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	gen := ana.channel("general")
	joinAll(t, ctx, gen, ana, bo)

	first := &fakePane{}
	first.set("first\r\n")
	if err := ana.StartShare(ctx, "first", first.capture); err != nil {
		t.Fatal(err)
	}
	eventually(t, "live", func() bool { _, ok := bo.LiveIn(gen); return ok })
	if err := bo.Watch(ctx, ana.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first picture", func() bool { return strings.Contains(watchText(bo), "first") })

	// /pair twice, quickly.
	pa, pb := &fakePane{}, &fakePane{}
	pa.set("pane A\r\n")
	pb.set("pane B\r\n")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, p := range []*fakePane{pa, pb} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = ana.StartShare(ctx, fmt.Sprint("pane", i), p.capture)
		}()
	}
	wg.Wait()
	t.Logf("StartShare errors: %v", errs)
	// Let both loops (if two run) capture a few times; pane content keeps
	// changing so they also encode and send.
	for i := range 10 {
		pa.set(fmt.Sprintf("pane A %d\r\n", i))
		pb.set(fmt.Sprintf("pane B %d\r\n", i))
		time.Sleep(50 * time.Millisecond)
	}
	info, _ := ana.Sharing()
	t.Logf("captures before stop: A %d B %d; sharing %q", pa.captures(), pb.captures(), info.Label)
	ana.StopShare()
	if _, ok := ana.Sharing(); ok {
		t.Fatal("still sharing after StopShare")
	}
	time.Sleep(300 * time.Millisecond) // anything already in flight settles
	na, nb := pa.captures(), pb.captures()
	time.Sleep(1500 * time.Millisecond)
	if da, db := pa.captures()-na, pb.captures()-nb; da != 0 || db != 0 {
		t.Fatalf("a share loop outlived StopShare: pane A captured %d more times, pane B %d (bo still watches)", da, db)
	}
}

// TestWatch_BreakRewatchWaitsForRefresh: a viewer that closes the viewer
// and opens it again (StopWatch, Watch) starts from a blank view and
// needs a keyframe, but the streamer only forces one when its watcher
// count went up since its last capture (shareLoop: force := w > seen;
// paneRoster: more := watchers > p.pane.watchers). Both rosters (watch
// off, watch on) arrive between two ticks of a static pane's 1 s loop,
// so the loop sees 1 watcher before and after, sends nothing, and the
// viewer stares at a blank pane until the 5 s refresh.
func TestWatch_BreakRewatchWaitsForRefresh(t *testing.T) {
	us := huddlers(t, "quic", "ana", "bo")
	ana, bo := us[0], us[1]
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	gen := ana.channel("general")
	joinAll(t, ctx, gen, ana, bo)
	pane := &fakePane{}
	pane.set("static pane\r\n")
	if err := ana.StartShare(ctx, "p", pane.capture); err != nil {
		t.Fatal(err)
	}
	eventually(t, "live", func() bool { _, ok := bo.LiveIn(gen); return ok })
	if err := bo.Watch(ctx, ana.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first picture", func() bool { return strings.Contains(watchText(bo), "static pane") })
	time.Sleep(2500 * time.Millisecond) // the static pane's loop backs off to 1 s
	bo.StopWatch()
	if err := bo.Watch(ctx, ana.Me().ID); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for !strings.Contains(watchText(bo), "static pane") {
		if time.Since(start) > 7*time.Second {
			t.Fatal("no picture 7 s after watching again")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("watching again showed a blank pane for %v (want a keyframe at once)", d.Round(10*time.Millisecond))
	}
}

// TestWatch_BreakRestartWhileEncoding: StopShare cancels the share loop
// but nobody waits for it, and StartShare only waits for a loop it stops
// itself (stopShare returns nil once StopShare already took the share).
// Esc then /pair therefore starts a second loop while the first may still
// be in Parse/Scrub/Encode (writing p.pane.enc) or between its ctx check
// and sealScreen (p.pane.seq++): the encoder races, and in an encrypted
// huddle two loops can seal different chunks under the same seq (nonce).
// Run under -race.
func TestWatch_BreakRestartWhileEncoding(t *testing.T) {
	us := huddlers(t, "quic", "ana", "bo")
	ana, bo := us[0], us[1]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	gen := ana.channel("general")
	joinAll(t, ctx, gen, ana, bo)

	// A busy pane: every capture a new picture, so the loop runs Parse,
	// Scrub and Encode on every 100 ms tick.
	busy := func(context.Context) (string, error) {
		var b strings.Builder
		for range 60 {
			for range 160 {
				b.WriteByte(byte('!' + rand.IntN(90)))
			}
			b.WriteString("\r\n")
		}
		return b.String(), nil
	}
	if err := ana.StartShare(ctx, "busy", busy); err != nil {
		t.Fatal(err)
	}
	eventually(t, "live", func() bool { _, ok := bo.LiveIn(gen); return ok })
	if err := bo.Watch(ctx, ana.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "watcher counted", func() bool { i, _ := ana.Sharing(); return i.Watchers == 1 })
	for i := range 120 {
		time.Sleep(time.Duration(rand.IntN(60)) * time.Millisecond)
		ana.StopShare()                          // Esc
		err := ana.StartShare(ctx, "busy", busy) // /pair
		for err != nil && strings.Contains(err.Error(), "rate_limited") {
			// huddle.share on is a write op (§9): 60 burst, 5 a second.
			time.Sleep(250 * time.Millisecond)
			err = ana.StartShare(ctx, "busy", busy)
		}
		if err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
	}
	ana.StopShare()
}
