package client

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/skailama/tc/internal/proto"
)

// Reminders (PROTOCOL.md §5). The server keeps them and posts the text into
// my private notes when due, whether or not a client is running; the client
// only turns what I typed into an absolute time and manages the list.

// maxAhead is how far ahead a reminder may be set here: a day short of the
// server's limit, so a clock a little ahead of the server's still passes.
const maxAhead = 365 * 24 * time.Hour

// ErrWhen is a /remind time the parser couldn't read.
var ErrWhen = errors.New(`when? try 10m, 2h, 1d, 15:30, 9am or "tomorrow 9am"`)

// ParseRemind splits the arguments of /remind into when it is due and what
// to remind about. The time is one of
//
//	10m  2h  1d  1w  1h30m   after that long from now
//	15:30  9:05  9am  9:30pm  the next time the wall clock shows that
//	                          (today, or tomorrow if that time has passed)
//	tomorrow 9am  tomorrow 15:30  that time tomorrow
//
// optionally preceded by "in" or "at". now's location is the local zone.
// Clock times are wall-clock times in that zone and ignore daylight saving
// except as Go's time.Date resolves it: "tomorrow 9am" is 9:00 on the next
// calendar day even if that day is 23 or 25 hours long.
func ParseRemind(args string, now time.Time) (at time.Time, text string, err error) {
	rest := strings.TrimSpace(args)
	tok, after := nextToken(rest)
	if l := strings.ToLower(tok); l == "in" || l == "at" {
		if t2, _ := nextToken(after); t2 != "" {
			tok, after = nextToken(after)
		}
	}
	lt := strings.ToLower(tok)
	switch {
	case tok == "":
		return time.Time{}, "", ErrWhen
	case lt == "tomorrow":
		clock, after2 := nextToken(after)
		h, m, ok := parseClock(clock)
		if !ok {
			return time.Time{}, "", errors.New(`"tomorrow" needs a time, e.g. tomorrow 9am`)
		}
		y, mo, d := now.Date()
		at, after = time.Date(y, mo, d+1, h, m, 0, 0, now.Location()), after2
	default:
		if d, ok := parseDuration(lt); ok {
			if d <= 0 {
				return time.Time{}, "", errors.New("that isn't in the future")
			}
			if d > maxAhead {
				return time.Time{}, "", errors.New("reminders can be set up to a year ahead")
			}
			at = now.Add(d)
			break
		}
		if h, m, ok := parseClock(lt); ok {
			y, mo, d := now.Date()
			at = time.Date(y, mo, d, h, m, 0, 0, now.Location())
			if !at.After(now) {
				at = time.Date(y, mo, d+1, h, m, 0, 0, now.Location())
			}
			break
		}
		return time.Time{}, "", ErrWhen
	}
	text = strings.TrimSpace(after)
	if text == "" {
		return time.Time{}, "", errors.New("remind you about what? /remind <when> <text>")
	}
	if len(text) > proto.MaxRemindText {
		return time.Time{}, "", errors.New("reminder text is over 4 KiB")
	}
	if !at.After(now) || at.Sub(now) > maxAhead {
		return time.Time{}, "", errors.New("reminders can be set up to a year ahead")
	}
	return at, text, nil
}

