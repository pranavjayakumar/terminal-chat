package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

func at(loc *time.Location, y int, mo time.Month, d, h, m, s int) time.Time {
	return time.Date(y, mo, d, h, m, s, 0, loc)
}

func TestParseRemindDurations(t *testing.T) {
	loc := time.FixedZone("X", 5*3600+1800)
	now := at(loc, 2026, 10, 10, 14, 20, 30)
	for _, tc := range []struct {
		in   string
		want time.Duration
		text string
	}{
		{"10m call mom", 10 * time.Minute, "call mom"},
		{"2h stand up", 2 * time.Hour, "stand up"},
		{"1d renew cert", 24 * time.Hour, "renew cert"},
		{"1w review", 7 * 24 * time.Hour, "review"},
		{"1h30m lunch", 90 * time.Minute, "lunch"},
		{"in 5m tea", 5 * time.Minute, "tea"},
		{"IN 5M tea", 5 * time.Minute, "tea"},
		{"  15m   extra   spaces  kept inside  ", 15 * time.Minute, "extra   spaces  kept inside"},
		{"10m 5pm meeting", 10 * time.Minute, "5pm meeting"},
		{"365d far", 365 * 24 * time.Hour, "far"},
	} {
		got, text, err := ParseRemind(tc.in, now)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if !got.Equal(now.Add(tc.want)) || text != tc.text {
			t.Errorf("%q = %v %q, want +%v %q", tc.in, got, text, tc.want, tc.text)
		}
	}
}

func TestParseRemindClockTimes(t *testing.T) {
	loc := time.FixedZone("X", -7*3600)
	now := at(loc, 2026, 10, 10, 14, 20, 30)
	for _, tc := range []struct {
		in   string
		want time.Time
	}{
		{"15:30 x", at(loc, 2026, 10, 10, 15, 30, 0)},
		{"15:30:00 x", time.Time{}},                // seconds aren't supported
		{"9:05 x", at(loc, 2026, 10, 11, 9, 5, 0)}, // passed: tomorrow
		{"09:05 x", at(loc, 2026, 10, 11, 9, 5, 0)},
		{"2pm x", at(loc, 2026, 10, 11, 14, 0, 0)}, // 14:00 < 14:20
		{"3pm x", at(loc, 2026, 10, 10, 15, 0, 0)},
		{"3PM x", at(loc, 2026, 10, 10, 15, 0, 0)},
		{"9am x", at(loc, 2026, 10, 11, 9, 0, 0)},
		{"9:30pm x", at(loc, 2026, 10, 10, 21, 30, 0)},
		{"12am x", at(loc, 2026, 10, 11, 0, 0, 0)}, // midnight
		{"12pm x", at(loc, 2026, 10, 11, 12, 0, 0)},
		{"12:15pm x", at(loc, 2026, 10, 11, 12, 15, 0)},
		{"12:30am x", at(loc, 2026, 10, 11, 0, 30, 0)},
		{"at 23:59 x", at(loc, 2026, 10, 10, 23, 59, 0)},
		{"00:00 x", at(loc, 2026, 10, 11, 0, 0, 0)},
		{"tomorrow 9am x", at(loc, 2026, 10, 11, 9, 0, 0)},
		{"Tomorrow 15:30 x", at(loc, 2026, 10, 11, 15, 30, 0)},
		{"tomorrow 12am x", at(loc, 2026, 10, 11, 0, 0, 0)},
		{"tomorrow 00:00 x", at(loc, 2026, 10, 11, 0, 0, 0)},
		{"tomorrow 1am x", at(loc, 2026, 10, 11, 1, 0, 0)},
	} {
		got, text, err := ParseRemind(tc.in, now)
		if tc.want.IsZero() {
			if err == nil {
				t.Errorf("%q accepted: %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) || text != "x" {
			t.Errorf("%q = %v %q, want %v", tc.in, got, text, tc.want)
		}
		if !got.After(now) {
			t.Errorf("%q = %v is not in the future", tc.in, got)
		}
	}
}

// Around midnight: "today" and "tomorrow" follow the calendar date of now,
// in now's own zone, never UTC's.
func TestParseRemindAroundMidnight(t *testing.T) {
	loc := time.FixedZone("X", 9*3600)

	late := at(loc, 2026, 12, 31, 23, 59, 59)
	for _, tc := range []struct {
		in   string
		want time.Time
	}{
		{"00:05 x", at(loc, 2027, 1, 1, 0, 5, 0)},         // 6 minutes away, next year
		{"11:59pm x", at(loc, 2027, 1, 1, 23, 59, 0)},     // passed one second ago
		{"23:59 x", at(loc, 2027, 1, 1, 23, 59, 0)},       // the same: strictly after now
		{"tomorrow 9am x", at(loc, 2027, 1, 1, 9, 0, 0)},  // year rollover
		{"1m x", late.Add(time.Minute)},                   // crosses midnight
		{"12am x", at(loc, 2027, 1, 1, 0, 0, 0)},          // a second away
		{"tomorrow 12am x", at(loc, 2027, 1, 1, 0, 0, 0)}, // also tomorrow's midnight
	} {
		got, _, err := ParseRemind(tc.in, late)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("late %q = %v, %v, want %v", tc.in, got, err, tc.want)
		}
	}

	early := at(loc, 2026, 2, 28, 0, 0, 1) // just after midnight, a non-leap year
	for _, tc := range []struct {
		in   string
		want time.Time
	}{
		{"00:00 x", at(loc, 2026, 3, 1, 0, 0, 0)}, // passed one second ago: tomorrow
		{"tomorrow 9am x", at(loc, 2026, 3, 1, 9, 0, 0)},
		{"12:01am x", at(loc, 2026, 2, 28, 0, 1, 0)},
	} {
		got, _, err := ParseRemind(tc.in, early)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("early %q = %v, %v, want %v", tc.in, got, err, tc.want)
		}
	}

	// Exactly now is not in the future.
	exact := at(loc, 2026, 10, 10, 15, 30, 0)
	got, _, err := ParseRemind("15:30 x", exact)
	if err != nil || !got.Equal(at(loc, 2026, 10, 11, 15, 30, 0)) {
		t.Errorf("exact now = %v, %v", got, err)
	}

	// Leap day: tomorrow 9am from Feb 28 2028 is Feb 29.
	leap := at(loc, 2028, 2, 28, 22, 0, 0)
	got, _, _ = ParseRemind("tomorrow 9am x", leap)
	if !got.Equal(at(loc, 2028, 2, 29, 9, 0, 0)) {
		t.Errorf("leap day = %v", got)
	}
}

