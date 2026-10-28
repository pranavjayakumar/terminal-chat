package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Once the agent has replied in a thread it stays in the conversation
// until nobody has written there for a while (config engage_minutes): new
// messages there reach it without an @mention, as follow-ups. A follow-up
// run decides whether to say anything; most end in NO_REPLY and show
// nothing at all.

// MaxFollowUps caps the follow-up runs one engagement may start.
const MaxFollowUps = 20

// FollowUpNote opens the prompt of a follow-up run.
const FollowUpNote = `[tc] Follow-up in a thread you are in. Answer only if it's directed at you or you clearly can help; otherwise reply NO_REPLY.
If they ask you to stay quiet for a while, call quiet (not leave_thread).
If the conversation is over for you (they thanked you, said bye, or moved on), say a short goodbye if it fits and call leave_thread.`

// ListenNote ends the prompt of a run outside the DM, so the agent knows
// how long it keeps listening and can say so.
const ListenNote = `
After you reply here you keep hearing this conversation without an @mention until nobody has written in it for %d min.
Asked to stay quiet for a while? Call quiet. Never promise to come back on your own otherwise: only quiet, schedule or an @mention brings you back.`

type engagement struct {
	until     time.Time
	d         time.Duration // each message heard moves until to now+d
	followUps int
}

// engage keeps the agent in a thread for d from now; each reply opens a
// fresh window. The follow-up count carries over unless reset (a direct
// ask), so replying to follow-ups can't lift MaxFollowUps.
func (s *Supervisor) engage(key string, d time.Duration, reset bool) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := &engagement{until: s.Now().Add(d), d: d}
	if old := s.engaged[key]; old != nil && !reset {
		e.followUps = old.followUps
	}
	s.engaged[key] = e
}

func (s *Supervisor) disengage(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.engaged, key)
}

// Engaged reports whether the agent is listening in a thread now.
func (s *Supervisor) Engaged(ch, thread uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.engaged[SessionKey(ch, thread)]
	return e != nil && s.Now().Before(e.until)
}

// followUp turns a message without a mention into a follow-up ask when the
// agent is engaged where it was written: a thread, or a channel's top
// level. Only people count (not bots, agents or posts through tc mcp), and
// only text that is what its author sent. A message that calls another
// agent ends the engagement: the conversation moved on. While the agent
// keeps quiet there, nothing is a follow-up; the wake picks up what was
// said (wakeQuiets).
func (s *Supervisor) followUp(m client.Message, kind string) (Ask, bool) {
	if m.ID == 0 || m.Deleted || !m.Sec.Trusted() || m.FromAgent() || m.Via != "" || s.Chat.User(m.Author).Bot {
		return Ask{}, false
	}
	key := SessionKey(m.Chan, m.Thread)
	s.mu.Lock()
	if s.quiets[key] != nil {
		s.mu.Unlock()
		return Ask{}, false
	}
	e := s.engaged[key]
	live := e != nil && s.Now().Before(e.until)
	if e != nil && !live {
		delete(s.engaged, key)
	}
	s.mu.Unlock()
	if !live {
		return Ask{}, false
	}
	if s.callsOtherAgent(m.Body) {
		s.disengage(key)
		return Ask{}, false
	}
	s.mu.Lock()
	e.followUps++
	over := e.followUps > MaxFollowUps
	if !over {
		if t := s.Now().Add(e.d); t.After(e.until) {
			e.until = t
		}
	}
	s.mu.Unlock()
	if over {
		return Ask{}, false
	}
	return Ask{Msg: m, Chan: m.Chan, Thread: m.Thread, Key: key, Asker: m.Author,
		Owner: m.Author == s.owner, InDM: kind == proto.ChanAgentDM, FollowUp: true}, true
}

// callsOtherAgent reports whether body @mentions another agent.
func (s *Supervisor) callsOtherAgent(body string) bool {
	if !strings.Contains(body, "@") {
		return false
	}
	me := s.handleNow()
	for _, a := range s.Chat.Agents() {
		if a.Handle != me && client.Mentioned(body, []string{a.Handle}) {
			return true
		}
	}
	return false
}

func (s *Supervisor) pruneEngaged() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	for k, e := range s.engaged {
		if !now.Before(e.until) {
			delete(s.engaged, k)
		}
	}
}

// hushed is a follow-up run's live status: nothing goes out until the
// engine starts a real reply (text that isn't NO_REPLY), so people don't
// see the agent "thinking" about every message in the thread.
func hushedUntilReply(text string) bool {
	t := strings.TrimSpace(text)
	return t == "" || strings.HasPrefix(NoReply, t) || strings.HasPrefix(t, NoReply)
}

// ---- quiet ----

// A Quiet is a pause someone asked for in a conversation ("be quiet for 5
// min, then say hey"): until it ends nothing there reaches the agent
// without an @mention. When it ends the agent listens again and, if it was
// told what to do then or someone wrote meanwhile, gets a turn. Quiets
// live in queue.json so a restart keeps them.
type Quiet struct {
	Chan    uint64 `json:"chan"`
	Thread  uint64 `json:"thread,omitempty"`
	Minutes int    `json:"minutes"`
	Until   int64  `json:"until"`          // unix ms
	After   uint64 `json:"after"`          // the newest message id when it started
	Then    string `json:"then,omitempty"` // what to do when it ends
	Asker   uint64 `json:"asker"`
	Owner   bool   `json:"owner,omitempty"`
}

