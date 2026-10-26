// Package tools holds the parts of the personal agent's scheduler that
// don't need the supervisor: reading times people say ("in 2h", "tomorrow
// 9am", "weekdays"), and the jobs file in the agent's state folder, which
// the supervisor fires and the TUI panel lists and cancels.
package tools

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Repeat says how a job comes back.
type Repeat string

const (
	Once     Repeat = "once"
	Daily    Repeat = "daily"
	Weekdays Repeat = "weekdays"
)

// ParseRepeat reads a repeat ("" = once).
func ParseRepeat(s string) (Repeat, error) {
	switch r := Repeat(strings.ToLower(strings.TrimSpace(s))); r {
	case "", "none", "no", Once:
		return Once, nil
	case Daily, "every day", "everyday":
		return Daily, nil
	case Weekdays, "weekday", "every weekday", "mon-fri":
		return Weekdays, nil
	}
	return "", fmt.Errorf("repeat %q: use once, daily or weekdays", s)
}

// Next is the time after at that a job repeating r comes back; zero for
// Once.
func Next(at time.Time, r Repeat) time.Time {
	switch r {
	case Daily:
		return at.AddDate(0, 0, 1)
	case Weekdays:
		n := at.AddDate(0, 0, 1)
		for n.Weekday() == time.Saturday || n.Weekday() == time.Sunday {
			n = n.AddDate(0, 0, 1)
		}
		return n
	}
	return time.Time{}
}

var (
	clockRe = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?\s*(am|pm)?$`)
	inRe    = regexp.MustCompile(`^in\s+(\d+)\s*(m|min|mins|minutes?|h|hrs?|hours?|d|days?)$`)
	agoRe   = regexp.MustCompile(`^(\d+)\s*(m|min|mins|minutes?|h|hrs?|hours?|d|days?)$`)
)

var weekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// WhenHelp lists the forms ParseWhen reads.
const WhenHelp = `"in 20m", "in 2h", "in 3d", "17:30", "5pm", "tomorrow 9am", "friday 10:00", "2026-10-25 09:00"`

// ParseWhen reads a time in now's location. A bare clock time is the next
// time it comes round (today, else tomorrow); a weekday is the next one.
func ParseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	if s == "" {
		return time.Time{}, errors.New("when? e.g. " + WhenHelp)
	}
	bad := fmt.Errorf("can't read %q as a time; use one of %s", s, WhenHelp)
	if m := inRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		if n <= 0 {
			return time.Time{}, bad
		}
		var d time.Duration
		switch m[2][0] {
		case 'm':
			d = time.Duration(n) * time.Minute
		case 'h':
			d = time.Duration(n) * time.Hour
		case 'd':
			return now.AddDate(0, 0, n), nil
		}
		return now.Add(d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 3pm", "2006-01-02 3:04pm"} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			return t, nil
		}
	}
	day, rest, ok := strings.Cut(s, " ")
	if !ok {
		day, rest = "", s
	}
	base := now
	switch {
	case day == "":
	case day == "today":
	case day == "tomorrow":
		base = now.AddDate(0, 0, 1)
	default:
		wd, ok := weekdays[day]
		if !ok {
			return time.Time{}, bad
		}
		n := (int(wd) - int(now.Weekday()) + 7) % 7
		if n == 0 {
			n = 7
		}
		base = now.AddDate(0, 0, n)
	}
	h, m, err := clock(rest)
	if err != nil {
		return time.Time{}, bad
	}
	t := time.Date(base.Year(), base.Month(), base.Day(), h, m, 0, 0, now.Location())
	if day == "" && !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	if !t.After(now) {
		return time.Time{}, fmt.Errorf("%s is in the past", t.Format("Mon 2 Jan 15:04"))
	}
	return t, nil
}

func clock(s string) (h, m int, err error) {
	switch s {
	case "noon":
		return 12, 0, nil
	case "midnight":
		return 0, 0, nil
	}
	x := clockRe.FindStringSubmatch(strings.TrimSpace(s))
	if x == nil {
		return 0, 0, errors.New("not a clock time")
	}
	h, _ = strconv.Atoi(x[1])
	if x[2] != "" {
		m, _ = strconv.Atoi(x[2])
	}
	switch x[3] {
	case "am":
		if h < 1 || h > 12 {
			return 0, 0, errors.New("bad hour")
		}
		h %= 12
	case "pm":
		if h < 1 || h > 12 {
			return 0, 0, errors.New("bad hour")
		}
		h = h%12 + 12
	default:
		if x[2] == "" {
			return 0, 0, errors.New("a bare number isn't a time")
		}
	}
	if h > 23 || m > 59 {
		return 0, 0, errors.New("bad time")
	}
	return h, m, nil
}

// ParseSince reads how far back to look: "2h", "30m", "3d", "today",
// "yesterday", "week", a date or a time. "" gives def.
func ParseSince(s string, now time.Time, def time.Duration) (time.Time, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	day := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()) }
	switch s {
	case "":
		return now.Add(-def), nil
	case "today":
		return day(now), nil
	case "yesterday":
		return day(now.AddDate(0, 0, -1)), nil
	case "week", "this week", "last week":
		return now.AddDate(0, 0, -7), nil
	}
	s = strings.TrimPrefix(strings.TrimSuffix(s, " ago"), "last ")
	if m := agoRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch m[2][0] {
		case 'm':
			return now.Add(-time.Duration(n) * time.Minute), nil
		case 'h':
			return now.Add(-time.Duration(n) * time.Hour), nil
		default:
			return now.AddDate(0, 0, -n), nil
		}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			return t, nil
		}
	}
	if h, m, err := clock(s); err == nil {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
		if t.After(now) {
			t = t.AddDate(0, 0, -1)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("can't read since %q: use 2h, 3d, today, yesterday, week or a date", s)
}
