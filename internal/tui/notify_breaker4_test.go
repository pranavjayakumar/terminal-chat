package tui

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/notify"
)

func countCue(m *Model, c notify.Cue) int {
	n := 0
	for _, x := range notes(m).sent() {
		if x.Cue == c {
			n++
		}
	}
	return n
}

// A member who keeps starting and ending a DM huddle (join, leave, join...)
// is a new "start" every time on the client (the roster went empty → non
// empty), and the TUI only arms the 5 min cooldown on Esc or the 30 s
// timeout — a ring stopped because the huddle ended (checkRing →
// stopRing(false)) leaves no cooldown. The risk table and the stopRing
// comment both promise "a join/leave flapper can't spam".
func TestRing_BreakFlapperRingsEveryFlap(t *testing.T) {
	m, h := newHuddleModel(t, 80, 24)
	clk := newClock(m)
	for range 20 {
		startHuddle(m, h, 20, 2)
		clk.add(2 * time.Second)
		h.hmu.Lock()
		h.rosters[20] = nil
		h.hmu.Unlock()
		h.update(m) // the huddle ended: checkRing stops the ring
		clk.add(time.Second)
	}
	// 60 s of flapping: one ring (plus at most its one resend) is the cap.
	if n := countCue(m, notify.CueRing); n > 2 {
		t.Fatalf("a join/leave flapper rang %d times in 60 s (cap: one ring per 5 min)", n)
	}
}

// The coalescing window re-checks the popup at flush ("settings may have
// changed while it waited") but keeps the cue it computed when the window
// opened: /sounds off (or /sounds message off) during a 30 s level-all
// window still plays a sound on the summary.
func TestNotify_BreakSoundsOffMidWindowSummaryStillSounds(t *testing.T) {
	m, _ := newTestModelFake(t, 100, 24)
	clk := newClock(m)
	runInput(t, m, "/notify all #secret")
	deliver(m, newMsg(901, 13, 3, "one"))
	if n := lastNote(t, m); n.Cue != notify.CueMessage {
		t.Fatalf("first: %+v", n)
	}
	gen := m.notes[13].gen
	deliver(m, newMsg(902, 13, 3, "two"), newMsg(903, 13, 3, "three"))
	runInput(t, m, "/sounds off")
	clk.add(allWindow)
	m.Update(notifyFlushMsg{ch: 13, gen: gen})
	s := notes(m).sent()
	if len(s) != 2 {
		t.Fatalf("want the first popup and one summary, got %d", len(s))
	}
	if s[1].Cue != notify.CueNone {
		t.Fatalf("summary after /sounds off still has cue %d: %+v", s[1].Cue, s[1])
	}
}

// "/dnd until HH:MM" is "the next HH:MM after now". On a fall-back day
// in the repeated hour (01:10 EST, the second 01:xx), 01:50 is 40 minutes
// away, but time.Date resolves the ambiguous 01:50 to the first (EDT)
// one, an hour in the past, so untilClock adds a day: DND for 24 h 40 m.
// time.Local is read from TZ at start-up, so this re-runs itself in a
// child with TZ set rather than swapping time.Local under other tests.
func TestDND_BreakUntilInRepeatedHourOnDSTDay(t *testing.T) {
	if os.Getenv("TC_BREAKER4_TZ") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDND_BreakUntilInRepeatedHourOnDSTDay$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "TZ=America/New_York", "TC_BREAKER4_TZ=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s", out)
		}
		if strings.Contains(string(out), "SKIP") {
			t.Skipf("%s", out)
		}
		return
	}
	ny := time.Local
	m, _ := newTestModelFake(t, 80, 24)
	clk := newClock(m)
	// 01:10 EST on Nov 1 2026 = 06:10 UTC (the second 01:10 of the day).
	clk.t = time.Date(2026, 11, 1, 6, 10, 0, 0, time.UTC).In(ny)
	if name, off := clk.t.Zone(); off != -5*3600 || clk.t.Hour() != 1 {
		t.Skipf("no DST data: %s %d %v", name, off, clk.t)
	}
	runInput(t, m, "/dnd until 01:50")
	until := time.UnixMilli(m.nt.prefs.DNDUntil).In(ny)
	if d := until.Sub(clk.t); d > 12*time.Hour || d <= 0 {
		t.Fatalf("/dnd until 01:50 at 01:10 EST ends %s (in %v), not in 40 min", until.Format("Jan 2 15:04 MST"), d)
	}
}

// An incomplete "/dnd for" (or "/notify off for") must be an error, not
// "no duration": at HEAD parseFor drops "for" and returns 0, so "/dnd for"
// silently turns DND *off* and "/notify off for" mutes forever.
func TestDND_BreakDanglingForTurnsDNDOff(t *testing.T) {
	m, _ := newTestModelFake(t, 80, 24)
	newClock(m)
	runInput(t, m, "/dnd 2h")
	runInput(t, m, "/dnd for")
	if !m.nt.prefs.DND(m.now()) {
		t.Fatal(`"/dnd for" turned DND off`)
	}
	if !m.noticeErr {
		t.Fatalf(`"/dnd for" accepted: %q`, m.notice)
	}
}
