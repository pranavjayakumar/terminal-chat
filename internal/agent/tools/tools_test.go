package tools

import (
	"testing"
	"time"
)

func TestParseWhen(t *testing.T) {
	loc := time.FixedZone("IST", 5*3600+1800)
	now := time.Date(2026, 10, 24, 15, 0, 0, 0, loc) // a Saturday
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, loc) }
	cases := map[string]time.Time{
		"in 20m":           now.Add(20 * time.Minute),
		"in 2 hours":       now.Add(2 * time.Hour),
		"in 3d":            at(27, 15, 0),
		"17:30":            at(24, 17, 30),
		"9am":              at(25, 9, 0), // passed today: tomorrow
		"5pm":              at(24, 17, 0),
		"12am":             at(25, 0, 0),
		"noon":             at(25, 12, 0),
		"tomorrow 9am":     at(25, 9, 0),
		"Tomorrow  08:15":  at(25, 8, 15),
		"monday 10:00":     at(26, 10, 0),
		"sat 9am":          at(31, 9, 0), // today's weekday: next week
		"2026-10-25 09:00": at(25, 9, 0),
	}
	for in, want := range cases {
		got, err := ParseWhen(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "someday", "in 0m", "25:00", "13pm", "9", "today 9am", "blursday 9am"} {
		if got, err := ParseWhen(bad, now); err == nil {
			t.Errorf("%q = %v, want an error", bad, got)
		}
	}
}

func TestNextAndRepeat(t *testing.T) {
	fri := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	if n := Next(fri, Weekdays); n.Weekday() != time.Monday || n.Hour() != 9 {
		t.Fatalf("weekdays after friday: %v", n)
	}
	if n := Next(fri, Daily); n.Day() != 24 {
		t.Fatalf("daily: %v", n)
	}
	if !Next(fri, Once).IsZero() {
		t.Fatal("once repeats")
	}
	for in, want := range map[string]Repeat{"": Once, "Daily": Daily, "every weekday": Weekdays} {
		if got, err := ParseRepeat(in); err != nil || got != want {
			t.Errorf("%q = %v %v", in, got, err)
		}
	}
	if _, err := ParseRepeat("hourly"); err == nil {
		t.Fatal("hourly accepted")
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"":           now.Add(-24 * time.Hour),
		"2h":         now.Add(-2 * time.Hour),
		"3 days ago": now.AddDate(0, 0, -3),
		"today":      time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC),
		"yesterday":  time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC),
		"2026-10-20": time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
		"9am":        time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC),
		"5pm":        time.Date(2026, 10, 23, 17, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := ParseSince(in, now, 24*time.Hour)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseSince("whenever", now, time.Hour); err == nil {
		t.Fatal("nonsense accepted")
	}
}

func TestJobsStore(t *testing.T) {
	dir := t.TempDir()
	if jobs, err := LoadJobs(dir); err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
	for i, at := range []int64{300, 100, 200} {
		j := Job{ID: NewID(), Kind: KindSchedule, Text: string(rune('a' + i)), At: at}
		if err := UpdateJobs(dir, func(js []Job) ([]Job, error) { return append(js, j), nil }); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ := LoadJobs(dir)
	if len(jobs) != 3 || jobs[0].At != 100 || jobs[2].At != 300 {
		t.Fatalf("not soonest first: %+v", jobs)
	}
	if _, ok, err := CancelJob(dir, jobs[1].ID); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, ok, _ := CancelJob(dir, "nope"); ok {
		t.Fatal("cancelled a missing job")
	}
	if jobs, _ = LoadJobs(dir); len(jobs) != 2 {
		t.Fatalf("%+v", jobs)
	}
}
