package agent

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/agent/runner"
	"github.com/skailama/tc/internal/mcp"
	"github.com/skailama/tc/internal/proto"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestEngagedThread(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	h := newHarness(t, cfgDefault(), func(s *Supervisor) { s.Now = clock.now })
	var mu sync.Mutex
	answer := "sure thing"
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		mu.Lock()
		text := answer
		mu.Unlock()
		for _, part := range []string{text[:2], text[2:]} {
			on(runner.Event{Kind: runner.EvText, Text: part})
		}
		return runner.Result{Session: req.Session, Text: text, CostUSD: 0.01}, nil
	}
	setAnswer := func(s string) { mu.Lock(); answer = s; mu.Unlock() }

	h.chat.say(bobID, general, 0, "@rocky what's our deploy cmd?", 0)
	h.waitFor("first reply", func() bool { return len(h.chat.posts()) == 1 })
	h.waitFor("engaged", func() bool { return h.s.Engaged(general, 0) })
	h.waitFor("first run's status", func() bool {
		l := h.chat.lives()
		return len(l) > 0 && l[len(l)-1].state == proto.AgentDone
	})

	// A follow-up without a mention runs, resumes the session, and stays
	// silent on NO_REPLY: no post, no live status at all.
	setAnswer(NoReply)
	livesBefore := len(h.chat.lives())
	h.chat.say(carolID, general, 0, "lol nice", 0)
	h.waitFor("follow-up run", func() bool { return len(h.run.requests()) == 2 })
	time.Sleep(50 * time.Millisecond)
	req := h.run.requests()[1]
	if !req.Resume || !strings.HasPrefix(req.Prompt, FollowUpNote) || len(h.chat.posts()) != 1 {
		t.Fatalf("follow-up %+v posts %+v", req, h.chat.posts())
	}
	if n := len(h.chat.lives()); n != livesBefore {
		t.Fatalf("NO_REPLY broadcast %d statuses: %+v", n-livesBefore, h.chat.lives()[livesBefore:])
	}
	// It counts toward the daily spend, not toward carol's asks.
	if h.s.ledger.Asks(carolID, clock.now()) != 0 || h.s.ledger.Spent(clock.now()) < 0.019 {
		t.Fatalf("ledger asks %d spent %v", h.s.ledger.Asks(carolID, clock.now()), h.s.ledger.Spent(clock.now()))
	}

	// A real answer to a follow-up shows its status, posts, and extends
	// the window.
	setAnswer("it's make deploy")
	clock.add(8 * time.Minute)
	h.chat.say(bobID, general, 0, "and for staging?", 0)
	h.waitFor("follow-up reply", func() bool { return len(h.chat.posts()) == 2 })
	if p := h.chat.posts()[1]; p.thread != 0 {
		t.Fatalf("posted %+v", p)
	}
	h.waitFor("live status", func() bool { return len(h.chat.lives()) > livesBefore })
	clock.add(8 * time.Minute)
	if !h.s.Engaged(general, 0) {
		t.Fatal("the reply didn't extend the window")
	}

	// Bots and other threads don't count.
	h.chat.mu.Lock()
	h.chat.users[9] = proto.User{ID: 9, Name: "ci-bot", Bot: true}
	h.chat.mu.Unlock()
	runs := len(h.run.requests())
	h.chat.say(9, general, 0, "build green", 0)
	h.chat.say(bobID, general, 999, "unrelated thread", 0)
	time.Sleep(50 * time.Millisecond)
	if len(h.run.requests()) != runs {
		t.Fatal("a bot or another thread's message ran")
	}
	// Calling another agent ends it.
	h.chat.say(bobID, general, 0, "@bobbot can you take over", 0)
	time.Sleep(30 * time.Millisecond)
	if h.s.Engaged(general, 0) || len(h.run.requests()) != runs {
		t.Fatal("still engaged after another agent was called")
	}
}

