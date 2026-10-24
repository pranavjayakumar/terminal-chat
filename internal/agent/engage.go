package agent

import (
	"strings"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// Once the agent has replied in a thread it stays in the conversation for
// a while (config engage_minutes): new messages there reach it without an
// @mention, as follow-ups. A follow-up run decides whether to say anything;
// most end in NO_REPLY and show nothing at all.

// MaxFollowUps caps the follow-up runs one engagement may start.
const MaxFollowUps = 20

// FollowUpNote opens the prompt of a follow-up run.
const FollowUpNote = `[tc] Follow-up in a thread you are in. Answer only if it's directed at you or you clearly can help; otherwise reply NO_REPLY.
If the conversation is over for you (they thanked you, said bye, or moved on), say a short goodbye if it fits and call leave_thread.`

type engagement struct {
	until     time.Time
	followUps int
}

// engage keeps the agent in a thread for d from now; each reply opens a
// fresh window.
func (s *Supervisor) engage(key string, d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.engaged[key] = &engagement{until: s.Now().Add(d)}
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
// agent is engaged in its thread. Only people count (not bots, agents or
// posts through tc mcp), and only text that is what its author sent. A
// message that calls another agent ends the engagement: the conversation
// moved on.
func (s *Supervisor) followUp(m client.Message, kind string) (Ask, bool) {
	if m.Thread == 0 || m.ID == 0 || m.Deleted || !m.Sec.Trusted() || m.FromAgent() || m.Via != "" || s.Chat.User(m.Author).Bot {
		return Ask{}, false
	}
	key := SessionKey(m.Chan, m.Thread)
	s.mu.Lock()
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
