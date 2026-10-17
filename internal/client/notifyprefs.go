package client

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// Notification preferences (docs/research/NOTIFICATIONS-2026-10.md §2.2,
// §2.10): local to this device and server, in the cache (never sent
// anywhere), like highlights.

// NotifyLevel is when a channel notifies.
type NotifyLevel string

const (
	LevelDefault  NotifyLevel = ""         // follow the global default
	LevelAll      NotifyLevel = "all"      // every message, and huddle starts
	LevelMentions NotifyLevel = "mentions" // DMs, mentions, highlights, replies, rings (the default)
	LevelOff      NotifyLevel = "off"      // nothing; the sidebar dims it
)

// ParseLevel accepts all, mentions or off.
func ParseLevel(s string) (NotifyLevel, error) {
	switch l := NotifyLevel(strings.ToLower(strings.TrimSpace(s))); l {
	case LevelAll, LevelMentions, LevelOff:
		return l, nil
	}
	return "", fmt.Errorf("level is all, mentions or off (not %q)", s)
}

// ChanNotify is a channel's override; Until (unix ms) ends a timed one
// (0: until changed).
type ChanNotify struct {
	Level NotifyLevel
	Until int64
}

// Sound cues the user can switch off one by one.
var SoundCues = []string{"mention", "message", "ring", "huddle", "join", "mic"}

// NotifyPrefs is everything /notify, /dnd and /sounds set.
type NotifyPrefs struct {
	Chans    map[uint64]ChanNotify
	Default  NotifyLevel // global; "" = mentions
	Content  string      // full (""), name or none
	Quiet    string      // "HH:MM-HH:MM", "" = none
	DNDUntil int64       // unix ms; 0 = off
	SoundOff bool        // master switch
	CueOff   map[string]bool
	Volume   int // in-huddle cue volume 0–100
}

// DefaultCueVolume is the in-huddle cue volume until /sounds volume.
const DefaultCueVolume = 60

// Level is ch's effective level at now.
func (p NotifyPrefs) Level(ch uint64, now time.Time) (lvl NotifyLevel, override bool) {
	if c, ok := p.Chans[ch]; ok && c.Level != LevelDefault && (c.Until == 0 || c.Until > now.UnixMilli()) {
		return c.Level, true
	}
	if p.Default != LevelDefault {
		return p.Default, false
	}
	return LevelMentions, false
}

// Muted reports whether ch is at level off now.
func (p NotifyPrefs) Muted(ch uint64, now time.Time) bool {
	l, _ := p.Level(ch, now)
	return l == LevelOff
}

// Sound reports whether cue may play.
func (p NotifyPrefs) Sound(cue string) bool { return !p.SoundOff && !p.CueOff[cue] }

// ParseQuiet validates "HH:MM-HH:MM" (it may wrap midnight).
func ParseQuiet(s string) (from, to int, err error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return 0, 0, errors.New("quiet hours are HH:MM-HH:MM, e.g. 22:00-08:00")
	}
	if from, err = parseHHMM(a); err == nil {
		to, err = parseHHMM(b)
	}
	if err == nil && from == to {
		err = errors.New("quiet hours need a start and an end that differ")
	}
	return from, to, err
}

// ParseHHMM reads "HH:MM" as minutes after midnight.
func parseHHMM(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || len(m) != 2 {
		return 0, fmt.Errorf("bad time %q (HH:MM)", s)
	}
	return hh*60 + mm, nil
}

// InQuiet reports whether now (local time) falls in the quiet hours.
func (p NotifyPrefs) InQuiet(now time.Time) bool {
	if p.Quiet == "" {
		return false
	}
	from, to, err := ParseQuiet(p.Quiet)
	if err != nil {
		return false
	}
	now = now.Local()
	t := now.Hour()*60 + now.Minute()
	if from < to {
		return t >= from && t < to
	}
	return t >= from || t < to // wraps midnight
}

// DND reports whether do-not-disturb is on at now.
func (p NotifyPrefs) DND(now time.Time) bool { return p.DNDUntil > now.UnixMilli() }

const notifyPrefsSchema = `
CREATE TABLE IF NOT EXISTS notify_prefs (chan INTEGER PRIMARY KEY, level TEXT NOT NULL, until INTEGER NOT NULL DEFAULT 0);
`

