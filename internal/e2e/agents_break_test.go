package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

func brAgentSetup(t *testing.T) (alice, bob, agent *user, ctx context.Context) {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice = login(t, s, addr, "alice", true)
	bob = login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	if _, err := alice.RegisterAgent(ctx, "rocky", ""); err != nil {
		t.Fatal(err)
	}
	agent = startAgent(t, alice, "rocky")
	eventually(t, "agent online", agent.online)
	return alice, bob, agent, ctx
}

// The owner renames the agent while the agent's program is not running (the
// usual case: the TUI is open, the agent harness is not). The agent home's
// config still says "rocky", the server answers HELLO agent=rocky with BYE
// auth_failed, and the client treats auth_failed as fatal: the agent can
// never log in again without a fresh LoginAgent.
func TestAgents_BreakRenameWhileAgentOffline(t *testing.T) {
	alice, _, agent, ctx := brAgentSetup(t)
	home := agent.home
	agent.cancel()
	agent.Close()
	eventually(t, "server sees the agent gone", func() bool {
		a, ok := alice.MyAgent()
		return ok && !a.Online
	})
	if _, err := alice.UpdateAgent(ctx, "rex", ""); err != nil {
		t.Fatal(err)
	}

	c, err := client.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(context.Background())
	go c.Run(rctx)
	t.Cleanup(func() { rcancel(); c.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c.Status().State == client.Online {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("renamed agent never reconnected: status %+v, handle %q", c.Status(), c.AgentHandle())
}

// An agent that dies (crash, kill, network drop) mid-reply leaves its live
// status behind: the server announces agent.presence offline, but clients
// keep showing "streaming" with the partial preview for the full 90 s TTL.
func TestAgents_BreakLiveStatusOutlivesAgent(t *testing.T) {
	alice, bob, agent, _ := brAgentSetup(t)
	gen := alice.channel("general")
	if _, err := alice.SetAgentAccess(context.Background(), []uint64{gen}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "agent sees #general", func() bool { _, ok := agent.Channel(gen); return ok })
	eventually(t, "bob knows the agent", func() bool { a, ok := bob.AgentOf(alice.Me().ID); return ok && a.Online })
	eventually(t, "bob sees the preview", func() bool {
		if len(bob.AgentStatus(gen, 0)) == 1 {
			return true
		}
		_ = agent.SendAgentStatus(gen, 0, proto.AgentStreaming, "writing", "half a sente")
		return false
	})

	agent.cancel()
	agent.Close()
	eventually(t, "bob sees the agent offline", func() bool {
		a, ok := bob.AgentOf(alice.Me().ID)
		return ok && !a.Online
	})
	time.Sleep(3 * time.Second)
	if l := bob.AgentStatus(gen, 0); len(l) != 0 {
		t.Fatalf("agent is offline but bob still shows it live: %+v", l)
	}
}

// Same, through retire: the agent is retired (its sessions closed) while it
// streams; its status stays on screen for 90 s under a handle that no
// longer names an active agent.
func TestAgents_BreakLiveStatusOutlivesRetire(t *testing.T) {
	alice, bob, agent, ctx := brAgentSetup(t)
	gen := alice.channel("general")
	if _, err := alice.SetAgentAccess(context.Background(), []uint64{gen}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "agent sees #general", func() bool { _, ok := agent.Channel(gen); return ok })
	eventually(t, "bob knows the agent", func() bool { a, ok := bob.AgentOf(alice.Me().ID); return ok && a.Online })
	eventually(t, "bob sees the status", func() bool {
		if len(bob.AgentStatus(gen, 0)) == 1 {
			return true
		}
		_ = agent.SendAgentStatus(gen, 0, proto.AgentWorking, "running tests", "")
		return false
	})
	if err := alice.RetireAgent(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob sees the retirement", func() bool { _, ok := bob.AgentOf(alice.Me().ID); return !ok })
	time.Sleep(3 * time.Second)
	if l := bob.AgentStatus(gen, 0); len(l) != 0 {
		t.Fatalf("retired agent still shown live to bob: %+v", l)
	}
}

// The agent's replies have author == owner, and unread counting (views.go
// "m.author != me") skips them: the owner asks in the agent DM, the agent
// answers, and the owner's client never shows the DM as unread.
func TestAgents_BreakAgentReplyNeverUnread(t *testing.T) {
	alice, _, agent, ctx := brAgentSetup(t)
	dm, err := alice.OpenAgentDM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "agent DM keyed", func() bool { return alice.E2EStatus(dm.ID).State == client.E2EOK })
	eventually(t, "agent sees its DM", func() bool { cv, ok := agent.AgentDM(); return ok && cv.ID == dm.ID })
	eventually(t, "agent holds the DM key", func() bool { return agent.E2EStatus(dm.ID).State == client.E2EOK })
	if err := agent.Send(dm.ID, "done: the build is green", 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice gets the reply", func() bool {
		m, ok := alice.msg(dm.ID, "done: the build is green")
		return ok && m.Agent == "rocky"
	})
	cv, ok := alice.Channel(dm.ID)
	if !ok {
		t.Fatal("no agent DM")
	}
	if cv.Unread == 0 {
		t.Fatalf("the agent's reply is not unread for its owner: %+v", cv)
	}
}
