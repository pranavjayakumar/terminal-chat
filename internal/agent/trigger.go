package agent

import (
	"fmt"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/agent/runner"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// Ask is a message the agent should answer.
type Ask struct {
	Msg    client.Message
	Chan   uint64
	Thread uint64 // where the reply goes (0 = top level)
	Key    string // session key: one engine session per (channel, thread)
	Asker  uint64 // the person behind the ask
	// Via is the handle of the agent that called us for Asker ("" = they
	// asked themselves).
	Via   string
	Owner bool // Asker is our owner, asking in person
	InDM  bool // in our agent DM
	// FollowUp: no mention, but the agent is engaged in the thread
	// (engage.go); the run decides whether to answer at all.
	FollowUp bool
	// Watch is the owner's watch this follow-up falls under (watch.go).
	Watch *Watch
}

// SessionKey names the session of a thread of ch.
func SessionKey(ch, thread uint64) string { return fmt.Sprintf("c%d-t%d", ch, thread) }

// Trigger decides whether m is an ask for the agent handle owned by owner.
// Only text that is what its author sent counts (plaintext channels and
// verified encrypted messages): anything the server may have forged or
// that can't be read yet never starts a run.
//
// Asks are: anything the owner writes in person in the agent DM, and any
// message that @mentions the handle. A message from another agent counts
// as its owner's ask.
func Trigger(m client.Message, kind string, owner uint64, handle string) (Ask, bool) {
	if m.ID == 0 || m.Deleted || !m.Sec.Trusted() {
		return Ask{}, false
	}
	if m.FromAgent() && m.Author == owner {
		return Ask{}, false // our own words: the owner has one agent
	}
	a := Ask{Msg: m, Chan: m.Chan, Asker: m.Author, Via: m.Agent}
	// Posted through tc mcp by an outside Claude session: the owner's
	// account, but not words the owner typed.
	a.Owner = m.Author == owner && !m.FromAgent() && m.Via == ""
	switch {
	case kind == proto.ChanAgentDM:
		if !a.Owner {
			return Ask{}, false
		}
		a.InDM = true
		a.Thread = m.Thread
	case client.Mentioned(m.Body, []string{handle}):
		a.Thread = m.Thread
		if a.Thread == 0 {
			a.Thread = m.ID // answer in a thread on the mention
		}
	default:
		return Ask{}, false
	}
	a.Key = SessionKey(a.Chan, a.Thread)
	return a, true
}

// EffectiveMode is what an ask may do under the owner's mode setting:
// Work lets only the owner's own asks work, Crew lets anyone's.
func EffectiveMode(set home.Mode, a Ask) runner.Mode {
	switch set {
	case home.ModeCrew:
		return runner.Work
	case home.ModeWork:
		if a.Owner {
			return runner.Work
		}
	}
	return runner.Talk
}
