package server

import (
	"crypto/rand"
	"testing"

	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

var agentCaps = []string{proto.CapAgent}

// connectAgent logs owner's key in as its agent handle; bye is the BYE code
// when refused.
func (ts *testServer) connectAgent(t *testing.T, owner *tclient, handle string) (*tclient, string) {
	t.Helper()
	conn, codec := ts.dial(t)
	exp, err := conn.Exporter()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := identity.Sign(owner.signer, exp)
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Write(proto.THello, proto.Hello{Client: "test", Caps: agentCaps, Agent: handle,
		Auth: proto.Auth{Pubkey: owner.signer.PublicKey().Marshal(), Sig: sig}}); err != nil {
		t.Fatal(err)
	}
	f, err := codec.Read()
	if err != nil {
		t.Fatalf("reading welcome: %v", err)
	}
	if f.Type == proto.TBye {
		var b proto.Bye
		f.Decode(&b)
		return nil, b.Code
	}
	var w proto.Welcome
	if f.Type != proto.TWelcome || f.Decode(&w) != nil || w.Agent == nil || w.Agent.Handle != handle {
		t.Fatalf("agent WELCOME: %#x %+v", f.Type, w.Agent)
	}
	codec.SetMaxFrame(proto.MaxFrame)
	c := &tclient{t: t, conn: conn, codec: codec, frames: make(chan proto.Frame, 1024), signer: owner.signer, user: w.User, welcome: w}
	go func() {
		defer close(c.frames)
		for {
			f, err := c.codec.Read()
			if err != nil {
				return
			}
			c.frames <- f
		}
	}()
	return c, ""
}

func (c *tclient) agentEvent(handle string) proto.Agent {
	c.t.Helper()
	for {
		ev := c.event(proto.KAgentUpdate)
		var a proto.Agent
		if err := proto.Unmarshal(ev.Data, &a); err != nil {
			c.t.Fatal(err)
		}
		if a.Handle == handle {
			return a
		}
	}
}

func TestAgentSessionRules(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, agentCaps)
	snap, _ := alice.syncFresh()
	gen := generalID(t, snap)
	bob := ts.registerCaps(t, "bob", false, agentCaps)
	bob.syncFresh()

	if _, bye := ts.connectAgent(t, alice, "rocky"); bye != proto.ByeAuthFailed {
		t.Fatalf("unregistered agent: BYE %q", bye)
	}
	var reg proto.AgentRes
	alice.ok(proto.OpAgentRegister, proto.AgentRegisterArgs{Handle: "rocky", Icon: "🤖"}, &reg)
	bob.agentEvent("rocky")
	if _, bye := ts.connectAgent(t, bob, "rocky"); bye != proto.ByeAuthFailed {
		t.Fatalf("someone else's agent: BYE %q", bye)
	}
	agent, bye := ts.connectAgent(t, alice, "rocky")
	if bye != "" {
		t.Fatalf("agent login: BYE %q", bye)
	}
	agent.syncFresh()
	alice.ok(proto.OpAgentAccess, proto.AgentAccessArgs{Add: []uint64{gen}}, nil)

	// Reactions are the owner's approvals: an agent's would pass for a tap.
	msg := bob.send(gen, "approve?").Msg
	agent.fail(proto.OpReact, proto.ReactArgs{Msg: msg, Emoji: "✅"}, proto.ErrForbidden)
	agent.fail(proto.OpUnreact, proto.ReactArgs{Msg: msg, Emoji: "✅"}, proto.ErrForbidden)
	agent.fail(proto.OpAgentRetire, proto.Empty{}, proto.ErrForbidden)
	agent.fail(proto.OpAgentAccess, proto.AgentAccessArgs{Add: []uint64{gen}}, proto.ErrForbidden)
	agent.ok(proto.OpAgentAccess, proto.AgentAccessArgs{}, nil)

	// The agent renames itself; everyone with agent1 hears it at once.
	var up proto.AgentRes
	agent.ok(proto.OpAgentUpdate, proto.AgentUpdateArgs{Handle: "bolt", Icon: "⚡"}, &up)
	if up.Agent.Handle != "bolt" || up.Agent.Icon != "⚡" || up.Agent.Owner != alice.user.ID {
		t.Fatalf("rename result %+v", up.Agent)
	}
	if a := bob.agentEvent("bolt"); a.Owner != alice.user.ID {
		t.Fatalf("bob's event %+v", a)
	}
	alice.agentEvent("bolt")
	// Its messages carry the new handle.
	nonce := make([]byte, 16)
	rand.Read(nonce)
	agent.ok(proto.OpSend, proto.SendArgs{Chan: gen, Body: "call me bolt", Nonce: nonce}, nil)
	for {
		ev := bob.event(proto.KMsgNew)
		var m proto.Message
		proto.Unmarshal(ev.Data, &m)
		if m.Body == "call me bolt" {
			if m.Agent != "bolt" {
				t.Fatalf("message agent %q", m.Agent)
			}
			break
		}
	}

	// Clashes: a user's name, another agent's handle, and an old handle
	// (kept by its owner, so old mentions never point at someone else).
	agent.fail(proto.OpAgentUpdate, proto.AgentUpdateArgs{Handle: "bob"}, proto.ErrConflict)
	agent.fail(proto.OpAgentUpdate, proto.AgentUpdateArgs{Handle: "Bad Name"}, proto.ErrBadRequest)
	bob.ok(proto.OpAgentRegister, proto.AgentRegisterArgs{Handle: "sparky"}, nil)
	agent.fail(proto.OpAgentUpdate, proto.AgentUpdateArgs{Handle: "sparky"}, proto.ErrConflict)
	bob.fail(proto.OpAgentUpdate, proto.AgentUpdateArgs{Handle: "rocky"}, proto.ErrConflict)

	// 5 renames a day: rocky → bolt was one; back to rocky is allowed (its own).
	for _, h := range []string{"rocky", "bolt2", "bolt3", "bolt4"} {
		agent.ok(proto.OpAgentUpdate, proto.AgentUpdateArgs{Handle: h}, nil)
	}
	agent.fail(proto.OpAgentUpdate, proto.AgentUpdateArgs{Handle: "bolt5"}, proto.ErrRateLimited)
	agent.ok(proto.OpAgentUpdate, proto.AgentUpdateArgs{Icon: "🦊"}, nil) // icons aren't renames
	alice.fail(proto.OpAgentUpdate, proto.AgentUpdateArgs{Handle: "bolt5"}, proto.ErrRateLimited)
}

