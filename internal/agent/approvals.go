package agent

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/sanitize"
)

// Approval cards: the agent posts "✋ approve: <action>?" in the thread
// and waits for its owner to react ✅ or ❌ on it. The card carries a
// random code so the supervisor can find its own message when it comes
// back from the server.

// Reactions that answer a card. ♾️ next to ✅ also means "PRs are ok
// for the rest of today".
var (
	yesEmoji    = []string{"✅", "👍"}
	noEmoji     = []string{"❌", "👎"}
	alwaysEmoji = []string{"♾️", "♾"}
)

// StopAutoPhrase, in an owner's message, ends today's ♾️ auto-approval.
const StopAutoPhrase = "stop auto-approving"

type verdict struct{ yes, always bool }

type approval struct {
	code   string
	ch, th uint64
	card   uint64 // the card's message id, once seen
	done   chan verdict
}

// CardBody is the text of an approval card.
func CardBody(owner, action, detail, code string) string {
	b := fmt.Sprintf("✋ @%s approve: %s? react ✅ / ❌  ·  %s", owner, oneLine(action, 200), code)
	if d := strings.TrimSpace(detail); d != "" {
		b += "\n" + short(sanitize.Text(d), 1500)
	}
	return b
}

func oneLine(s string, n int) string {
	return short(strings.Join(strings.Fields(sanitize.Line(s)), " "), n)
}

func newCode() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return "#" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

// askOwner posts an approval card in the run's thread and waits for the
// owner's answer, the timeout (no), or ctx (no).
func (s *Supervisor) askOwner(ctx context.Context, r *run, action, detail string) bool {
	code := newCode()
	ap := &approval{code: code, ch: r.ask.Chan, th: r.ask.Thread, done: make(chan verdict, 1)}
	s.mu.Lock()
	s.pending[code] = ap
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, code)
		s.mu.Unlock()
	}()
	owner := s.Chat.User(s.owner).Name
	s.reply(r.ask.Chan, r.ask.Thread, CardBody(owner, action, detail, code))
	r.live.setStatus("waiting for " + sanitize.Line(owner) + "'s ok…")
	s.activity(r.ask, "approval", "asked: "+oneLine(action, 200), 0)

	t := time.NewTimer(s.ApprovalTimeout)
	defer t.Stop()
	var v verdict
	select {
	case v = <-ap.done:
	case <-t.C:
		s.reply(r.ask.Chan, r.ask.Thread, "⌛ no answer on "+code+", so I'm not doing it")
	case <-ctx.Done():
	}
	said := "no"
	if v.yes {
		said = "yes"
	}
	s.activity(r.ask, "approval", said+": "+oneLine(action, 200), 0)
	if v.yes && v.always {
		today := s.Now().Format(time.DateOnly)
		s.queue.update(func(q *queueState) { q.AutoPR = today })
		s.activity(r.ask, "approval", "♾️ draft PRs approved for the rest of today (say \""+StopAutoPhrase+"\" to stop)", 0)
	}
	return v.yes
}

// autoPR reports whether the owner's ♾️ approves draft PRs today.
func (s *Supervisor) autoPR() bool {
	return s.queue.get().AutoPR == s.Now().Format(time.DateOnly)
}

// stopAuto handles an owner's "stop auto-approving": true when the ask was
// only that.
func (s *Supervisor) stopAuto(a Ask) bool {
	if !a.Owner || !strings.Contains(strings.ToLower(a.Msg.Body), StopAutoPhrase) {
		return false
	}
	was := s.autoPR()
	s.queue.update(func(q *queueState) { q.AutoPR = "" })
	if was {
		s.reply(a.Chan, a.Thread, "ok, PRs need your tap again ✋")
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
