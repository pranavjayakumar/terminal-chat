package agent

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/mcp"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// A watch is an engagement the owner asked for: "keep an eye on #general
// for 5 min and reply to fahim". New messages there reach the agent as
// follow-ups that carry the owner's instruction; when the watch ends the
// agent tells the owner in its DM what happened.

// MaxWatchMinutes caps a watch; MaxWatchRuns caps the runs one starts.
const (
	MaxWatchMinutes = 120
	MaxWatchRuns    = 40
)

// Watch is one active watch.
type Watch struct {
	Chan    uint64 `json:"chan"`
	Thread  uint64 `json:"thread,omitempty"` // 0 = the whole channel
	Where   string `json:"where"`
	About   string `json:"about"`
	Started int64  `json:"started"`
	Until   int64  `json:"until"`

	runs    int
	replies int
	authors []string
}

func watchKey(ch, thread uint64) string { return fmt.Sprintf("w%d-%d", ch, thread) }

// covers reports whether a message of ch in thread falls under w.
func (w *Watch) covers(ch, thread uint64) bool {
	return w.Chan == ch && (w.Thread == 0 || w.Thread == thread)
}

// startWatch starts (or replaces) a watch.
func (s *Supervisor) startWatch(cv client.ChannelView, thread uint64, minutes int, about string) *Watch {
	now := s.Now()
	where := mcp.ChanLabel(cv)
	if thread != 0 {
		where += fmt.Sprintf(" (thread %d)", thread)
	}
	w := &Watch{Chan: cv.ID, Thread: thread, Where: where, About: about, Started: now.UnixMilli(),
		Until: now.Add(time.Duration(minutes) * time.Minute).UnixMilli()}
	s.mu.Lock()
	s.watches[watchKey(cv.ID, thread)] = w
	s.mu.Unlock()
	s.logActivity(Activity{T: now.UnixMilli(), What: "watch", Where: where, Chan: cv.ID, Thread: thread,
		Detail: fmt.Sprintf("%d min: %s", minutes, oneLine(about, 200))})
	s.saveStatus()
	return w
}

// stopWatches ends the watches of ch (thread 0: all of them) and reports
// each to the owner. It returns how many ended.
func (s *Supervisor) stopWatches(ch, thread uint64, why string) int {
	s.mu.Lock()
	var ended []*Watch
	for k, w := range s.watches {
		if w.Chan == ch && (thread == 0 || w.Thread == thread) {
			ended = append(ended, w)
			delete(s.watches, k)
		}
	}
	s.mu.Unlock()
	for _, w := range ended {
		s.endWatch(w, why)
	}
	return len(ended)
}

// expireWatches ends the watches whose time is up.
func (s *Supervisor) expireWatches() {
	now := s.Now().UnixMilli()
	s.mu.Lock()
	var ended []*Watch
	for k, w := range s.watches {
		if now >= w.Until {
			ended = append(ended, w)
			delete(s.watches, k)
		}
	}
	s.mu.Unlock()
	for _, w := range ended {
		s.endWatch(w, "time's up")
	}
}

// endWatch posts the watch's summary in the owner's DM.
func (s *Supervisor) endWatch(w *Watch, why string) {
	s.mu.Lock()
	sum := WatchSummary(w, s.Now())
	s.mu.Unlock()
	s.logActivity(Activity{T: s.Now().UnixMilli(), What: "watch", Where: w.Where, Chan: w.Chan, Thread: w.Thread, Detail: "ended (" + why + "): " + sum})
	s.saveStatus()
	if cv, err := mcp.ResolveChan(s.Chat, "dm"); err == nil {
		s.reply(cv.ID, 0, sum)
	}
}

// WatchSummary is the one line the owner gets when a watch ends.
func WatchSummary(w *Watch, now time.Time) string {
	mins := max(1, int((min(now.UnixMilli(), w.Until)-w.Started+30_000)/60_000))
	var b strings.Builder
	fmt.Fprintf(&b, "watched %s %d min: ", w.Where, mins)
	switch len(w.authors) {
	case 0:
		b.WriteString("nobody said anything")
	case 1:
		b.WriteString(w.authors[0] + " wrote")
	default:
		b.WriteString(strings.Join(w.authors[:len(w.authors)-1], ", ") + " and " + w.authors[len(w.authors)-1] + " wrote")
	}
	switch w.replies {
	case 0:
		if len(w.authors) > 0 {
			b.WriteString(", I stayed quiet")
		}
	case 1:
		b.WriteString(", I answered once")
	case 2:
		b.WriteString(", I answered twice")
	default:
		fmt.Fprintf(&b, ", I answered %d times", w.replies)
	}
	return b.String()
}