func TestVia(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, agentCaps)
	snap, _ := alice.syncFresh()
	gen := generalID(t, snap)
	old := ts.register(t, "old", false)
	old.syncFresh()

	send := func(c *tclient, body, via string) {
		nonce := make([]byte, 16)
		rand.Read(nonce)
		c.ok(proto.OpSend, proto.SendArgs{Chan: gen, Body: body, Nonce: nonce, Via: via}, nil)
	}
	got := func(body string) proto.Message {
		for {
			ev := alice.event(proto.KMsgNew)
			var m proto.Message
			proto.Unmarshal(ev.Data, &m)
			if m.Body == body {
				return m
			}
		}
	}
	send(alice, "from claude", proto.ViaMCP)
	if m := got("from claude"); m.Via != proto.ViaMCP {
		t.Fatalf("via %q", m.Via)
	}
	send(old, "no cap", proto.ViaMCP) // only agent1 sessions mark messages
	if m := got("no cap"); m.Via != "" {
		t.Fatalf("via without cap %q", m.Via)
	}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	alice.fail(proto.OpSend, proto.SendArgs{Chan: gen, Body: "x", Nonce: nonce, Via: "Not Valid"}, proto.ErrBadRequest)
	var h proto.HistoryRes
	alice.ok(proto.OpHistory, proto.HistoryArgs{Chan: gen, Limit: 10}, &h)
	for _, m := range h.Msgs {
		if m.Body == "from claude" && m.Via != proto.ViaMCP {
			t.Fatalf("stored via %q", m.Via)
		}
	}
}
