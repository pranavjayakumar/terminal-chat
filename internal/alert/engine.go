package alert

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/proto"
)

// World is what the engine needs to know about the chat right now. The UI
// answers from its loaded state, the background process from the cache.
type World interface {
	Me() uint64
	Chan(id uint64) (client.ChannelView, bool)
	User(id uint64) proto.User
	MentionNames() []string // names that @mention me
	Highlights() []string
	InMyThread(x proto.Message) bool
	// Current is x as the cache has it now; ok is false when it's gone.
	Current(x proto.Message) (proto.Message, bool)
	// Looking: ch is open and the window has focus (never in the background).
	Looking(ch uint64) bool
	// InHuddle: we're in ch's huddle on this device.
	InHuddle(ch uint64) bool
	// Roster is ch's huddle roster; ok is false when unknown.
	Roster(ch uint64) (users []uint64, ok bool)
}

// Sender shows a notification without blocking (*notify.Notifier). It may
// drop it.
type Sender interface {
	Send(n notify.Note) bool
}

// TimerKind says which engine method a Timer calls back.
type TimerKind int

const (
	FlushTimer  TimerKind = iota // Flush(Ch, Gen)
	RingResendT                  // RingTimer(Gen, true)
	RingEndT                     // RingTimer(Gen, false)
)

// Timer is a callback the caller must arrange After from now.
type Timer struct {
	Kind  TimerKind
	Ch    uint64
	Gen   int
	After time.Duration
}

// Pending is a channel's coalescing window: the first message notifies at
// once, later ones within the window are counted and summarized when it
// closes. The strongest cue in the window wins.
type Pending struct {
	Gen         int
	N           int // messages not yet notified
	title, body string
	group       string // title for a summary
	cue         notify.Cue
	Kind        Kind          // the strongest in the window
	last        proto.Message // the message title/body show, re-read before showing
}

// RingState is the DM ring in progress (Ch 0: none).
type RingState struct {
	Ch, By uint64
	Gen    int
	Since  time.Time
}

// Engine is the notification state. It is not safe for concurrent use: one
// goroutine (the UI's Update, the background loop) owns it.
type Engine struct {
	W   World
	Out Sender // nil: messages don't notify
	Now func() time.Time

	Prefs      client.NotifyPrefs
	Notes      map[uint64]*Pending
	LastNotify time.Time
	Ring       RingState

	noteGen  int
	ringGen  int
	ringCool map[uint64]time.Time
	rangAt   map[uint64]time.Time // when each DM last rang
	lastRing time.Time
	huddleAt map[uint64]time.Time // last channel huddle-start popup
}

// New returns an engine with default settings.
func New(w World, out Sender, now func() time.Time) *Engine {
	return &Engine{W: w, Out: out, Now: now, Prefs: DefaultPrefs(), Notes: map[uint64]*Pending{}}
}

// SetPrefs installs new settings; a ring they no longer allow stops.
func (e *Engine) SetPrefs(p client.NotifyPrefs) {
	e.Prefs = p
	if r := e.Ring; r.Ch != 0 {
		if popup, _ := Decide(p, e.Now(), r.Ch, Ring); !popup {
			e.StopRing(false)
		}
	}
}

// IsReminder reports whether x is a delivered reminder in my notes.
func (e *Engine) IsReminder(x proto.Message) bool {
	v, ok := e.W.Chan(x.Chan)
	return ok && client.IsReminder(x, e.W.Me(), v.Channel)
}

// Eligible is the trigger rule every notification passes first: never
// bots, my own messages (but my reminders), or the channel I'm looking at.
func (e *Engine) Eligible(x proto.Message) bool {
	if x.Deleted || e.W.User(x.Author).Bot {
		return false
	}
	if e.W.Looking(x.Chan) {
		return false
	}
	if x.Author == e.W.Me() {
		return e.IsReminder(x)
	}
	return true
}