// Clock times are wall-clock times of the client's zone. On a day with a
// daylight-saving change "tomorrow 9am" is still 9:00 local, not 24 hours
// from now.
func TestParseRemindDSTIsWallClock(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	// US clocks spring forward on 2026-03-08 and fall back on 2026-11-01.
	for _, tc := range []struct {
		name         string
		now          time.Time
		in           string
		wantH, wantM int
		wantDay      int
		elapsed      time.Duration
	}{
		{"spring forward day is 23h", at(ny, 2026, 3, 7, 9, 0, 0), "tomorrow 9am x", 9, 0, 8, 23 * time.Hour},
		{"fall back day is 25h", at(ny, 2026, 10, 31, 9, 0, 0), "tomorrow 9am x", 9, 0, 1, 25 * time.Hour},
		{"today across the change", at(ny, 2026, 3, 8, 0, 30, 0), "9:00 x", 9, 0, 8, 7*time.Hour + 30*time.Minute},
		{"duration is elapsed time", at(ny, 2026, 3, 7, 12, 0, 0), "1d x", 12, 0, 8, 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := ParseRemind(tc.in, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			l := got.In(ny)
			if tc.name == "duration is elapsed time" {
				// 1d is 24 absolute hours: 13:00 local after the lost hour.
				if got.Sub(tc.now) != tc.elapsed || l.Hour() != 13 {
					t.Fatalf("1d = %v (%v later)", l, got.Sub(tc.now))
				}
				return
			}
			if l.Hour() != tc.wantH || l.Minute() != tc.wantM || l.Day() != tc.wantDay || got.Sub(tc.now) != tc.elapsed {
				t.Fatalf("got %v (%v later), want %02d:%02d on day %d (%v later)", l, got.Sub(tc.now), tc.wantH, tc.wantM, tc.wantDay, tc.elapsed)
			}
		})
	}

	// A wall-clock time that doesn't exist (02:30 on the spring-forward
	// night) still yields a future moment, within an hour of where it'd be.
	now := at(ny, 2026, 3, 7, 23, 0, 0)
	got, _, err := ParseRemind("tomorrow 2:30am x", now)
	if err != nil {
		t.Fatal(err)
	}
	if h := got.In(ny).Hour(); !got.After(now) || (h != 1 && h != 3) {
		t.Fatalf("nonexistent time resolved to %v", got.In(ny))
	}
	// A time that happens twice (01:30 on the fall-back night) resolves to
	// one of the two.
	now = at(ny, 2026, 10, 31, 23, 0, 0)
	got, _, err = ParseRemind("tomorrow 1:30am x", now)
	if err != nil || got.In(ny).Hour() != 1 || got.In(ny).Minute() != 30 {
		t.Fatalf("ambiguous time resolved to %v, %v", got.In(ny), err)
	}
}