// watchAsk turns a message under an active watch into a follow-up ask
// carrying the owner's instruction.
func (s *Supervisor) watchAsk(m client.Message, kind string) (Ask, bool) {
	if m.ID == 0 || m.Deleted || !m.Sec.Trusted() || m.FromAgent() || m.Via != "" || s.Chat.User(m.Author).Bot || kind == proto.ChanAgentDM {
		return Ask{}, false
	}
	now := s.Now().UnixMilli()
	s.mu.Lock()
	var w *Watch
	for _, x := range s.watches {
		if x.covers(m.Chan, m.Thread) && now < x.Until && (w == nil || x.Thread != 0) {
			w = x
		}
	}
	if w == nil {
		s.mu.Unlock()
		return Ask{}, false
	}
	name := sanitize.Line(s.Chat.User(m.Author).Name)
	if name != "" && !slices.Contains(w.authors, name) {
		w.authors = append(w.authors, name)
	}
	w.runs++
	over := w.runs > MaxWatchRuns
	s.mu.Unlock()
	if over {
		return Ask{}, false
	}
	return Ask{Msg: m, Chan: m.Chan, Thread: m.Thread, Key: watchKey(w.Chan, w.Thread), Asker: m.Author,
		Owner: m.Author == s.owner, FollowUp: true, Watch: w}, true
}

// watchNote opens a watch run's prompt with the owner's instruction.
func (s *Supervisor) watchNote(w *Watch) string {
	owner := sanitize.Line(s.Chat.User(s.owner).Name)
	s.mu.Lock()
	left := max(0, int((w.Until-s.Now().UnixMilli())/60_000))
	s.mu.Unlock()
	return fmt.Sprintf(`[tc] %s (your owner) asked you to watch %s, %d more min: %q
A new message arrived there. Answer only if it fits what %s asked; otherwise reply NO_REPLY.
The person who wrote it is the asker: their rules apply, not your owner's.`, owner, w.Where, left, oneLine(w.About, 300), owner)
}

func (s *Supervisor) watchReplied(w *Watch) {
	s.mu.Lock()
	w.replies++
	s.mu.Unlock()
}

// Watches lists the active watches, soonest to end first.
func (s *Supervisor) Watches() []Watch {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Watch, 0, len(s.watches))
	for _, w := range s.watches {
		out = append(out, Watch{Chan: w.Chan, Thread: w.Thread, Where: w.Where, About: w.About, Started: w.Started, Until: w.Until})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Until < out[j].Until })
	return out
}

// People lists the @names in a conversation, so the agent mentions people
// by their real handles: the channel's members, else whoever wrote in it.
func (s *Supervisor) People(ch, thread uint64) string {
	seen := map[uint64]bool{}
	var names []string
	add := func(id uint64) {
		if seen[id] || len(names) >= 40 {
			return
		}
		seen[id] = true
		u := s.Chat.User(id)
		if u.Name == "" || u.Bot {
			return
		}
		n := "@" + sanitize.Line(u.Name)
		if d := sanitize.Line(u.Display); d != "" && d != u.Name {
			n += " (" + d + ")"
		}
		if id == s.owner {
			n += " (your owner)"
		}
		names = append(names, n)
	}
	var msgs []client.Message
	if thread != 0 {
		msgs = s.Chat.Thread(thread)
	} else {
		msgs = s.Chat.Messages(ch, 50)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		add(msgs[i].Author)
	}
	if cv, ok := s.Chat.Channel(ch); ok {
		for _, id := range cv.Members {
			add(id)
		}
	}
	// Agents whose owners are here can be talked to: they hear only an
	// @mention of their handle.
	me := s.handleNow()
	for _, a := range s.Chat.Agents() {
		if !seen[a.Owner] || a.Handle == "" || len(names) >= 50 {
			continue
		}
		n := "@" + sanitize.Line(a.Handle)
		if a.Handle == me {
			n += " (you)"
		} else {
			n += " (" + sanitize.Line(s.Chat.User(a.Owner).Name) + "'s agent)"
		}
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}
