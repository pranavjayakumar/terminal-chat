// Package alert is tc's notification policy: which messages and huddle
// starts pop up, with which sound, coalesced and capped how
// (docs/research/NOTIFICATIONS-2026-10.md §2). The chat UI and the
// background notifier (`tc background`) both run it, so a message notifies
// the same way whether the chat window is open or not.
//
// The Engine holds the state (coalescing windows, the global cap, rings);
// it never sleeps or starts timers itself. Every entry point returns the
// Timers its caller must arrange to fire later, so the UI can turn them
// into Bubble Tea ticks and the background process into time.AfterFunc.
package alert

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Kind is what happened.
type Kind int

const (
	Message Kind = iota // any message (level all)
	Mention             // DM, mention, highlight, reminder, reply in my thread
	Ring                // a DM huddle started
	Huddle              // a channel huddle started
)

// Timing (§2.5).
const (
	MentionWindow = 5 * time.Second  // per-channel coalescing
	AllWindow     = 30 * time.Second // coalescing for level-all channels
	Every         = 2 * time.Second  // global cap
	RingResend    = 10 * time.Second
	RingFor       = 30 * time.Second
	RingCooldown  = 5 * time.Minute  // after a dismissed or unanswered ring
	RingRepeat    = 2 * time.Minute  // a DM rings at most this often, however a ring ended
	RingEvery     = 10 * time.Second // across all DMs (a hostile server cycling DMs)
	HuddleNoteGap = 5 * time.Minute  // channel huddle-start popups per channel
)

// Decide is the policy: whether an event of kind pops up in ch at now, and
// its sound.
func Decide(p client.NotifyPrefs, now time.Time, ch uint64, kind Kind) (bool, notify.Cue) {
	lvl, _ := p.Level(ch, now)
	if lvl == client.LevelOff || p.DND(now) || p.InQuiet(now) {
		return false, notify.CueNone
	}
	var cue notify.Cue
	var sound string
	switch kind {
	case Message:
		if lvl != client.LevelAll {
			return false, notify.CueNone
		}
		cue, sound = notify.CueMessage, "message"
	case Mention:
		cue, sound = notify.CueMention, "mention"
	case Ring:
		cue, sound = notify.CueRing, "ring"
	case Huddle:
		if lvl != client.LevelAll {
			return false, notify.CueNone
		}
		cue, sound = notify.CueHuddle, "huddle"
	}
	if !p.Sound(sound) {
		cue = notify.CueNone
	}
	return true, cue
}

// Env are the env overrides (TC_SOUND, TC_NOTIFY_CONTENT, TC_QUIET): they
// win over stored settings, for scripts and SSH.
type Env struct {
	Sound, Content, Quiet string
}

// OSEnv reads Env from the process environment.
func OSEnv() Env {
	return Env{Sound: os.Getenv("TC_SOUND"), Content: os.Getenv("TC_NOTIFY_CONTENT"), Quiet: os.Getenv("TC_QUIET")}
}

// Apply returns p with e's overrides.
func (e Env) Apply(p client.NotifyPrefs) client.NotifyPrefs {
	switch strings.ToLower(e.Sound) {
	case "off", "0", "no":
		p.SoundOff = true
	case "on", "1", "yes":
		p.SoundOff = false
	}
	switch strings.ToLower(e.Content) {
	case "full":
		p.Content = ""
	case "name", "none":
		p.Content = strings.ToLower(e.Content)
	}
	if e.Quiet != "" {
		if strings.EqualFold(e.Quiet, "off") {
			p.Quiet = ""
		} else if _, _, err := client.ParseQuiet(e.Quiet); err == nil {
			p.Quiet = e.Quiet
		}
	}
	return p
}

// DefaultPrefs are the settings when none can be read.
func DefaultPrefs() client.NotifyPrefs {
	return client.NotifyPrefs{Chans: map[uint64]client.ChanNotify{}, CueOff: map[string]bool{}, Volume: client.DefaultCueVolume}
}

// UserName is how notifications (and the chat log) name user id.
func UserName(u proto.User, id uint64) string {
	if n := sanitize.Line(u.Name); n != "" {
		return n
	}
	return "user" + strconv.FormatUint(id, 10)
}

// ChanTitle is "#name", or "@names" for a DM; "#?" for an unknown channel.
func ChanTitle(v client.ChannelView, ok bool) string {
	if !ok {
		return "#?"
	}
	if v.Kind == proto.ChanDM {
		return "@" + sanitize.Line(v.Display)
	}
	return "#" + sanitize.Line(v.Name)
}

// ConcernsMe is true for a body that @mentions one of names or contains one
// of the highlight words. Mentions inside fenced code don't count.
func ConcernsMe(body string, names, highlights []string) bool {
	return client.Mentioned(OutsideCode(body), names) || client.Highlighted(body, highlights)
}

// OutsideCode is body without its fenced code blocks (the renderer's fence
// rule): a pasted package.json full of "@alice/utils" isn't a mention.
func OutsideCode(body string) string {
	if !strings.Contains(body, "```") {
		return body
	}
	var keep []string
	in := false
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			in = !in
			continue
		}
		if !in {
			keep = append(keep, ln)
		}
	}
	return strings.Join(keep, "\n")
}

// FirstNonEmptyLine is the first line of s with text on it, sanitized.
func FirstNonEmptyLine(s string) string {
	for _, l := range strings.Split(sanitize.Text(s), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// NoteTag groups a channel's popups where the backend can replace them.
func NoteTag(ch uint64) string { return "tc-" + strconv.FormatUint(ch, 10) }

// PlainJoinHint replaces "/huddle to join" in builds without voice, where
// /huddle can only fail (#85).
const PlainJoinHint = "to join: tc update --voice"

// cueRank orders cues so a mention in a window of soft messages wins.
func cueRank(c notify.Cue) int {
	switch c {
	case notify.CueMention:
		return 3
	case notify.CueMessage:
		return 2
	case notify.CueNone:
		return 1
	}
	return 0
}
