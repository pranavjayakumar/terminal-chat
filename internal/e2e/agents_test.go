package e2e

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// calls collects the messages an agent client reports in Update.New.
type calls struct {
	mu   sync.Mutex
	msgs []client.Message
}

func (c *calls) watch(u *user) {
	go func() {
		for up := range u.Updates() {
			c.mu.Lock()
			c.msgs = append(c.msgs, up.New...)
			c.mu.Unlock()
		}
	}()
}

func (c *calls) got(ch uint64, body string) (client.Message, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.msgs {
		if m.Chan == ch && m.Body == body {
			return m, true
		}
	}
	return client.Message{}, false
}

func (u *user) msg(ch uint64, body string) (client.Message, bool) {
	for _, m := range u.Messages(ch, 200) {
		if m.Body == body && m.ID != 0 {
			return m, true
		}
	}
	return client.Message{}, false
}

func (u *user) reply(root uint64, body string) (client.Message, bool) {
	for _, m := range u.Thread(root) {
		if m.Body == body && m.ID != 0 {
			return m, true
		}
	}
	return client.Message{}, false
}

func protoCode(err error) string {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// startAgent logs a second device of owner in as its agent handle and runs it.
func startAgent(t *testing.T, owner *user, handle string) *user {
	t.Helper()
	home := filepath.Join(t.TempDir(), "agent")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := client.LoginAgent(ctx, owner.home, home, handle)
	if err != nil {
		t.Fatalf("LoginAgent: %v", err)
	}
	if a.Handle != handle || a.Owner != owner.Me().ID {
		t.Fatalf("LoginAgent = %+v", a)
	}
	c, err := client.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(context.Background())
	go c.Run(rctx)
	u := &user{Client: c, cancel: rcancel, home: home}
	t.Cleanup(func() { rcancel(); c.Close() })
	return u
}

// An owner, their agent (a second device logged in as @rocky) and another
// user: the agent is called by @mention in a plaintext and an encrypted
// channel, reads the call once the owner's client grants it the channel,
// replies as itself, streams a live preview the other user sees (sealed in
// the encrypted channel), and is kept out of everything else.
func TestAgentEndToEnd(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	a, b := alice.Me().ID, bob.Me().ID
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := alice.RegisterAgent(ctx, "bob", "🤖"); protoCode(err) != proto.ErrConflict {
		t.Fatalf("handle of a user: %v", err)
	}
	if _, err := alice.RegisterAgent(ctx, "rocky", "🤖"); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.RegisterAgent(ctx, "rocky2", ""); protoCode(err) != proto.ErrConflict {
		t.Fatalf("second agent: %v", err)
	}
	if _, err := bob.RegisterAgent(ctx, "rocky", ""); protoCode(err) != proto.ErrConflict {
		t.Fatalf("taken handle: %v", err)
	}

	if _, err := client.LoginAgent(ctx, bob.home, filepath.Join(t.TempDir(), "x"), "rocky"); err == nil {
		t.Fatal("bob logged in as alice's agent")
	}
	agent := startAgent(t, alice, "rocky")
	var seen calls
	seen.watch(agent)
	eventually(t, "agent online", agent.online)
	if !agent.IsAgentSession() || agent.AgentHandle() != "rocky" || agent.Me().ID != a {
		t.Fatalf("agent session: %v %q %d", agent.IsAgentSession(), agent.AgentHandle(), agent.Me().ID)
	}
	if got := agent.MentionNames(); len(got) != 1 || got[0] != "rocky" {
		t.Fatalf("agent MentionNames = %v", got)
	}
	eventually(t, "bob sees rocky online", func() bool {
		ag, ok := bob.AgentOf(a)
		return ok && ag.Handle == "rocky" && ag.Online
	})
	if ag, ok := bob.AgentByHandle("rocky"); !ok || ag.Owner != a {
		t.Fatalf("AgentByHandle = %+v %v", ag, ok)
	}

	// Plaintext: bob calls @rocky in #general.
	gen := alice.channel("general")
	eventually(t, "agent sees #general", func() bool { _, ok := agent.Channel(gen); return ok })
	if err := bob.Send(gen, "hey @rocky what's 2+2", 0); err != nil {
		t.Fatal(err)
	}
	var call client.Message
	eventually(t, "the agent gets the call", func() bool {
		var ok bool
		call, ok = seen.got(gen, "hey @rocky what's 2+2")
		return ok
	})
	if call.Author != b || call.Agent != "" {
		t.Fatalf("call = %+v", call.Message)
	}
	if err := agent.SendAgentStatus(gen, call.ID, proto.AgentStreaming, "thinking hard", "it's 4, obvi"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob sees the preview", func() bool {
		for _, l := range bob.AgentStatus(gen, call.ID) {
			if l.Agent.Handle == "rocky" && l.State == proto.AgentStreaming && l.Status == "thinking hard" && l.Preview == "it's 4, obvi" {
				return true
			}
		}
		return false
	})
	if th := bob.AgentThreads(gen); len(th) != 1 || th[0] != call.ID {
		t.Fatalf("AgentThreads = %v", th)
	}
	if err := agent.Send(gen, "it's 4", call.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob gets the agent's reply", func() bool {
		m, ok := bob.reply(call.ID, "it's 4")
		return ok && m.Agent == "rocky" && m.Author == a
	})

	// Encrypted: alice's private e2e channel with bob.
	sid := alice.sid(t)
	eventually(t, "devices verified", func() bool {
		return alice.verifiedDevices(sid, a) == 2 && bob.verifiedDevices(sid, a) == 2 &&
			agent.verifiedDevices(sid, b) == 1 && alice.verifiedDevices(sid, b) == 1
	})
	sec, err := alice.CreateEncryptedChannel(ctx, "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "secret has a key", func() bool { return alice.E2EStatus(sec.ID).State == client.E2EOK })
	if err := alice.AddMember(ctx, sec.ID, b); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob in secret", func() bool { return bob.E2EStatus(sec.ID).State == client.E2EOK })
	if err := bob.Send(sec.ID, "@rocky keep this quiet", 0); err != nil {
		t.Fatal(err)
	}
	var ecall client.Message
	eventually(t, "the agent gets the encrypted call", func() bool {
		var ok bool
		ecall, ok = seen.got(sec.ID, "@rocky keep this quiet")
		return ok
	})
	if ecall.Sec != client.SecE2E {
		t.Fatalf("encrypted call sec = %s", ecall.Sec)
	}
	eventually(t, "agent holds the key", func() bool { return agent.E2EStatus(sec.ID).State == client.E2EOK })
	if err := agent.SendAgentStatus(sec.ID, ecall.ID, proto.AgentStreaming, "", "my lips are sealed"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob opens the sealed preview", func() bool {
		for _, l := range bob.AgentStatus(sec.ID, ecall.ID) {
			if l.Preview == "my lips are sealed" {
				return true
			}
		}
		return false
	})
	if err := agent.Send(sec.ID, "sealed", ecall.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob reads the agent's encrypted reply", func() bool {
		m, ok := bob.reply(ecall.ID, "sealed")
		return ok && m.Sec == client.SecE2E && m.Agent == "rocky"
	})

	// The access filter: a channel nobody called the agent in stays shut.
	hidden, err := alice.CreateChannel(ctx, "hidden", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.AddMember(ctx, hidden.ID, b); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob in hidden", func() bool { _, ok := bob.Channel(hidden.ID); return ok })
	if err := bob.Send(hidden.ID, "just us", 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice reads hidden", func() bool { return alice.has(hidden.ID, "just us") })
	if _, err := agent.History(ctx, hidden.ID, 0); protoCode(err) != proto.ErrForbidden {
		t.Fatalf("agent history of hidden: %v", err)
	}
	if _, err := agent.Search(ctx, "just", 0); protoCode(err) != proto.ErrForbidden {
		t.Fatalf("agent search everywhere: %v", err)
	}
	if agent.has(hidden.ID, "just us") {
		t.Fatal("the agent received a channel it may not read")
	}
	if _, err := agent.SetAgentAccess(ctx, []uint64{hidden.ID}, nil); err == nil {
		t.Fatal("the agent granted itself access")
	}

	// DMs: nobody DMs through an agent, bob has no agent DM, and the owner's
	// agent DM is the owner's and the agent's alone.
	if _, err := agent.OpenDM(ctx, []uint64{b}); protoCode(err) != proto.ErrForbidden {
		t.Fatalf("agent dm.open: %v", err)
	}
	if _, err := bob.OpenAgentDM(ctx); protoCode(err) != proto.ErrNotFound {
		t.Fatalf("bob's agent DM: %v", err)
	}
	dm, err := alice.OpenAgentDM(ctx)
	if err != nil || dm.Kind != proto.ChanAgentDM {
		t.Fatalf("OpenAgentDM: %+v %v", dm, err)
	}
	if again, err := alice.OpenAgentDM(ctx); err != nil || again.ID != dm.ID {
		t.Fatalf("second OpenAgentDM: %+v %v", again, err)
	}
	eventually(t, "agent DM keyed", func() bool { return alice.E2EStatus(dm.ID).State == client.E2EOK })
	eventually(t, "agent sees its DM", func() bool { cv, ok := agent.AgentDM(); return ok && cv.ID == dm.ID })
	if err := agent.Send(dm.ID, "hi! I'm rocky", 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice gets the greeting", func() bool {
		m, ok := alice.msg(dm.ID, "hi! I'm rocky")
		return ok && m.Agent == "rocky" && m.Sec == client.SecE2E
	})
	if _, ok := bob.Channel(dm.ID); ok {
		t.Fatal("bob sees alice's agent DM")
	}

	// Refused to agents: admin powers and agent ops.
	if _, err := agent.CreateInvite(ctx, 0); protoCode(err) != proto.ErrForbidden {
		t.Fatalf("agent invite: %v", err)
	}
	if err := agent.RetireAgent(ctx); protoCode(err) != proto.ErrForbidden {
		t.Fatalf("agent retire: %v", err)
	}

	// The agent never makes its owner look online.
	alice.cancel()
	eventually(t, "alice offline for bob", func() bool { return bob.Presence(a).State == proto.PresOffline })
	if ag, ok := bob.AgentOf(a); !ok || !ag.Online {
		t.Fatal("the agent went offline with its owner's client")
	}
}
