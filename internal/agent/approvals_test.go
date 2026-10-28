package agent

import (
	"strings"
	"testing"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/agent/runner"
	"github.com/skailama/tc/internal/mcp"
)

func TestCardBody(t *testing.T) {
	got := CardBody("rocky", Approval{Action: "post in #general", Preview: "hey @fahim\nline 2\nline 3\nline 4", Why: `fizan asked in your DM: "send fahim a note"`}, "#abc")
	want := "✋ Rocky wants to post in #general  ·  #abc\n> hey @fahim\n> line 2\n> line 3 …\nwhy: fizan asked in your DM: \"send fahim a note\""
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := CardBody("rocky", Approval{Action: "push it"}, "#x"); got != "✋ Rocky wants to push it  ·  #x" {
		t.Fatal(got)
	}
}

func TestOwnersAskToPostIsTheApproval(t *testing.T) {
	h := newHarness(t, cfgDefault(), nil)
	block := make(chan struct{})
	defer close(block)
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-block
		return runner.Result{Session: req.Session, Text: NoReply}, nil
	}
	h.chat.say(ownerID, agentDM, 0, "lets send fahim a message on general", 0)
	for i, text := range []string{"hey fahim, fizan says hi", "and one more thing"} {
		if r := h.call(mcp.ToolPostReply, map[string]any{"chan": "#general", "text": text}); r.Error || r.Text != "posted" {
			t.Fatalf("post %d: %+v", i, r)
		}
	}
	for _, p := range h.chat.posts() {
		if strings.HasPrefix(p.body, "✋") {
			t.Fatalf("asked the owner about their own ask: %+v", p)
		}
	}
	if ps := h.chat.posts(); len(ps) != 2 || ps[0].ch != general {
		t.Fatalf("posts %+v", ps)
	}
}

// A ♾️ is the owner's ok for their own asks: it never waves through
// someone else's ask, and a tool grant covers only that exact call.
func TestGrantsOwnerAsksExactTarget(t *testing.T) {
	cfg := cfgDefault()
	cfg.Approvals = home.ApproveAlways
	h := newHarness(t, cfg, nil)
	block := make(chan struct{})
	defer close(block)
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-block
		return runner.Result{Session: req.Session, Text: NoReply}, nil
	}
	card := func(seen int) uint64 {
		var body string
		h.waitFor("card", func() bool {
			for _, p := range h.chat.posts()[seen:] {
				if strings.HasPrefix(p.body, "✋ ") {
					body = p.body
					return true
				}
			}
			return false
		})
		return h.cardID(body)
	}
	async := func(ch uint64, tool string, args any) chan mcp.BridgeRes {
		res := make(chan mcp.BridgeRes, 1)
		go func() { res <- h.callIn(ch, tool, args) }()
		return res
	}

	// Bob's ask: the owner's ♾️ on its card isn't kept, the next post
	// asks again.
	root := h.chat.say(carolID, general, 0, "deploy thread", 0)
	h.chat.say(bobID, general, root.ID, "@rocky tell everyone the deploy is done", 0)
	post := func(text string) chan mcp.BridgeRes {
		return async(general, mcp.ToolPostReply, map[string]any{"chan": "#general", "text": text})
	}
	n := len(h.chat.posts())
	res := post("deploy is done 🎉")
	id := card(n)
	h.chat.react(id, "♾️", ownerID)
	h.chat.react(id, "✅", ownerID)
	if r := <-res; r.Text != "posted" {
		t.Fatalf("first post %+v", r)
	}
	if g := h.s.Grants(); len(g) != 0 {
		t.Fatalf("a stranger's card left a grant: %+v", g)
	}
	n = len(h.chat.posts())
	res = post("really done")
	id = card(n)
	h.chat.react(id, "✅", ownerID)
	if r := <-res; r.Text != "posted" {
		t.Fatalf("second post %+v", r)
	}

	// The owner's ask: ♾️ on a shell call covers that exact call only.
	h.chat.say(ownerID, agentDM, 0, "run the tests", 0)
	approve := func(cmd string) chan mcp.BridgeRes {
		return async(agentDM, mcp.ToolApprove, map[string]any{"tool_name": "Bash", "input": map[string]any{"command": cmd}})
	}
	n = len(h.chat.posts())
	res = approve("go test ./...")
	id = card(n)
	h.chat.react(id, "♾️", ownerID)
	h.chat.react(id, "✅", ownerID)
	if r := <-res; !strings.Contains(r.Text, "allow") {
		t.Fatalf("approve %+v", r)
	}
	g := h.s.Grants()
	if len(g) != 1 || g[0].Kind != KindTool || g[0].Target != ToolTarget("Bash", []byte(`{"command":"go test ./..."}`)) {
		t.Fatalf("grants %+v", g)
	}
	if GrantText(g[0]) != "that exact Bash call" {
		t.Fatal(GrantText(g[0]))
	}
	n = len(h.chat.posts())
	if r := <-approve("go test ./..."); !strings.Contains(r.Text, "allow") || len(h.chat.posts()) != n {
		t.Fatalf("same call asked again: %+v %+v", r, h.chat.posts()[n:])
	}
	res = approve("rm -rf ~")
	id = card(n)
	h.chat.react(id, "❌", ownerID)
	if r := <-res; !strings.Contains(r.Text, "deny") {
		t.Fatalf("another call rode the grant: %+v", r)
	}
	// And the owner's grant never covers bob's ask.
	n = len(h.chat.posts())
	res = async(general, mcp.ToolApprove, map[string]any{"tool_name": "Bash", "input": map[string]any{"command": "go test ./..."}})
	id = card(n)
	h.chat.react(id, "❌", ownerID)
	if r := <-res; !strings.Contains(r.Text, "deny") {
		t.Fatalf("bob's ask rode the owner's grant: %+v", r)
	}
}

