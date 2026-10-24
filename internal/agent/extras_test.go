package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/agent/runner"
	"github.com/skailama/tc/internal/agent/tools"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/mcp"
	"github.com/skailama/tc/internal/proto"
)

func (f *fakeChat) Users() []proto.User {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []proto.User
	for _, u := range f.users {
		out = append(out, u)
	}
	return out
}

func (f *fakeChat) AgentByHandle(h string) (proto.Agent, bool) {
	if h == "bobbot" {
		return proto.Agent{Handle: "bobbot", Owner: bobID}, true
	}
	return proto.Agent{}, false
}

// withMembers gives the fake channels members: #general has everyone,
// #secret only the owner and carol.
func withMembers(s *Supervisor) {
	f := s.Chat.(*fakeChat)
	f.chans[0].Members = []uint64{ownerID, bobID, carolID}
	f.chans[1].Members = []uint64{ownerID, carolID}
	s.Extras = Extras{JobsEvery: 20 * time.Millisecond, AskAgentPoll: 10 * time.Millisecond, AskAgentTimeout: 2 * time.Second}
}

// hold keeps every run going until the test ends, so tools can be called.
func hold(t *testing.T, h *harness) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-block
		return runner.Result{Session: req.Session, Text: NoReply}, nil
	}
}

func jobsOf(h *harness) []tools.Job {
	jobs, _ := tools.LoadJobs(home.PathsFor(h.s.Dir).State)
	return jobs
}

func TestScheduleOwnerOnly(t *testing.T) {
	h := newHarness(t, cfgDefault(), withMembers)
	hold(t, h)
	h.chat.say(bobID, general, 0, "@rocky post hi every day", 0)
	res := h.call(mcp.ToolSchedule, map[string]string{"when": "in 1h", "text": "hi", "repeat": "daily"})
	if !res.Error || !strings.Contains(res.Text, "only your owner") {
		t.Fatalf("bob scheduled: %+v", res)
	}
	if len(jobsOf(h)) != 0 {
		t.Fatal("job saved")
	}
}

func TestScheduleChecksNamesAndFires(t *testing.T) {
	h := newHarness(t, cfgDefault(), withMembers)
	hold(t, h)
	h.chat.say(ownerID, agentDM, 0, "every weekday at 9 tell #general standup", 0)
	if res := h.call(mcp.ToolSchedule, map[string]string{"when": "9am", "chan": "#general", "text": "standup @bobb", "repeat": "weekdays"}); !res.Error || !strings.Contains(res.Text, "@bob") {
		t.Fatalf("misspelled name accepted: %+v", res)
	}
	if res := h.call(mcp.ToolSchedule, map[string]string{"when": "someday", "text": "x"}); !res.Error {
		t.Fatalf("bad time accepted: %+v", res)
	}
	res := h.call(mcp.ToolSchedule, map[string]string{"when": "9am", "chan": "#general", "text": "standup @bob @here", "repeat": "weekdays"})
	if res.Error || !strings.Contains(res.Text, "every weekday") {
		t.Fatalf("schedule: %+v", res)
	}
	jobs := jobsOf(h)
	if len(jobs) != 1 || jobs[0].Chan != general || jobs[0].Thread != 0 || jobs[0].Repeat != tools.Weekdays {
		t.Fatalf("jobs %+v", jobs)
	}
	// Make it due: it posts once and comes back on the next weekday.
	due := time.Now().Add(-time.Minute)
	_ = tools.UpdateJobs(home.PathsFor(h.s.Dir).State, func(j []tools.Job) ([]tools.Job, error) {
		j[0].At = due.UnixMilli()
		return j, nil
	})
	h.waitFor("the post", func() bool {
		for _, p := range h.chat.posts() {
			if p.ch == general && p.body == "standup @bob @here" {
				return true
			}
		}
		return false
	})
	after := jobsOf(h)
	if len(after) != 1 || !time.UnixMilli(after[0].At).After(time.Now()) {
		t.Fatalf("repeat not moved on: %+v", after)
	}
	if wd := time.UnixMilli(after[0].At).Weekday(); wd == time.Saturday || wd == time.Sunday {
		t.Fatalf("weekday job lands on %s", wd)
	}
}

