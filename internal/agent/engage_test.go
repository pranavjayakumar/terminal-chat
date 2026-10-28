package agent

import (
	"context"
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
	h.waitFor("window extended", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		e := h.s.engaged[SessionKey(general, 0)]
		return e != nil && e.until.After(clock.now().Add(9*time.Minute))
	})
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
	cfg := cfgDefault()
	cfg.AsksPerPerson = 100
	h := newHarness(t, cfg, nil)
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
	if n := h.s.ledger.Asks(bobID, time.Now()); n != 1 {
		t.Fatalf("silent follow-ups counted as asks: %d", n)
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

// The window runs from the last message the agent heard, not from its last
// reply: a conversation that keeps going keeps it in (#161).
func TestFollowUpWindowSlides(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	h := newHarness(t, cfgDefault(), func(s *Supervisor) { s.Now = clock.now })
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		if strings.HasPrefix(req.Prompt, FollowUpNote) {
			return runner.Result{Session: req.Session, Text: NoReply}, nil
		}
		return runner.Result{Session: req.Session, Text: "sure"}, nil
	}
	h.chat.say(bobID, general, 0, "@rocky hi", 0)
	h.waitFor("engaged", func() bool { return h.s.Engaged(general, 0) })
	clock.add(8 * time.Minute)
	h.chat.say(carolID, general, 0, "lol", 0)
	h.waitFor("follow-up", func() bool { return len(h.run.requests()) == 2 })
	clock.add(8 * time.Minute) // 16 min after the reply, 8 after the last message
	h.chat.say(bobID, general, 0, "rocky, still with us?", 0)
	h.waitFor("second follow-up", func() bool { return len(h.run.requests()) == 3 })
}

func (s *Supervisor) busyKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[key]
}

// quietRun has the agent answer "be quiet" in thread root by calling quiet
// with args, and waits for the run to end.
func quietRun(t *testing.T, h *harness, root uint64, args map[string]any) {
	t.Helper()
	block := make(chan struct{})
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		if strings.Contains(req.Prompt, "be quiet for a bit") && !strings.Contains(req.Prompt, quietOverMark) {
			<-block
			return runner.Result{Session: req.Session, Text: "ok, going quiet"}, nil
		}
		if strings.HasPrefix(req.Prompt, FollowUpNote) {
			return runner.Result{Session: req.Session, Text: NoReply}, nil
		}
		return runner.Result{Session: req.Session, Text: "hey"}, nil
	}
	posts := len(h.chat.posts())
	h.chat.say(ownerID, general, root, "@rocky be quiet for a bit, then say hey", 0)
	if r := h.callIn(general, mcp.ToolQuiet, args); r.Error {
		t.Fatal(r.Text)
	}
	close(block)
	h.waitFor("ack", func() bool { return len(h.chat.posts()) == posts+1 })
	h.waitFor("run over", func() bool { return !h.s.busyKey(SessionKey(general, root)) })
}

func TestQuietThenWake(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	h := newHarness(t, cfgDefault(), func(s *Supervisor) { s.Now = clock.now })
	root := h.chat.say(bobID, general, 0, "deploy plan?", 0).ID
	quietRun(t, h, root, map[string]any{"minutes": 5, "then": "say hey"})
	runs := len(h.run.requests())

	// Quiet: nothing reaches it without a mention, and the timer doesn't
	// wake it early.
	clock.add(2 * time.Minute)
	h.chat.say(bobID, general, root, "anyone?", 0)
	time.Sleep(50 * time.Millisecond)
	h.s.wakeQuiets(context.Background())
	time.Sleep(30 * time.Millisecond)
	if n := len(h.run.requests()); n != runs {
		t.Fatalf("%d runs during the quiet", n-runs)
	}

	// Over, even past the follow-up window the ack opened: it gets a turn
	// with what it was asked to do and what it missed.
	clock.add(9 * time.Minute)
	h.s.wakeQuiets(context.Background())
	h.waitFor("wake run", func() bool { return len(h.run.requests()) == runs+1 })
	p := h.run.requests()[runs].Prompt
	if !strings.Contains(p, quietOverMark) || !strings.Contains(p, "say hey") || !strings.Contains(p, "anyone?") {
		t.Fatalf("wake prompt: %s", p)
	}
	h.waitFor("hey", func() bool { return len(h.chat.posts()) == 2 })
	h.waitFor("listening again", func() bool { return h.s.Engaged(general, root) })
	h.waitFor("run over", func() bool { return !h.s.busyKey(SessionKey(general, root)) })
	h.s.wakeQuiets(context.Background())
	h.chat.say(bobID, general, root, "cool", 0)
	h.waitFor("follow-up", func() bool { return len(h.run.requests()) == runs+2 })
	time.Sleep(50 * time.Millisecond)
	if n := len(h.run.requests()); n != runs+2 {
		t.Fatalf("woke twice: %d runs", n-runs)
	}
	if p := h.run.requests()[runs+1].Prompt; strings.Contains(p, quietOverMark) {
		t.Fatalf("a plain follow-up carried the wake note: %s", p)
	}
}