// MentionClass: DMs, @mentions, highlights, my reminders, replies in my
// threads.
func (e *Engine) MentionClass(x proto.Message) bool {
	if x.Author == e.W.Me() {
		return e.IsReminder(x)
	}
	if v, ok := e.W.Chan(x.Chan); ok && v.Kind == proto.ChanDM {
		return true
	}
	if ConcernsMe(x.Body, e.W.MentionNames(), e.W.Highlights()) {
		return true
	}
	return x.Thread != 0 && e.W.InMyThread(x)
}

// ShouldNotify is the mention-class trigger for x.
func (e *Engine) ShouldNotify(x proto.Message) bool { return e.Eligible(x) && e.MentionClass(x) }

func (e *Engine) userName(id uint64) string { return UserName(e.W.User(id), id) }

// ChanTitle names ch.
func (e *Engine) ChanTitle(ch uint64) string {
	v, ok := e.W.Chan(ch)
	return ChanTitle(v, ok)
}

// NoteText is the notification for x: "#chan · alice" or "alice (DM)",
// and the first non-empty line of the body.
func (e *Engine) NoteText(x proto.Message) (title, body, group string) {
	if e.IsReminder(x) {
		title = "⏰ Reminder"
		return title, FirstNonEmptyLine(strings.TrimPrefix(x.Body, proto.RemindPrefix)), title
	}
	who := e.userName(x.Author)
	if v, ok := e.W.Chan(x.Chan); ok && v.Kind == proto.ChanDM {
		title = who + " (DM)"
		group = title
	} else {
		group = e.ChanTitle(x.Chan)
		title = group + " · " + who
	}
	return title, FirstNonEmptyLine(x.Body), group
}

// Private applies the content setting: name hides the text, none the
// sender and channel too (popups persist in OS notification history).
func (e *Engine) Private(title, body string, n int) (string, string) {
	switch e.Prefs.Content {
	case "name":
		body = "new message"
	case "none":
		title, body = "tc", "new message"
	default:
		return title, body
	}
	if n > 1 {
		body = strconv.Itoa(n) + " new messages"
	}
	return title, body
}

// ---- messages ----

// Messages handles newly arrived messages (client.Update.New).
func (e *Engine) Messages(news []client.Message) []Timer {
	if e.Out == nil {
		return nil
	}
	var ts []Timer
	now := e.Now()
	for _, x := range news {
		// Text that isn't verifiably its author's (inserted by the server,
		// invalid) never notifies.
		if !x.Sec.Trusted() || !e.Eligible(x.Message) {
			continue
		}
		kind, window := Message, AllWindow
		if e.MentionClass(x.Message) {
			kind, window = Mention, MentionWindow
		}
		if popup, cue := Decide(e.Prefs, now, x.Chan, kind); popup {
			if t, ok := e.queue(x.Message, kind, cue, window); ok {
				ts = append(ts, t)
			}
		}
	}
	return ts
}

func (e *Engine) queue(x proto.Message, kind Kind, cue notify.Cue, window time.Duration) (Timer, bool) {
	title, body, group := e.NoteText(x)
	var soft *Pending
	if p := e.Notes[x.Chan]; p != nil && kind == Mention && p.Kind < Mention {
		// A mention never waits out a level-all channel's 30 s window: it
		// opens its own, and the soft messages still unannounced are
		// announced when that one closes.
		delete(e.Notes, x.Chan)
		if p.N > 0 {
			soft = p
		}
	}
	if p := e.Notes[x.Chan]; p != nil {
		p.N++
		p.title, p.body, p.group, p.last = title, body, group, x
		if cueRank(cue) > cueRank(p.cue) {
			p.cue = cue
		}
		p.Kind = max(p.Kind, kind)
		return Timer{}, false
	}
	e.noteGen++
	p := &Pending{Gen: e.noteGen, title: title, body: body, group: group, cue: cue, Kind: kind, last: x}
	e.Notes[x.Chan] = p
	if e.Now().Sub(e.LastNotify) >= Every {
		e.fire(x.Chan, title, body, 1, cue)
		if soft != nil {
			p.N = soft.N
			if soft.N == 1 { // the window ends with the soft one, not a repeat
				p.title, p.body, p.last = soft.title, soft.body, soft.last
			}
		}
	} else {
		p.N = 1 // over the global cap: say it when the window closes
		if soft != nil {
			p.N += soft.N
		}
	}
	return Timer{Kind: FlushTimer, Ch: x.Chan, Gen: p.Gen, After: window}, true
}

