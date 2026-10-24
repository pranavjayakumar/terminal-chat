package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/agent/runner"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/mcp"
)

func TestWatch(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	h := newHarness(t, cfgDefault(), func(s *Supervisor) { s.Now = clock.now })
	block := make(chan struct{})
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		switch {
		case strings.Contains(req.Prompt, "asked you to watch"):
			if strings.Contains(req.Prompt, "@bob") && strings.Contains(req.Prompt, "is the build ok") {
				return runner.Result{Session: req.Session, Text: "@bob yep, green"}, nil
			}
			return runner.Result{Session: req.Session, Text: NoReply}, nil
		}
		<-block
		return runner.Result{Session: req.Session, Text: "on it"}, nil
	}
	h.chat.say(ownerID, agentDM, 0, "watch #general for 5 min and answer bob", 0)
	if r := h.call(mcp.ToolWatch, map[string]any{"chan": "#general", "minutes": 500, "about": "x"}); !r.Error {
		t.Fatal("watch over the cap")
	}
	r := h.call(mcp.ToolWatch, map[string]any{"chan": "#general", "minutes": 5, "about": "answer bob about the build"})
	if r.Error || !strings.Contains(r.Text, "watching #general for 5 min") {
		t.Fatalf("watch: %+v", r)
	}
	close(block)
	h.waitFor("DM reply", func() bool { return len(h.chat.posts()) == 1 })
	if w := h.s.Watches(); len(w) != 1 || w[0].About != "answer bob about the build" {
		t.Fatalf("watches %+v", w)
	}
	h.waitFor("status lists it", func() bool {
		st, err := ReadStatus(h.s.paths.State)
		return err == nil && len(st.Watches) == 1
	})

	// A top-level message in the channel: answered in a thread on it,
	// with the owner's instruction in the prompt.
	q := h.chat.say(bobID, general, 0, "is the build ok?", 0)
	h.waitFor("watch reply", func() bool { return len(h.chat.posts()) == 2 })
	if p := h.chat.posts()[1]; p.ch != general || p.thread != q.ID || p.body != "@bob yep, green" {
		t.Fatalf("reply %+v", p)
	}
	req := h.run.requests()[1]
	if !strings.Contains(req.Prompt, `"answer bob about the build"`) || !strings.Contains(req.Prompt, "@bob") || strings.Contains(req.Prompt, memorySaysOwner) {
		t.Fatalf("prompt %q", req.Prompt)
	}
	if !strings.Contains(strings.Join(req.Env, " "), "TC_AGENT_OWNER_ASK=0") {
		t.Fatal("the watch run took the owner's trust")
	}
	// Something not for the agent: nothing posted.
	h.chat.say(carolID, general, 0, "lunch?", 0)
	h.waitFor("third run", func() bool { return len(h.run.requests()) == 3 })
	time.Sleep(30 * time.Millisecond)
	if len(h.chat.posts()) != 2 {
		t.Fatalf("posted %+v", h.chat.posts())
	}

	// Time's up: one summary line in the owner's DM.
	clock.add(6 * time.Minute)
	h.s.expireWatches()
	h.waitFor("summary", func() bool { return len(h.chat.posts()) == 3 })
	sum := h.chat.posts()[2]
	if sum.ch != agentDM || sum.body != "watched #general 5 min: bob and carol wrote, I answered once" {
		t.Fatalf("summary %+v", sum)
	}
	h.chat.say(bobID, general, 0, "still there?", 0)
	time.Sleep(30 * time.Millisecond)
	if len(h.run.requests()) != 3 {
		t.Fatal("ran after the watch ended")
	}
}

// memorySaysOwner would mark a run as the owner's own ask.
const memorySaysOwner = "(your owner) asked you in"

func TestWatchOwnerOnlyAndUnwatch(t *testing.T) {
	h := newHarness(t, cfgDefault(), nil)
	block := make(chan struct{})
	defer close(block)
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-block
		return runner.Result{Session: req.Session, Text: "ok"}, nil
	}
	h.chat.say(bobID, general, 0, "@rocky watch #secret for me", 0)
	if r := h.call(mcp.ToolWatch, map[string]any{"chan": "#general", "minutes": 5, "about": "x"}); !r.Error {
		t.Fatal("bob started a watch")
	}
	if r := h.call(mcp.ToolUnwatch, map[string]any{"chan": "#general"}); !r.Error {
		t.Fatal("bob stopped a watch")
	}
	h.s.startWatch(mustChan(t, h, "#general"), 0, 5, "x")
	before := len(h.chat.posts())
	if n := h.s.stopWatches(general, 0, "stopped"); n != 1 {
		t.Fatalf("stopped %d", n)
	}
	h.waitFor("summary", func() bool { return len(h.chat.posts()) == before+1 })
	if p := h.chat.posts()[before]; p.ch != agentDM || !strings.Contains(p.body, "nobody said anything") {
		t.Fatalf("summary %+v", p)
	}
}

func mustChan(t *testing.T, h *harness, name string) client.ChannelView {
	t.Helper()
	cv, err := mcp.ResolveChan(h.chat, name)
	if err != nil {
		t.Fatal(err)
	}
	return cv
}

func TestWatchSummary(t *testing.T) {
	start := time.Now()
	w := &Watch{Where: "#general", Started: start.UnixMilli(), Until: start.Add(5 * time.Minute).UnixMilli(), authors: []string{"a", "b", "c"}, replies: 3}
	if got := WatchSummary(w, start.Add(2*time.Minute)); got != "watched #general 2 min: a, b and c wrote, I answered 3 times" {
		t.Fatal(got)
	}
	w.authors, w.replies = []string{"a"}, 0
	if got := WatchSummary(w, start.Add(time.Hour)); got != "watched #general 5 min: a wrote, I stayed quiet" {
		t.Fatal(got)
	}
}