// A message that lands after the quiet ended but before the timer looked
// is the wake: one run, carrying the wake note.
func TestQuietWakeOnMessage(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	h := newHarness(t, cfgDefault(), func(s *Supervisor) { s.Now = clock.now })
	root := h.chat.say(bobID, general, 0, "deploy plan?", 0).ID
	quietRun(t, h, root, map[string]any{"minutes": 5})
	runs := len(h.run.requests())
	clock.add(5*time.Minute + time.Second)
	h.chat.say(ownerID, general, root, "ok you can talk", 0)
	h.waitFor("wake run", func() bool { return len(h.run.requests()) == runs+1 })
	h.s.wakeQuiets(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := len(h.run.requests()); n != runs+1 {
		t.Fatalf("%d runs for one message", n-runs)
	}
	if p := h.run.requests()[runs].Prompt; !strings.Contains(p, quietOverMark) || !strings.Contains(p, "ok you can talk") {
		t.Fatalf("prompt: %s", p)
	}
}

// Nothing to do and nobody spoke: no run, but it is listening again.
func TestQuietEndsSilently(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	h := newHarness(t, cfgDefault(), func(s *Supervisor) { s.Now = clock.now })
	root := h.chat.say(bobID, general, 0, "deploy plan?", 0).ID
	quietRun(t, h, root, map[string]any{"minutes": 3})
	runs := len(h.run.requests())
	clock.add(4 * time.Minute)
	h.s.wakeQuiets(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := len(h.run.requests()); n != runs || !h.s.Engaged(general, root) {
		t.Fatalf("%d runs, engaged %v", n-runs, h.s.Engaged(general, root))
	}
}

// A restart mid-quiet keeps it: the new supervisor stays quiet, then wakes.
func TestQuietSurvivesRestart(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	h := newHarness(t, cfgDefault(), func(s *Supervisor) { s.Now = clock.now })
	root := h.chat.say(bobID, general, 0, "deploy plan?", 0).ID
	quietRun(t, h, root, map[string]any{"minutes": 5, "then": "say hey"})
	h.stop()

	h2 := restartHarness(t, h, func(s *Supervisor) { s.Now = clock.now })
	clock.add(time.Minute)
	h2.chat.say(bobID, general, root, "anyone?", 0)
	time.Sleep(50 * time.Millisecond)
	if n := len(h2.run.requests()); n != 0 {
		t.Fatalf("%d runs during the quiet after a restart", n)
	}
	clock.add(5 * time.Minute)
	h2.s.wakeQuiets(context.Background())
	h2.waitFor("wake run", func() bool { return len(h2.run.requests()) == 1 })
	if p := h2.run.requests()[0].Prompt; !strings.Contains(p, "say hey") {
		t.Fatalf("prompt: %s", p)
	}
}

// Down long past the end: the moment is gone, so no late wake.
func TestQuietStaleAfterRestart(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	h := newHarness(t, cfgDefault(), func(s *Supervisor) { s.Now = clock.now })
	root := h.chat.say(bobID, general, 0, "deploy plan?", 0).ID
	quietRun(t, h, root, map[string]any{"minutes": 5, "then": "say hey"})
	h.stop()
	clock.add(3 * time.Hour)
	h2 := restartHarness(t, h, func(s *Supervisor) { s.Now = clock.now })
	h2.s.wakeQuiets(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := len(h2.run.requests()); n != 0 {
		t.Fatalf("woke %d times, hours late", n)
	}
}

// A mention that gets an answer ends the quiet; so does leave_thread.
func TestQuietEndedByMentionOrLeave(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	h := newHarness(t, cfgDefault(), func(s *Supervisor) { s.Now = clock.now })
	root := h.chat.say(bobID, general, 0, "deploy plan?", 0).ID
	quietRun(t, h, root, map[string]any{"minutes": 30, "then": "say hey"})
	runs := len(h.run.requests())
	h.chat.say(ownerID, general, root, "@rocky actually, what's the cmd?", 0)
	h.waitFor("mention answered", func() bool { return len(h.chat.posts()) == 2 })
	h.waitFor("run over", func() bool { return !h.s.busyKey(SessionKey(general, root)) })
	h.chat.say(bobID, general, root, "and staging?", 0)
	h.waitFor("follow-up again", func() bool { return len(h.run.requests()) == runs+2 })
	h.waitFor("run over", func() bool { return !h.s.busyKey(SessionKey(general, root)) })
	clock.add(31 * time.Minute)
	h.s.wakeQuiets(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := len(h.run.requests()); n != runs+2 {
		t.Fatalf("woke after a mention ended the quiet: %d runs", n-runs)
	}

	// leave_thread: no wake either.
	root2 := h.chat.say(bobID, general, 0, "other thing", 0).ID
	quietRun(t, h, root2, map[string]any{"minutes": 5, "then": "say hey"})
	runs = len(h.run.requests())
	block := make(chan struct{})
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-block
		return runner.Result{Session: req.Session, Text: "bye"}, nil
	}
	h.chat.say(ownerID, general, root2, "@rocky never mind, bye", 0)
	if r := h.callIn(general, mcp.ToolLeaveThread, map[string]string{}); r.Error {
		t.Fatal(r.Text)
	}
	close(block)
	h.waitFor("run over", func() bool {
		return len(h.run.requests()) == runs+1 && !h.s.busyKey(SessionKey(general, root2))
	})
	clock.add(10 * time.Minute)
	h.s.wakeQuiets(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := len(h.run.requests()); n != runs+1 {
		t.Fatalf("woke after leave_thread: %d runs", n-runs)
	}
}

func TestQuietRefusals(t *testing.T) {
	h := newHarness(t, cfgDefault(), nil)
	block := make(chan struct{})
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-block
		return runner.Result{Session: req.Session, Text: "ok"}, nil
	}
	defer close(block)
	h.chat.say(ownerID, agentDM, 0, "be quiet for 5 min", 0)
	if r := h.callIn(agentDM, mcp.ToolQuiet, map[string]any{"minutes": 5}); !r.Error {
		t.Fatal("quiet in the agent DM")
	}
	h.chat.say(bobID, general, 0, "@rocky be quiet", 0)
	for _, m := range []int{0, MaxQuietMinutes + 1} {
		if r := h.callIn(general, mcp.ToolQuiet, map[string]any{"minutes": m}); !r.Error {
			t.Fatalf("quiet for %d min", m)
		}
	}
}
