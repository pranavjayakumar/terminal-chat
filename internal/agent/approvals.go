package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/mcp"
	"github.com/skailama/tc/internal/sanitize"
)

// Approval cards. The agent posts a card in the thread and waits for its
// owner to react on it:
//
//	✋ Rocky wants to post in #general  ·  #k3f9a2b
//	> the first three lines
//	> of exactly what it will do
//	why: bob asked in #general: "can you tell fahim…"
//
// ✅ yes, ❌ no; ✅ with ♾️ also grants the same kind of action on the
// same target for the rest of the local day (a Grant). The code lets the
// supervisor find its own card when it comes back from the server.

// Reactions that answer a card.
var (
	yesEmoji    = []string{"✅", "👍"}
	noEmoji     = []string{"❌", "👎"}
	alwaysEmoji = []string{"♾️", "♾"}
)

// StopAutoPhrase, in an owner's message, ends every grant of today.
const StopAutoPhrase = "stop auto-approving"

// Kinds of actions a card asks about; a Grant covers one kind on one
// target.
const (
	KindPost  = "post"  // post outside the asking thread; target: the channel
	KindPR    = "pr"    // push and open a draft PR; target: the repo
	KindIssue = "issue" // open an issue for someone else's ask; target: the repo
	KindTool  = "tool"  // a tool call the rules didn't settle; target: the tool
	KindAsk   = "ask"   // anything else the agent asks about; target: the action
)

// Grant is a ♾️ "don't ask again today" for one kind of action on one
// target.
type Grant struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Day    string `json:"day"` // YYYY-MM-DD, local
}

// Approval is what a card asks for.
type Approval struct {
	Kind, Target string
	Action       string // "post in #general"
	Preview      string // exactly what will happen; the first lines are shown
	Why          string // which ask it came from
}

type verdict struct{ yes, always bool }

type approval struct {
	code   string
	ch, th uint64
	card   uint64 // the card's message id, once seen
	done   chan verdict
}

// CardBody is the text of an approval card.
func CardBody(agent string, ap Approval, code string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "✋ %s wants to %s  ·  %s", agentTitle(agent), oneLine(ap.Action, 200), code)
	if p := strings.TrimSpace(sanitize.Text(ap.Preview)); p != "" {
		lines := strings.Split(p, "\n")
		more := len(lines) > 3
		if more {
			lines = lines[:3]
		}
		for i, l := range lines {
			l = short(l, 200)
			if more && i == 2 {
				l += " …"
			}
			b.WriteString("\n> " + l)
		}
	}
	if w := oneLine(ap.Why, 300); w != "" {
		b.WriteString("\nwhy: " + w)
	}
	return b.String()
}

// agentTitle is "rocky" as a name: "Rocky".
func agentTitle(h string) string {
	if h == "" {
		return "Your agent"
	}
	return strings.ToUpper(h[:1]) + h[1:]
}

func oneLine(s string, n int) string {
	return short(strings.Join(strings.Fields(sanitize.Line(s)), " "), n)
}

func newCode() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return "#" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

// why says which ask a card came from.
func (s *Supervisor) why(a Ask) string {
	where := "a channel"
	if cv, ok := s.Chat.Channel(a.Chan); ok {
		where = mcp.ChanLabel(cv)
	}
	text := oneLine(a.Msg.Body, 80)
	if text == "" {
		return s.askerName(a) + " in " + where
	}
	return fmt.Sprintf("%s asked in %s: %q", s.askerName(a), where, text)
}

// askOwner asks the owner for ap: a grant of today answers at once;
// otherwise a card goes up in the run's thread and the answer, the
// timeout (no) or ctx (no) decides.
func (s *Supervisor) askOwner(ctx context.Context, r *run, ap Approval) bool {
	if ap.Why == "" {
		ap.Why = s.why(r.ask)
	}
	if !NeedsApproval(r.cfg.Approvals, ap.Kind) {
		s.activity(r.ask, "approval", "not asked (approvals: "+string(r.cfg.Approvals)+"): "+oneLine(ap.Action, 200), 0)
		return true
	}
	// A ♾️ is the owner's ok for their own asks: someone else's ask always
	// gets a card.
	if r.ask.Owner && s.granted(ap.Kind, ap.Target) {
		s.activity(r.ask, "approval", "auto (♾️ today): "+oneLine(ap.Action, 200), 0)
		return true
	}
	code := newCode()
	pend := &approval{code: code, ch: r.ask.Chan, th: r.ask.Thread, done: make(chan verdict, 1)}
	s.mu.Lock()
	s.pending[code] = pend
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, code)
		s.mu.Unlock()
	}()
	owner := s.Chat.User(s.owner).Name
	s.reply(r.ask.Chan, r.ask.Thread, CardBody(s.handleNow(), ap, code))
	r.live.setStatus("waiting for " + sanitize.Line(owner) + "'s ok…")
	s.activity(r.ask, "approval", "asked: "+oneLine(ap.Action, 200), 0)

	t := time.NewTimer(s.ApprovalTimeout)
	defer t.Stop()
	var v verdict
	select {
	case v = <-pend.done:
	case <-t.C:
		s.reply(r.ask.Chan, r.ask.Thread, "⌛ no answer on "+code+", so I'm not doing it")
	case <-ctx.Done():
	}
	said := "no"
	if v.yes {
		said = "yes"
	}
	s.activity(r.ask, "approval", said+": "+oneLine(ap.Action, 200), 0)
	if v.yes && v.always && !r.ask.Owner {
		s.activity(r.ask, "approval", "♾️ not kept: it only covers your own asks", 0)
	}
	if v.yes && v.always && r.ask.Owner {
		s.grant(ap.Kind, ap.Target)
		s.activity(r.ask, "approval", "♾️ "+GrantText(Grant{Kind: ap.Kind, Target: ap.Target})+" ok for the rest of today (say \""+StopAutoPhrase+"\" to stop)", 0)
	}
	return v.yes
}

