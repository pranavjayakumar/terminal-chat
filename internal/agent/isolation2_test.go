package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/agent/runner"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/mcp"
	"github.com/skailama/tc/internal/proto"
)

type groupFakeChat struct{ *fakeChat }

func (groupFakeChat) Groups() []proto.Group {
	return []proto.Group{{Name: "eng", Members: []uint64{bobID, carolID}}}
}

// tc clients treat "@eng" followed by '.' as a mention of group eng
// (client.mentions: the next byte only has to be a non-word, non-'-'),
// but the agent's regex swallows ".team" into the name, so "eng.team"
// isn't a group and nothing is defanged or refused.
func TestMassMentionMatchesClients(t *testing.T) {
	s := &Supervisor{Chat: groupFakeChat{newFakeChat()}}
	for _, text := range []string{"heads up @eng.team: deploy", "cc @eng.x"} {
		if !client.Mentioned(text, []string{"eng"}) {
			t.Fatalf("precondition: %q should ping eng", text)
		}
		if out := s.defangBroadcast(text); client.Mentioned(out, []string{"eng"}) {
			t.Errorf("defangBroadcast(%q) = %q still pings @eng", text, out)
		}
		if s.errMass(text) == nil {
			t.Errorf("errMass(%q) let a group ping through", text)
		}
	}
}

// Follow-up runs that end in NO_REPLY (people chatting among themselves in
// a thread the agent is engaged in) burn their asks_per_person: someone who
// never addressed the agent is then refused on their first @mention.
func TestSilentFollowUpsKeepAsks(t *testing.T) {
	cfg := cfgDefault()
	cfg.AsksPerPerson = 2
	h := newHarness(t, cfg, nil)
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		if strings.Contains(req.Prompt, "Follow-up in a thread") {
			return runner.Result{Session: req.Session, Text: NoReply}, nil
		}
		return runner.Result{Session: req.Session, Text: "ok"}, nil
	}
	h.chat.say(ownerID, general, 0, "@rocky status?", 0)
	h.waitFor("engaged", func() bool { return h.s.Engaged(general, 0) })
	h.chat.say(bobID, general, 0, "carol, lunch?", 0)
	h.waitFor("follow-up 1", func() bool { return len(h.run.requests()) == 2 })
	h.chat.say(bobID, general, 0, "noon works", 0)
	h.waitFor("follow-up 2", func() bool { return len(h.run.requests()) == 3 })
	time.Sleep(50 * time.Millisecond)
	h.chat.say(bobID, general, 0, "@rocky what's the deploy status?", 0)
	h.waitFor("bob's answer", func() bool { return len(h.chat.posts()) == 2 })
	if p := h.chat.posts()[1].body; p != "ok" {
		t.Fatalf("bob's first real ask was refused: %q", p)
	}
}

// search without chan stops after 30 readable channels in list order, so
// it can miss the very channel the ask came from.
func TestSearchAllAskChannelFirst(t *testing.T) {
	h := newHarness(t, cfgDefault(), nil)
	hold(t, h)
	const late = 500
	h.chat.mu.Lock()
	for i := range 35 {
		h.chat.chans = append(h.chat.chans, client.ChannelView{Channel: proto.Channel{ID: uint64(100 + i), Name: fmt.Sprintf("c%d", i), Kind: proto.ChanPublic}, Member: true})
	}
	h.chat.chans = append(h.chat.chans, client.ChannelView{Channel: proto.Channel{ID: late, Name: "late", Kind: proto.ChanPublic}, Member: true})
	h.chat.msgs = append(h.chat.msgs, client.Message{Message: proto.Message{ID: 2, Chan: late, Author: carolID, Body: "needle here", Time: time.Now().UnixMilli()}})
	h.chat.mu.Unlock()
	h.chat.say(ownerID, late, 0, "@rocky find the needle", 0)
	r := h.callIn(late, mcp.ToolSearch, map[string]string{"q": "needle here"})
	if r.Error || !strings.Contains(r.Text, "needle here") {
		t.Fatalf("search from #late missed #late: %+v", r)
	}
}