func TestEngagementExpiresAndLeave(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	h := newHarness(t, cfgDefault(), func(s *Supervisor) { s.Now = clock.now })
	h.chat.say(bobID, general, 0, "@rocky hi", 0)
	h.waitFor("reply", func() bool { return len(h.chat.posts()) == 1 })
	h.waitFor("engaged", func() bool { return h.s.Engaged(general, 0) })
	clock.add(11 * time.Minute)
	h.chat.say(bobID, general, 0, "you there?", 0)
	time.Sleep(50 * time.Millisecond)
	if len(h.run.requests()) != 1 || h.s.Engaged(general, 0) {
		t.Fatal("ran after the window closed")
	}

	// leave_thread: the run says bye and the thread is left.
	block := make(chan struct{})
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-block
		return runner.Result{Session: req.Session, Text: "bye!"}, nil
	}
	h.chat.say(bobID, general, 0, "@rocky thanks, bye", 0)
	if r := h.call(mcp.ToolLeaveThread, map[string]string{}); r.Error {
		t.Fatal(r.Text)
	}
	close(block)
	h.waitFor("bye", func() bool { return len(h.chat.posts()) == 2 })
	time.Sleep(30 * time.Millisecond)
	if h.s.Engaged(general, 0) {
		t.Fatal("still engaged after leave_thread")
	}

	// Off in the config: no window at all.
	cfg := cfgDefault()
	cfg.Handle, cfg.EngageMinutes = "rocky", 0
	if err := home.SaveConfig(h.s.Dir, cfg); err != nil {
		t.Fatal(err)
	}
	h.run.reply = nil
	h.chat.say(bobID, secret, 0, "@rocky again", 0)
	h.waitFor("reply", func() bool { return len(h.chat.posts()) == 3 })
	time.Sleep(30 * time.Millisecond)
	if h.s.Engaged(secret, 0) {
		t.Fatal("engaged with engage_minutes 0")
	}
}

func TestFollowUpCap(t *testing.T) {
	h := newHarness(t, cfgDefault(), nil)
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		if strings.HasPrefix(req.Prompt, FollowUpNote) {
			return runner.Result{Session: req.Session, Text: NoReply}, nil
		}
		return runner.Result{Session: req.Session, Text: "hi"}, nil
	}
	h.chat.say(bobID, general, 0, "@rocky hi", 0)
	h.waitFor("engaged", func() bool { return h.s.Engaged(general, 0) })
	for range MaxFollowUps + 5 {
		h.chat.say(bobID, general, 0, "chatter", 0)
	}
	h.waitFor("follow-ups", func() bool { return len(h.run.requests()) == 1+MaxFollowUps })
	time.Sleep(100 * time.Millisecond)
	if n := len(h.run.requests()); n != 1+MaxFollowUps {
		t.Fatalf("%d runs", n)
	}
	if h.s.ledger.Asks(bobID, time.Now()) != 1 {
		t.Fatalf("follow-ups counted as asks: %d", h.s.ledger.Asks(bobID, time.Now()))
	}
}

func TestDMAlwaysAnswers(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	h := newHarness(t, cfgDefault(), func(s *Supervisor) { s.Now = clock.now })
	h.chat.say(ownerID, agentDM, 0, "hey", 0)
	h.waitFor("reply", func() bool { return len(h.chat.posts()) == 1 })
	clock.add(3 * time.Hour)
	h.chat.say(ownerID, agentDM, 0, "still there?", 0)
	h.waitFor("reply", func() bool { return len(h.chat.posts()) == 2 })
	if r := h.run.requests()[1]; strings.HasPrefix(r.Prompt, FollowUpNote) {
		t.Fatal("DM asks are never follow-ups")
	}
}

func TestHushedUntilReply(t *testing.T) {
	for _, s := range []string{"", " ", "NO", "NO_REP", "NO_REPLY", "NO_REPLY\n"} {
		if !hushedUntilReply(s) {
			t.Errorf("%q should stay hushed", s)
		}
	}
	for _, s := range []string{"No worries", "sure", "NOPE"} {
		if hushedUntilReply(s) {
			t.Errorf("%q should be released", s)
		}
	}
}