func TestRemindRules(t *testing.T) {
	h := newHarness(t, cfgDefault(), withMembers)
	hold(t, h)
	ask := h.chat.say(bobID, general, 0, "@rocky remind me at 5pm to ship", 0)
	if res := h.call(mcp.ToolRemind, map[string]string{"person": "carol", "when": "in 1h", "text": "ship"}); !res.Error {
		t.Fatalf("bob reminded carol: %+v", res)
	}
	res := h.call(mcp.ToolRemind, map[string]string{"person": "me", "when": "in 1h", "text": "ship it"})
	if res.Error {
		t.Fatal(res.Text)
	}
	jobs := jobsOf(h)
	if len(jobs) != 1 || jobs[0].Person != "bob" || jobs[0].Chan != general || jobs[0].Thread != ask.ID || jobs[0].Asker != bobID {
		t.Fatalf("jobs %+v", jobs)
	}
	for i := 0; i < tools.MaxPerAsker-1; i++ {
		if r := h.call(mcp.ToolRemind, map[string]string{"person": "bob", "when": "in 2h", "text": "x"}); r.Error {
			t.Fatal(r.Text)
		}
	}
	if r := h.call(mcp.ToolRemind, map[string]string{"person": "me", "when": "in 2h", "text": "x"}); !r.Error {
		t.Fatal("per-person cap not enforced")
	}
	// Bob sees and cancels only his own.
	if r := h.call(mcp.ToolListSchedules, map[string]string{}); strings.Count(r.Text, "\n") != tools.MaxPerAsker {
		t.Fatalf("list: %q", r.Text)
	}
	if r := h.call(mcp.ToolCancelSchedule, map[string]string{"id": jobs[0].ID}); r.Error {
		t.Fatal(r.Text)
	}
	if len(jobsOf(h)) != tools.MaxPerAsker-1 {
		t.Fatal("not cancelled")
	}
}

func TestRemindFromDMUsesSharedChannel(t *testing.T) {
	h := newHarness(t, cfgDefault(), withMembers)
	hold(t, h)
	h.chat.say(ownerID, agentDM, 0, "remind carol tomorrow", 0)
	if r := h.call(mcp.ToolRemind, map[string]string{"person": "@Carol", "when": "in 10m", "text": "the review"}); r.Error {
		t.Fatal(r.Text)
	}
	if r := h.call(mcp.ToolRemind, map[string]string{"person": "carl", "when": "in 10m", "text": "x"}); !r.Error || !strings.Contains(r.Text, "@carol") {
		t.Fatalf("guessed a name: %+v", r)
	}
	if r := h.call(mcp.ToolRemind, map[string]string{"person": "me", "when": "in 10m", "text": "stretch"}); r.Error {
		t.Fatal(r.Text)
	}
	jobs := jobsOf(h)
	if len(jobs) != 2 {
		t.Fatalf("jobs %+v", jobs)
	}
	for _, j := range jobs {
		switch j.Text {
		case "the review":
			if j.Chan != secret || j.Person != "carol" { // the smallest channel the owner shares with carol
				t.Fatalf("carol's reminder %+v", j)
			}
			if got := jobText(j); got != "⏰ @carol the review (from @fizan)" {
				t.Fatalf("text %q", got)
			}
		case "stretch":
			if j.Chan != agentDM || j.Person != "" {
				t.Fatalf("own reminder %+v", j)
			}
		}
	}
}

func TestLateJobSkipped(t *testing.T) {
	h := newHarness(t, cfgDefault(), withMembers)
	st := home.PathsFor(h.s.Dir).State
	_ = tools.UpdateJobs(st, func(j []tools.Job) ([]tools.Job, error) {
		return append(j, tools.Job{ID: "sold", Kind: tools.KindSchedule, Chan: general, Text: "stale", At: time.Now().Add(-8 * time.Hour).UnixMilli(), Repeat: tools.Once}), nil
	})
	h.waitFor("dropped", func() bool { return len(jobsOf(h)) == 0 })
	for _, p := range h.chat.posts() {
		if p.body == "stale" {
			t.Fatal("posted a job missed by 8 h")
		}
	}
}