// Flush closes ch's window (a FlushTimer fired).
func (e *Engine) Flush(ch uint64, gen int) []Timer {
	p := e.Notes[ch]
	if p == nil || p.Gen != gen {
		return nil
	}
	if p.N == 0 {
		delete(e.Notes, ch)
		return nil
	}
	if wait := Every - e.Now().Sub(e.LastNotify); wait > 0 {
		return []Timer{{Kind: FlushTimer, Ch: ch, Gen: gen, After: wait}}
	}
	delete(e.Notes, ch)
	// Settings may have changed while it waited (muted, DND).
	popup, cue := Decide(e.Prefs, e.Now(), ch, p.Kind)
	if !popup {
		return nil
	}
	if p.N == 1 {
		// It may have been deleted or edited while it waited: never show
		// text its author took back.
		cur, ok := e.W.Current(p.last)
		if ok && cur.Deleted {
			return nil
		}
		if ok && cur.Body != p.last.Body {
			p.title, p.body, _ = e.NoteText(cur)
		}
		e.fire(ch, p.title, p.body, 1, cue)
	} else {
		e.fire(ch, p.group, strconv.Itoa(p.N)+" new messages", p.N, cue)
	}
	return nil
}

func (e *Engine) fire(ch uint64, title, body string, n int, cue notify.Cue) {
	e.LastNotify = e.Now()
	title, body = e.Private(title, body, n)
	e.send(notify.Note{Title: title, Body: body, Cue: cue, Tag: NoteTag(ch)})
}

func (e *Engine) send(n notify.Note) {
	if e.Out != nil {
		e.Out.Send(n)
	}
}

// Seen drops pending notifications for a channel the user is now looking at.
func (e *Engine) Seen(ch uint64) { delete(e.Notes, ch) }

// ---- huddle starts: rings and popups ----

