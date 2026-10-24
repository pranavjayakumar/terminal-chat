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

func TestGrantsAreKindAndTarget(t *testing.T) {
	cfg := cfgDefault()
	cfg.Mode = home.ModeCrew
	cfg.Access.Chans = []uint64{secret}
	h := newHarness(t, cfg, nil)
	block := make(chan struct{})
	defer close(block)
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-block
		return runner.Result{Session: req.Session, Text: NoReply}, nil
	}
	// Bob's ask in a thread of #general wants to post at the top of
	// #general: a card, every time, until the owner says ♾️.
	h.chat.say(bobID, general, 0, "@rocky tell everyone the deploy is done", 0)
	post := func(ch, text string) chan mcp.BridgeRes {
		res := make(chan mcp.BridgeRes, 1)
		go func() { res <- h.call(mcp.ToolPostReply, map[string]any{"chan": ch, "text": text}) }()
		return res
	}
	card := func(sub string) uint64 {
		var body string
		h.waitFor("card "+sub, func() bool {
			for _, p := range h.chat.posts() {
				if strings.HasPrefix(p.body, "✋ Rocky wants to post in "+sub) && !strings.Contains(body, p.body) {
					body = p.body
					return true
				}
			}
			return false
		})
		return h.cardID(body)
	}
	res := post("#general", "deploy is done 🎉")
	id := card("#general")
	h.chat.react(id, "♾️", ownerID)
	h.chat.react(id, "✅", ownerID)
	if r := <-res; r.Text != "posted" {
		t.Fatalf("first post %+v", r)
	}
	if g := h.s.Grants(); len(g) != 1 || g[0].Kind != KindPost || g[0].Target != "#general" {
		t.Fatalf("grants %+v", g)
	}
	// Same kind, same target: no card.
	before := len(h.chat.posts())
	if r := <-post("#general", "really done"); r.Text != "posted" {
		t.Fatalf("second post %+v", r)
	}
	if n := len(h.chat.posts()); n != before+1 {
		t.Fatalf("asked again: %+v", h.chat.posts()[before:])
	}
	// Another target: a card again.
	res = post("#secret", "psst")
	id = card("#secret")
	h.chat.react(id, "❌", ownerID)
	if r := <-res; !strings.Contains(r.Text, "said no") {
		t.Fatalf("secret post %+v", r)
	}
	st, err := ReadStatus(h.s.paths.State)
	if err != nil || len(st.Grants) != 1 {
		t.Fatalf("status grants %+v %v", st.Grants, err)
	}
	if GrantText(st.Grants[0]) != "posting in #general" {
		t.Fatal(GrantText(st.Grants[0]))
	}
}