// NotifyPrefs reads the preferences.
func (c *Client) NotifyPrefs() NotifyPrefs {
	p := NotifyPrefs{Chans: map[uint64]ChanNotify{}, CueOff: map[string]bool{}, Volume: DefaultCueVolume}
	if rows, err := c.db.Query(`SELECT chan, level, until FROM notify_prefs`); err == nil {
		for rows.Next() {
			var ch uint64
			var cn ChanNotify
			var lvl string
			if rows.Scan(&ch, &lvl, &cn.Until) == nil {
				cn.Level = NotifyLevel(lvl)
				p.Chans[ch] = cn
			}
		}
		rows.Close()
	}
	str := func(k string) string {
		b, _ := metaBytes(c.db, k)
		return string(b)
	}
	if l, err := ParseLevel(str("notify.default")); err == nil {
		p.Default = l
	}
	switch v := str("notify.content"); v {
	case "name", "none":
		p.Content = v
	}
	if q := str("notify.quiet"); q != "" {
		if _, _, err := ParseQuiet(q); err == nil {
			p.Quiet = q
		}
	}
	p.DNDUntil, _ = strconv.ParseInt(str("notify.dnd_until"), 10, 64)
	p.SoundOff = str("sound.all") == "off"
	for _, cue := range SoundCues {
		p.CueOff[cue] = str("sound."+cue) == "off"
	}
	if v, err := strconv.Atoi(str("sound.volume")); err == nil && v >= 0 && v <= 100 {
		p.Volume = v
	}
	return p
}

// SetChanNotify sets ch's level (LevelDefault removes the override);
// until (unix ms) makes it temporary.
func (c *Client) SetChanNotify(ch uint64, lvl NotifyLevel, until int64) error {
	var err error
	if lvl == LevelDefault {
		_, err = c.db.Exec(`DELETE FROM notify_prefs WHERE chan = ?`, ch)
	} else {
		if _, perr := ParseLevel(string(lvl)); perr != nil {
			return perr
		}
		_, err = c.db.Exec(`INSERT INTO notify_prefs (chan, level, until) VALUES (?, ?, ?)
			ON CONFLICT (chan) DO UPDATE SET level = excluded.level, until = excluded.until`, ch, string(lvl), until)
	}
	if err != nil {
		return fmt.Errorf("saving notification level: %w", err)
	}
	c.post(Update{Channels: true})
	return nil
}

// SetNotifySetting stores one global setting: notify.default,
// notify.content, notify.quiet, notify.dnd_until, sound.all,
// sound.<cue>, sound.volume. Values are validated; "" clears.
func (c *Client) SetNotifySetting(key, value string) error {
	ok := false
	switch {
	case key == "notify.default":
		_, err := ParseLevel(value)
		ok = value == "" || err == nil
	case key == "notify.content":
		ok = value == "" || value == "full" || value == "name" || value == "none"
	case key == "notify.quiet":
		_, _, err := ParseQuiet(value)
		ok = value == "" || err == nil
	case key == "notify.dnd_until":
		_, err := strconv.ParseInt(value, 10, 64)
		ok = value == "" || err == nil
	case key == "sound.volume":
		v, err := strconv.Atoi(value)
		ok = value == "" || (err == nil && v >= 0 && v <= 100)
	case key == "sound.all" || strings.HasPrefix(key, "sound.") && isCue(strings.TrimPrefix(key, "sound.")):
		ok = value == "" || value == "on" || value == "off"
	}
	if !ok {
		return fmt.Errorf("bad setting %s=%q", key, value)
	}
	var err error
	if value == "" {
		_, err = c.db.Exec(`DELETE FROM meta WHERE k = ?`, key)
	} else {
		err = setMeta(c.db, key, []byte(value))
	}
	if err != nil {
		return fmt.Errorf("saving %s: %w", key, err)
	}
	c.post(Update{Channels: true})
	return nil
}

func isCue(s string) bool {
	for _, c := range SoundCues {
		if c == s {
			return true
		}
	}
	return false
}

// InMyThread reports whether m is someone else's reply in a thread I
// started or replied in before it (the activity view's ActReply rule), so
// it notifies like a mention.
func (c *Client) InMyThread(m proto.Message) bool {
	me := c.Me().ID
	if m.Thread == 0 || m.Author == me {
		return false
	}
	var author, first uint64
	if c.db.QueryRow(`SELECT author FROM msgs WHERE id = ?`, m.Thread).Scan(&author) == nil && author == me {
		return true
	}
	_ = c.db.QueryRow(`SELECT COALESCE(MIN(id), 0) FROM msgs WHERE thread = ? AND author = ?`, m.Thread, me).Scan(&first)
	return first != 0 && first < m.ID
}