// HuddleStarts handles huddles that just started (client.Update.HuddleStarted).
func (e *Engine) HuddleStarts(starts []client.HuddleStart) []Timer {
	var ts []Timer
	now := e.Now()
	me := e.W.Me()
	for _, s := range starts {
		v, ok := e.W.Chan(s.Chan)
		by := e.W.User(s.By)
		if !ok || !v.Member || by.Bot {
			continue
		}
		// Rosters aren't end-to-end authenticated: only name a starter
		// who is a live member (for a DM, someone in it besides us).
		if s.By == me || !slices.Contains(v.Members, s.By) || by.Disabled {
			continue
		}
		who := e.userName(s.By)
		if v.Kind == proto.ChanDM {
			// One ring per DM per RingRepeat however it ended (a peer
			// flapping join/leave), one per RingEvery overall.
			if now.Before(e.ringCool[s.Chan]) || e.W.Looking(s.Chan) ||
				now.Sub(e.rangAt[s.Chan]) < RingRepeat || now.Sub(e.lastRing) < RingEvery {
				continue
			}
			if e.Ring.Ch != 0 { // already ringing: a plain popup for the second
				if popup, _ := Decide(e.Prefs, now, s.Chan, Ring); popup && now.Sub(e.LastNotify) >= Every {
					if e.rangAt == nil {
						e.rangAt = map[uint64]time.Time{}
					}
					e.rangAt[s.Chan] = now
					e.LastNotify = now
					join := "/huddle in that DM to join"
					if !client.VoiceAvailable() {
						join = PlainJoinHint
					}
					title, body := e.Private("📞 "+who+" started a huddle", join, 1)
					e.send(notify.Note{Title: title, Body: body, Cue: notify.CueMention, Tag: NoteTag(s.Chan)})
				}
				continue
			}
			if e.W.InHuddle(s.Chan) {
				continue
			}
			popup, cue := Decide(e.Prefs, now, s.Chan, Ring)
			if !popup {
				continue
			}
			e.ringGen++
			e.Ring = RingState{Ch: s.Chan, By: s.By, Gen: e.ringGen, Since: now}
			if e.rangAt == nil {
				e.rangAt = map[uint64]time.Time{}
			}
			e.rangAt[s.Chan], e.lastRing = now, now
			e.sendRing(cue)
			ts = append(ts, Timer{Kind: RingResendT, Gen: e.ringGen, After: RingResend},
				Timer{Kind: RingEndT, Gen: e.ringGen, After: RingFor})
			continue
		}
		if e.W.Looking(s.Chan) {
			continue
		}
		popup, cue := Decide(e.Prefs, now, s.Chan, Huddle)
		// The 2 s cap covers these too; the sidebar's 🎧 still shows a
		// start that is skipped here.
		if !popup || now.Sub(e.huddleAt[s.Chan]) < HuddleNoteGap || now.Sub(e.LastNotify) < Every {
			continue
		}
		e.LastNotify = now
		if e.huddleAt == nil {
			e.huddleAt = map[uint64]time.Time{}
		}
		e.huddleAt[s.Chan] = now
		title, body := "🎧 "+who+" started a huddle in "+e.ChanTitle(s.Chan), "/huddle to join"
		if !client.VoiceAvailable() {
			body = PlainJoinHint
		}
		if e.Prefs.Content == "none" {
			title, body = "tc", "huddle started"
		}
		e.send(notify.Note{Title: title, Body: body, Cue: cue, Tag: NoteTag(s.Chan)})
	}
	return ts
}

// sendRing shows the ring popup; rings skip the queue and the 2 s cap
// (the Notifier gives them their own slot).
func (e *Engine) sendRing(cue notify.Cue) {
	r := e.Ring
	title := "📞 " + e.userName(r.By) + " started a huddle"
	body := "/huddle to join"
	if !client.VoiceAvailable() {
		body = PlainJoinHint
	}
	if e.Prefs.Content == "none" {
		title, body = "tc", "incoming huddle"
	}
	e.send(notify.Note{Title: title, Body: body, Cue: cue, Tag: "tc-ring-" + strconv.FormatUint(r.Ch, 10)})
}

// RingTimer handles a RingResendT (resend) or RingEndT timer.
func (e *Engine) RingTimer(gen int, resend bool) {
	if gen != e.Ring.Gen || e.Ring.Ch == 0 {
		return
	}
	if resend {
		if popup, cue := Decide(e.Prefs, e.Now(), e.Ring.Ch, Ring); popup {
			e.sendRing(cue)
		}
		return
	}
	e.StopRing(true)
}

// StopRing ends the ring; cool keeps the same DM from ringing again soon
// (dismissed or unanswered — a join/leave flapper can't spam).
func (e *Engine) StopRing(cool bool) {
	if e.Ring.Ch == 0 {
		return
	}
	if cool {
		if e.ringCool == nil {
			e.ringCool = map[uint64]time.Time{}
		}
		e.ringCool[e.Ring.Ch] = e.Now().Add(RingCooldown)
	}
	e.Ring = RingState{}
}

// CheckRing stops the ring once it's answered (we joined, from any
// device), over (the huddle ended), or seen (the DM is open and focused).
func (e *Engine) CheckRing() {
	r := e.Ring
	if r.Ch == 0 {
		return
	}
	if e.W.InHuddle(r.Ch) || e.W.Looking(r.Ch) {
		e.StopRing(false)
		return
	}
	roster, ok := e.W.Roster(r.Ch)
	if !ok {
		return
	}
	if len(roster) == 0 || slices.Contains(roster, e.W.Me()) {
		e.StopRing(false)
	}
}