func TestNeedsApproval(t *testing.T) {
	for _, c := range []struct {
		level home.Approvals
		kind  string
		want  bool
	}{
		{home.ApproveAlways, KindPost, true}, {home.ApproveAlways, KindPR, true},
		{home.ApproveWorkOnly, KindPost, false}, {home.ApproveWorkOnly, KindPR, true},
		{home.ApproveWorkOnly, KindIssue, true}, {home.ApproveWorkOnly, KindTool, true}, {home.ApproveWorkOnly, KindAsk, true},
		{home.ApproveNever, KindPR, false}, {home.ApproveNever, KindTool, false},
	} {
		if got := NeedsApproval(c.level, c.kind); got != c.want {
			t.Errorf("%s %s: %v", c.level, c.kind, got)
		}
	}
	if home.DefaultConfig().Approvals != home.ApproveWorkOnly {
		t.Fatal("default isn't work_only")
	}
}

func TestWorkOnlyPostsWithoutAsking(t *testing.T) {
	cfg := cfgDefault()
	cfg.Access.Chans = []uint64{secret}
	h := newHarness(t, cfg, nil)
	block := make(chan struct{})
	defer close(block)
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-block
		return runner.Result{Session: req.Session, Text: NoReply}, nil
	}
	h.chat.setMembers(secret, ownerID, bobID)
	h.chat.say(bobID, general, 0, "@rocky tell #secret hi", 0)
	if r := h.call(mcp.ToolPostReply, map[string]any{"chan": "#secret", "text": "hi from bob"}); r.Text != "posted" {
		t.Fatalf("post %+v", r)
	}
	for _, p := range h.chat.posts() {
		if strings.HasPrefix(p.body, "✋") {
			t.Fatalf("work_only asked about a post: %+v", p)
		}
	}
}

func TestNeverAsks(t *testing.T) {
	cfg := cfgDefault()
	cfg.Approvals = home.ApproveNever
	h := newHarness(t, cfg, nil)
	block := make(chan struct{})
	defer close(block)
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-block
		return runner.Result{Session: req.Session, Text: NoReply}, nil
	}
	h.chat.say(bobID, general, 0, "@rocky can you push?", 0)
	if r := h.call(mcp.ToolAskOwner, map[string]any{"action": "push"}); r.Text != "yes" {
		t.Fatalf("ask_owner %+v", r)
	}
	if len(h.chat.posts()) != 0 {
		t.Fatalf("posted a card: %+v", h.chat.posts())
	}
}