func TestCatchUp(t *testing.T) {
	h := newHarness(t, cfgDefault(), withMembers)
	hold(t, h)
	h.chat.mu.Lock()
	h.chat.msgs = append(h.chat.msgs,
		client.Message{Message: proto.Message{ID: 101, Chan: general, Author: carolID, Body: "deploy moved to friday", Time: time.Now().Add(-2 * time.Hour).UnixMilli()}},
		client.Message{Message: proto.Message{ID: 102, Chan: general, Author: carolID, Body: "ancient news", Time: time.Now().Add(-72 * time.Hour).UnixMilli()}},
		client.Message{Message: proto.Message{ID: 103, Chan: general, Author: ownerID, Thread: 102, Body: "late reply in an old thread", Time: time.Now().Add(-time.Hour).UnixMilli()}},
		client.Message{Message: proto.Message{ID: 104, Chan: secret, Author: carolID, Body: "secret plans", Time: time.Now().Add(-time.Hour).UnixMilli()}},
		client.Message{Message: proto.Message{ID: 105, Chan: general, Author: bobID, Body: "my own words", Time: time.Now().Add(-time.Hour).UnixMilli()}},
	)
	h.chat.mu.Unlock()
	h.chat.say(bobID, general, 0, "@rocky what did I miss", 0)
	res := h.call(mcp.ToolCatchUp, map[string]string{})
	if res.Error || !strings.Contains(res.Text, "deploy moved") || !strings.Contains(res.Text, "late reply") {
		t.Fatalf("catch_up: %+v", res)
	}
	if strings.Contains(res.Text, "secret plans") || strings.Contains(res.Text, "my own words") || strings.Contains(res.Text, "carol: ancient news") {
		t.Fatalf("bob saw too much: %s", res.Text)
	}
	if r := h.call(mcp.ToolCatchUp, map[string]string{"chan": "#secret"}); !r.Error {
		t.Fatalf("bob read #secret: %+v", r)
	}
	if r := h.call(mcp.ToolCatchUp, map[string]string{"since": "30m"}); r.Error || strings.Contains(r.Text, "deploy moved") {
		t.Fatalf("since ignored: %+v", r)
	}
}

func TestCatchUpOwnerSeesSecret(t *testing.T) {
	h := newHarness(t, cfgDefault(), withMembers)
	hold(t, h)
	h.chat.mu.Lock()
	h.chat.msgs = append(h.chat.msgs, client.Message{Message: proto.Message{ID: 104, Chan: secret, Author: carolID, Body: "secret plans", Time: time.Now().Add(-time.Hour).UnixMilli()}})
	h.chat.mu.Unlock()
	h.chat.say(ownerID, agentDM, 0, "what did I miss", 0)
	if r := h.call(mcp.ToolCatchUp, map[string]string{}); r.Error || !strings.Contains(r.Text, "secret plans") {
		t.Fatalf("owner catch_up: %+v", r)
	}
}

func TestAskAgent(t *testing.T) {
	h := newHarness(t, cfgDefault(), withMembers)
	hold(t, h)
	ask := h.chat.say(ownerID, general, 0, "@rocky ask bob's agent when the release is", 0)
	if r := h.call(mcp.ToolAskAgent, map[string]string{"handle": "nobody", "question": "x"}); !r.Error {
		t.Fatal("asked a missing agent")
	}
	if r := h.call(mcp.ToolAskAgent, map[string]string{"handle": "rocky", "question": "x"}); !r.Error {
		t.Fatal("asked itself")
	}
	got := make(chan mcp.BridgeRes, 1)
	go func() {
		got <- h.call(mcp.ToolAskAgent, map[string]string{"handle": "@bobbot", "question": "when is the release?"})
	}()
	var q posted
	h.waitFor("the question", func() bool {
		for _, p := range h.chat.posts() {
			if p.body == "@bobbot when is the release?" {
				q = p
				return true
			}
		}
		return false
	})
	if q.ch != general || q.thread != ask.ID {
		t.Fatalf("asked in %+v", q)
	}
	// Someone else's words in the thread are not the answer; bob's agent's are.
	h.chat.say(carolID, general, ask.ID, "no idea", 0)
	h.chat.mu.Lock()
	h.chat.nextID++
	h.chat.msgs = append(h.chat.msgs, client.Message{Message: proto.Message{ID: h.chat.nextID, Chan: general, Thread: ask.ID, Author: bobID, Agent: "bobbot", Body: "friday at 5", Time: time.Now().UnixMilli()}})
	h.chat.mu.Unlock()
	select {
	case r := <-got:
		if r.Error || !strings.Contains(r.Text, "friday at 5") {
			t.Fatalf("answer: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no answer")
	}
}

func TestAskAgentFromDMNeedsChannel(t *testing.T) {
	h := newHarness(t, cfgDefault(), withMembers)
	hold(t, h)
	h.chat.say(ownerID, agentDM, 0, "ask bobbot", 0)
	if r := h.call(mcp.ToolAskAgent, map[string]string{"handle": "bobbot", "question": "x"}); !r.Error || !strings.Contains(r.Text, "DM") {
		t.Fatalf("asked from the DM: %+v", r)
	}
	r := h.call(mcp.ToolAskAgent, map[string]string{"handle": "bobbot", "question": "ping?", "chan": "#general"})
	if r.Error || !strings.Contains(r.Text, "no answer") {
		t.Fatalf("timeout: %+v", r)
	}
	for _, p := range h.chat.posts() {
		if p.body == "@bobbot ping?" && (p.ch != general || p.thread != 0) {
			t.Fatalf("asked in %+v", p)
		}
	}
}