func TestParseRemindUTCAndLocalAgree(t *testing.T) {
	// The same instant in two zones gives the same absolute time for
	// durations, and different ones for clock times.
	utc := at(time.UTC, 2026, 10, 10, 12, 0, 0)
	tokyo := utc.In(time.FixedZone("JST", 9*3600))
	a, _, _ := ParseRemind("2h x", utc)
	b, _, _ := ParseRemind("2h x", tokyo)
	if !a.Equal(b) {
		t.Fatalf("durations differ: %v %v", a, b)
	}
	a, _, _ = ParseRemind("15:00 x", utc)
	b, _, _ = ParseRemind("15:00 x", tokyo) // 21:00 JST: already past, so tomorrow
	if a.Equal(b) || a.Hour() != 15 || b.Hour() != 15 || !b.After(a) {
		t.Fatalf("clock times: %v %v", a, b)
	}
}

func TestParseRemindErrors(t *testing.T) {
	now := at(time.UTC, 2026, 10, 10, 12, 0, 0)
	for _, in := range []string{
		"", "   ", "soon call", "call mom", "in", "at", "10 call", "0m x", "0h x", "-5m x", "5x x", "5s x", "1h30 x",
		"25:00 x", "24:00 x", "9:60 x", "13pm x", "0am x", "9:5 x", "9:005 x", "123:00 x", "tomorrow", "tomorrow call mom",
		"tomorrow 25:00 x", "366d x", "9999999m x", "1000000h x", ":30 x", "am x", "9am:30 x", "5 pm x",
		"10m", "10m   ", "15:30", "tomorrow 9am", "in 10m", // no text
	} {
		if got, text, err := ParseRemind(in, now); err == nil {
			t.Errorf("%q accepted: %v %q", in, got, text)
		}
	}
	if _, _, err := ParseRemind("10m "+strings.Repeat("x", proto.MaxRemindText+1), now); err == nil {
		t.Error("over-long text accepted")
	}
	if _, _, err := ParseRemind("10m "+strings.Repeat("x", proto.MaxRemindText), now); err != nil {
		t.Errorf("max-length text rejected: %v", err)
	}
	if _, _, err := ParseRemind("soon call", now); err != ErrWhen {
		t.Errorf("unparseable time error = %v", err)
	}
}

func TestParseRemindKeepsTextVerbatim(t *testing.T) {
	now := at(time.UTC, 2026, 10, 10, 12, 0, 0)
	_, text, err := ParseRemind("1h \"quoted\" <b> 10m tomorrow 9am emoji 🎉 ‮", now)
	if err != nil || text != "\"quoted\" <b> 10m tomorrow 9am emoji 🎉 ‮" {
		t.Fatalf("text %q err %v", text, err)
	}
	_, text, _ = ParseRemind("1h line one\nline two", now)
	if text != "line one\nline two" {
		t.Fatalf("multi-line text %q", text)
	}
}

func TestFormatReminderTime(t *testing.T) {
	loc := time.FixedZone("X", 3600)
	now := at(loc, 2026, 10, 10, 12, 0, 0)
	for _, tc := range []struct {
		t    time.Time
		want string
	}{
		{at(loc, 2026, 10, 10, 15, 30, 0), "15:30"},
		{at(loc, 2026, 10, 11, 9, 0, 0), "tomorrow 09:00"},
		{at(loc, 2026, 12, 25, 8, 5, 0), "Dec 25 08:05"},
		{at(loc, 2027, 1, 2, 8, 5, 0), "2027-01-02 08:05"},
		{at(time.UTC, 2026, 10, 10, 23, 30, 0), "00:30"}, // 23:30 UTC is 00:30 the next day in X
	} {
		got := FormatReminderTime(tc.t.UnixMilli(), now)
		want := tc.want
		if tc.t.Location() == time.UTC {
			want = "tomorrow 00:30"
		}
		if got != want {
			t.Errorf("%v = %q, want %q", tc.t, got, want)
		}
	}
}