// NeedsApproval reports whether a kind of action asks the owner first at
// an approvals level. work_only asks only about code going out (PRs,
// pushes), issues, tool calls the rules didn't settle (shell writes
// outside granted folders) and what the agent itself flags with
// ask_owner; posting never asks.
func NeedsApproval(level home.Approvals, kind string) bool {
	switch level {
	case home.ApproveNever:
		return false
	case home.ApproveAlways:
		return true
	}
	return kind != KindPost
}

// GrantText says what a grant covers.
func GrantText(g Grant) string {
	switch g.Kind {
	case KindPost:
		return "posting in " + g.Target
	case KindPR:
		return "draft PRs on " + g.Target
	case KindIssue:
		return "issues on " + g.Target
	case KindTool:
		tool, _, _ := strings.Cut(g.Target, " ")
		return "that exact " + tool + " call"
	}
	return g.Target
}

// ToolTarget is a tool call's grant target: the tool and a digest of its
// exact input, so a ♾️ never stretches to other calls of the same tool.
func ToolTarget(tool string, input []byte) string {
	sum := sha256.Sum256(input)
	return tool + " " + hex.EncodeToString(sum[:8])
}

func (s *Supervisor) grant(kind, target string) {
	today := s.Now().Format(time.DateOnly)
	s.queue.update(func(q *queueState) {
		q.Grants = slices.DeleteFunc(q.Grants, func(g Grant) bool { return g.Day != today || g.Kind == kind && g.Target == target })
		q.Grants = append(q.Grants, Grant{Kind: kind, Target: target, Day: today})
	})
	s.saveStatus()
}

// granted reports whether a ♾️ of today covers kind on target.
func (s *Supervisor) granted(kind, target string) bool {
	today := s.Now().Format(time.DateOnly)
	return slices.ContainsFunc(s.queue.get().Grants, func(g Grant) bool {
		return g.Day == today && g.Kind == kind && g.Target == target
	})
}

// Grants lists today's grants.
func (s *Supervisor) Grants() []Grant {
	today := s.Now().Format(time.DateOnly)
	var out []Grant
	for _, g := range s.queue.get().Grants {
		if g.Day == today {
			out = append(out, g)
		}
	}
	return out
}

// stopAuto handles an owner's "stop auto-approving": true when the ask was
// only that.
func (s *Supervisor) stopAuto(a Ask) bool {
	if !a.Owner || !strings.Contains(strings.ToLower(a.Msg.Body), StopAutoPhrase) {
		return false
	}
	was := len(s.Grants()) > 0
	s.queue.update(func(q *queueState) { q.Grants = nil })
	s.saveStatus()
	if was {
		s.reply(a.Chan, a.Thread, "ok, everything needs your tap again ✋")
		s.activity(a, "approval", "auto-approval stopped", 0)
	} else {
		s.reply(a.Chan, a.Thread, "nothing was auto-approved, so we're good")
	}
	return true
}

// noteOwnMessage links an approval card to its message id.
func (s *Supervisor) noteOwnMessage(m client.Message) {
	if !strings.HasPrefix(m.Body, "✋ ") {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for code, ap := range s.pending {
		if ap.card == 0 && ap.ch == m.Chan && strings.Contains(m.Body, code) {
			ap.card = m.ID
		}
	}
	s.checkApprovalsLocked(m.Chan)
}

// checkApprovals answers the cards in ch the owner has reacted to.
func (s *Supervisor) checkApprovals(ch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkApprovalsLocked(ch)
}

func (s *Supervisor) checkApprovalsLocked(ch uint64) {
	for _, ap := range s.pending {
		if ap.ch != ch || ap.card == 0 {
			continue
		}
		var msgs []client.Message
		if ap.th != 0 {
			msgs = s.Chat.Thread(ap.th)
		} else {
			msgs = s.Chat.Messages(ch, 100)
		}
		for _, m := range msgs {
			if m.ID != ap.card {
				continue
			}
			if yes, ok, always := Verdict(m.Reactions, s.owner); ok {
				select {
				case ap.done <- verdict{yes, always}:
				default:
				}
			}
		}
	}
}

// Verdict reads the owner's answer from a card's reactions: ❌ wins over
// ✅ when both are there; always is ✅ with ♾️ (PRs ok for today). The
// server refuses reactions from agent sessions, so the owner's user here
// is one of the owner's own devices.
func Verdict(reactions map[string][]uint64, owner uint64) (yes, answered, always bool) {
	has := func(emoji []string) bool {
		for _, e := range emoji {
			if slices.Contains(reactions[e], owner) {
				return true
			}
		}
		return false
	}
	switch {
	case has(noEmoji):
		return false, true, false
	case has(yesEmoji):
		return true, true, has(alwaysEmoji)
	}
	return false, false, false
}