// MaxQuietMinutes caps a quiet.
const MaxQuietMinutes = 120

// quietGrace is how late a wake may still come (a supervisor that was
// down); past it the moment is gone and the quiet just ends.
const quietGrace = 15 * time.Minute

// quietOverMark starts the prompt of a wake run.
const quietOverMark = "[tc] Your quiet time here is over."

// startQuiet pauses a's conversation for minutes. The run that calls it
// keeps it even when it replies (its reply is the "ok, going quiet").
func (s *Supervisor) startQuiet(a Ask, minutes int, then string) {
	after := a.Msg.ID
	for _, m := range s.conversation(a.Chan, a.Thread) {
		after = max(after, m.ID)
	}
	q := &Quiet{Chan: a.Chan, Thread: a.Thread, Minutes: minutes, Until: s.Now().Add(time.Duration(minutes) * time.Minute).UnixMilli(),
		After: after, Then: then, Asker: a.Asker, Owner: a.Owner}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quiets[a.Key] = q
	s.quieted[a.Key] = true
	delete(s.engaged, a.Key)
	s.saveQuietsLocked()
}

// endQuiet drops key's quiet without a wake.
func (s *Supervisor) endQuiet(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quiets[key] != nil {
		delete(s.quiets, key)
		s.saveQuietsLocked()
	}
}

// saveQuietsLocked writes the quiets to queue.json; s.mu is held so the
// writes land in order.
func (s *Supervisor) saveQuietsLocked() {
	snap := make(map[string]Quiet, len(s.quiets))
	for k, q := range s.quiets {
		snap[k] = *q
	}
	s.queue.update(func(st *queueState) { st.Quiets = snap })
}

func (s *Supervisor) loadQuiets() {
	st := s.queue.get()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, q := range st.Quiets {
		s.quiets[k] = &q
	}
}

// wakeQuiets ends the quiets whose time is up: the agent listens there
// again, and gets a turn when it was told what to do then or someone wrote
// meanwhile. A conversation with a run going or waiting is left for the
// next look: that run may end the quiet itself.
func (s *Supervisor) wakeQuiets(ctx context.Context) {
	now := s.Now()
	s.mu.Lock()
	var due []*Quiet
	var keys []string
	for k, q := range s.quiets {
		if now.UnixMilli() < q.Until || s.busy[k] || len(s.threads[k]) > 0 {
			continue
		}
		delete(s.quiets, k)
		due, keys = append(due, q), append(keys, k)
	}
	if len(due) > 0 {
		s.saveQuietsLocked()
	}
	s.mu.Unlock()
	if len(due) == 0 {
		return
	}
	cfg, err := home.LoadConfig(s.Dir)
	if err != nil {
		s.Log.Warn("agent config", "err", err)
	}
	for i, q := range due {
		key := keys[i]
		if now.Sub(time.UnixMilli(q.Until)) > max(quietGrace, time.Duration(cfg.EngageMinutes)*time.Minute) {
			continue
		}
		s.engage(key, time.Duration(cfg.EngageMinutes)*time.Minute, false)
		msg, spoke := s.sinceQuiet(q)
		if msg.ID == 0 || (q.Then == "" && !spoke) {
			continue
		}
		// The run answers what was said meanwhile as whoever said it: the
		// owner's quiet never lends their trust to someone else's words.
		asker, owner := q.Asker, q.Owner
		if spoke && msg.Author != q.Asker {
			asker, owner = msg.Author, msg.Author == s.owner
		}
		s.enqueue(ctx, Ask{Msg: msg, Chan: q.Chan, Thread: q.Thread, Key: key, Asker: asker, Owner: owner,
			FollowUp: true, Wake: q})
	}
}

// sinceQuiet finds what a wake run answers: the newest message a person
// wrote during the quiet (spoke), else the newest message there.
func (s *Supervisor) sinceQuiet(q *Quiet) (client.Message, bool) {
	var last, said client.Message
	for _, m := range s.conversation(q.Chan, q.Thread) {
		if m.ID == 0 || m.Deleted {
			continue
		}
		if m.ID > last.ID {
			last = m
		}
		if m.ID > q.After && m.ID > said.ID && m.Sec.Trusted() && !m.FromAgent() && m.Via == "" && !s.Chat.User(m.Author).Bot {
			said = m
		}
	}
	if said.ID != 0 {
		return said, true
	}
	return last, false
}

// conversation is the recent messages of a thread, or of a channel's top
// level.
func (s *Supervisor) conversation(ch, thread uint64) []client.Message {
	if thread != 0 {
		return s.Chat.Thread(thread)
	}
	return s.Chat.Messages(ch, 50)
}

// wakeNote opens a wake run's prompt.
func (s *Supervisor) wakeNote(q *Quiet) string {
	var b strings.Builder
	who := sanitize.Line(s.Chat.User(q.Asker).Name)
	if who == "" {
		who = "someone"
	}
	fmt.Fprintf(&b, "%s %s asked you to stay quiet here for %d min; you're listening again now.\n", quietOverMark, who, q.Minutes)
	if q.Then != "" {
		fmt.Fprintf(&b, "When it ended you were to: %q\n", q.Then)
	}
	b.WriteString("Do that now if it still fits, and answer what was said meanwhile if it's for you; otherwise reply NO_REPLY.")
	return b.String()
}