func TestIsReminder(t *testing.T) {
	notes := proto.Channel{ID: 5, Kind: proto.ChanDM, Members: []uint64{alice}}
	two := proto.Channel{ID: 6, Kind: proto.ChanDM, Members: []uint64{alice, bob}}
	pub := proto.Channel{ID: 7, Kind: proto.ChanPublic, Members: []uint64{alice}}
	rem := proto.Message{Author: alice, Body: "⏰ call mom"}
	for name, tc := range map[string]struct {
		m    proto.Message
		ch   proto.Channel
		want bool
	}{
		"reminder in my notes":    {rem, notes, true},
		"with an origin":          {proto.Message{Author: alice, Body: "⏰ x (from #general)"}, notes, true},
		"no prefix":               {proto.Message{Author: alice, Body: "call mom"}, notes, false},
		"prefix not at the start": {proto.Message{Author: alice, Body: "x ⏰ y"}, notes, false},
		"bare emoji":              {proto.Message{Author: alice, Body: "⏰"}, notes, false},
		"someone else's":          {proto.Message{Author: bob, Body: "⏰ x"}, notes, false},
		"in a two-person DM":      {rem, two, false},
		"in a channel":            {rem, pub, false},
		"deleted":                 {proto.Message{Author: alice, Body: "⏰ x", Deleted: true}, notes, false},
	} {
		if got := IsReminder(tc.m, alice, tc.ch); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestRemindOps(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	fs.caps.Store([]string{proto.CapRemind})
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	due := time.UnixMilli(1_800_000_000_000)
	type created struct {
		id  uint64
		err error
	}
	done := make(chan created, 1)
	go func() {
		id, err := c.Remind(context.Background(), due, "ship it", general)
		done <- created{id, err}
	}()
	var ca proto.RemindCreateArgs
	r := fc.expectReq(proto.OpRemindCreate, &ca)
	if ca.At != due.UnixMilli() || ca.Text != "ship it" || ca.Chan != general {
		t.Fatalf("remind.create %+v", ca)
	}
	fc.ok(r.ID, proto.RemindCreateRes{ID: 42})
	if got := <-done; got.err != nil || got.id != 42 {
		t.Fatalf("Remind = %+v", got)
	}

	list := make(chan []proto.Reminder, 1)
	go func() {
		rs, err := c.Reminders(context.Background())
		if err != nil {
			t.Error(err)
		}
		list <- rs
	}()
	r = fc.expectReq(proto.OpRemindList, nil)
	fc.ok(r.ID, proto.RemindListRes{Reminders: []proto.Reminder{{ID: 42, At: due.UnixMilli(), Text: "ship it"}}})
	if rs := <-list; len(rs) != 1 || rs[0].ID != 42 || rs[0].Text != "ship it" {
		t.Fatalf("Reminders = %+v", rs)
	}

	del := make(chan error, 1)
	go func() { del <- c.DeleteReminder(context.Background(), 42) }()
	var da proto.RemindDeleteArgs
	r = fc.expectReq(proto.OpRemindDelete, &da)
	if da.ID != 42 {
		t.Fatalf("remind.delete %+v", da)
	}
	fc.fail(r.ID, proto.ErrNotFound)
	err := <-del
	if pe, ok := err.(*proto.Error); !ok || pe.Code != proto.ErrNotFound {
		t.Fatalf("DeleteReminder err = %v", err)
	}
}

func TestRemindOpsNeedServerSupportAndConnection(t *testing.T) {
	c, _ := localClient(t)
	if _, err := c.Remind(context.Background(), time.Now().Add(time.Hour), "x", 0); err != ErrOffline {
		t.Fatalf("offline: %v", err)
	}
	if _, err := c.Reminders(context.Background()); err != ErrOffline {
		t.Fatalf("offline list: %v", err)
	}

	testEnv(t)
	fs := newFakeServer(t) // no caps
	c2 := open(t, login(t, fs))
	run(t, c2)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c2))
	if _, err := c2.Remind(context.Background(), time.Now().Add(time.Hour), "x", 0); err == nil || err == ErrOffline {
		t.Fatalf("server without remind: %v", err)
	}
	if err := c2.DeleteReminder(context.Background(), 1); err == nil || err == ErrOffline {
		t.Fatalf("delete on a server without remind: %v", err)
	}
}