// nextToken splits off the first whitespace-delimited word.
func nextToken(s string) (tok, rest string) {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	i := strings.IndexFunc(s, unicode.IsSpace)
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

// parseDuration reads "10m", "2h", "1d", "1w" and sums like "1h30m". Seconds
// are not accepted: a reminder is for people, and "10s" is more likely a typo.
func parseDuration(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	var total time.Duration
	for s != "" {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 || i > 6 || i == len(s) {
			return 0, false
		}
		n, _ := strconv.Atoi(s[:i])
		var unit time.Duration
		switch s[i] {
		case 'm':
			unit = time.Minute
		case 'h':
			unit = time.Hour
		case 'd':
			unit = 24 * time.Hour
		case 'w':
			unit = 7 * 24 * time.Hour
		default:
			return 0, false
		}
		total += time.Duration(n) * unit
		s = s[i+1:]
	}
	return total, true
}

// parseClock reads "15:30", "9:05", "9am", "9:30pm", "12am" (midnight) and
// "12pm" (noon).
func parseClock(s string) (h, m int, ok bool) {
	s = strings.ToLower(s)
	meridiem := ""
	for _, suf := range []string{"am", "pm"} {
		if strings.HasSuffix(s, suf) {
			meridiem, s = suf, strings.TrimSuffix(s, suf)
		}
	}
	hs, ms, colon := strings.Cut(s, ":")
	if !isDigits(hs, 1, 2) || (colon && !isDigits(ms, 2, 2)) {
		return 0, 0, false
	}
	h, _ = strconv.Atoi(hs)
	if colon {
		m, _ = strconv.Atoi(ms)
	} else if meridiem == "" {
		return 0, 0, false // a bare number is not a time
	}
	if m > 59 {
		return 0, 0, false
	}
	switch meridiem {
	case "":
		if h > 23 {
			return 0, 0, false
		}
	default:
		if h < 1 || h > 12 {
			return 0, 0, false
		}
		h %= 12
		if meridiem == "pm" {
			h += 12
		}
	}
	return h, m, true
}

func isDigits(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Remind asks the server to post text in my notes at at (optionally noting
// the channel ch it was set in). It returns the reminder's id.
func (c *Client) Remind(ctx context.Context, at time.Time, text string, ch uint64) (uint64, error) {
	// The server keeps reminder text in the clear; it mustn't be about an
	// encrypted conversation.
	if ch != 0 && c.needsEnc(ch) {
		return 0, ErrRemindEncrypted
	}
	if err := c.requireCap(proto.CapRemind, "reminders"); err != nil {
		return 0, err
	}
	var r proto.RemindCreateRes
	err := c.call(ctx, proto.OpRemindCreate, proto.RemindCreateArgs{At: at.UnixMilli(), Text: text, Chan: ch}, &r, nil)
	return r.ID, err
}

// ErrRemindEncrypted refuses a reminder that points into an encrypted channel.
var ErrRemindEncrypted = errors.New("reminders can't point into encrypted channels")

// Reminders lists my pending reminders, soonest first.
func (c *Client) Reminders(ctx context.Context) ([]proto.Reminder, error) {
	if err := c.requireCap(proto.CapRemind, "reminders"); err != nil {
		return nil, err
	}
	var r proto.RemindListRes
	err := c.call(ctx, proto.OpRemindList, proto.Empty{}, &r, nil)
	return r.Reminders, err
}

// DeleteReminder cancels one of my pending reminders.
func (c *Client) DeleteReminder(ctx context.Context, id uint64) error {
	if err := c.requireCap(proto.CapRemind, "reminders"); err != nil {
		return err
	}
	return c.call(ctx, proto.OpRemindDelete, proto.RemindDeleteArgs{ID: id}, nil, nil)
}

// IsReminder reports whether m is a delivered reminder: a message of mine in
// my private notes (a DM with only me) that starts with the reminder prefix.
// Such a message is my own, which notifications otherwise skip; a reminder
// is meant to get my attention. The check is only what the server does, so
// typing "⏰ x" into your notes from another device counts too.
func IsReminder(m proto.Message, me uint64, ch proto.Channel) bool {
	return m.Author == me && !m.Deleted && ch.Kind == proto.ChanDM &&
		len(ch.Members) == 1 && ch.Members[0] == me && strings.HasPrefix(m.Body, proto.RemindPrefix)
}

// FormatReminderTime is how /reminders shows a due time.
func FormatReminderTime(at int64, now time.Time) string {
	t := time.UnixMilli(at).In(now.Location())
	y, m, d := now.Date()
	switch {
	case sameDay(t, y, m, d):
		return t.Format("15:04")
	case sameDay(t, y, m, d+1):
		return "tomorrow " + t.Format("15:04")
	case t.Year() == y:
		return t.Format("Jan 2 15:04")
	}
	return t.Format("2006-01-02 15:04")
}

func sameDay(t time.Time, y int, m time.Month, d int) bool {
	ty, tm, td := t.Date()
	ny, nm, nd := time.Date(y, m, d, 0, 0, 0, 0, t.Location()).Date()
	return ty == ny && tm == nm && td == nd
}

// unreadReminders counts delivered reminders in my notes ch after my read
// marker.
func (c *Client) unreadReminders(ch, me uint64) int {
	var n int
	_ = c.db.QueryRow(`SELECT COUNT(*) FROM msgs m LEFT JOIN reads r ON r.chan = m.chan
		WHERE m.chan = ? AND m.author = ? AND m.deleted = 0 AND m.id > COALESCE(r.seq, 0)
		AND substr(m.body, 1, ?) = ?`, ch, me, utf8.RuneCountInString(proto.RemindPrefix), proto.RemindPrefix).Scan(&n)
	return n
}

// ParseClock is parseClock for the UI (/dnd until 9am).
func ParseClock(s string) (h, m int, ok bool) { return parseClock(s) }
