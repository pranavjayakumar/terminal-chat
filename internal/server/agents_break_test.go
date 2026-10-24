package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

var brAgentCaps = []string{proto.CapAgent, proto.CapE2E}

// brAgentHandshake dials and sends HELLO agent=handle with key. It returns
// the client (with a background reader) or the BYE code.
func brAgentHandshake(t *testing.T, ts *testServer, key ssh.Signer, handle string) (*tclient, string) {
	t.Helper()
	conn, codec := ts.dial(t)
	exp, err := conn.Exporter()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := identity.Sign(key, exp)
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Write(proto.THello, proto.Hello{Client: "test", Caps: brAgentCaps, Agent: handle,
		Auth: proto.Auth{Pubkey: key.PublicKey().Marshal(), Sig: sig}}); err != nil {
		t.Fatal(err)
	}
	return brFinishHandshake(t, conn, codec, key)
}

func brFinishHandshake(t *testing.T, conn transport.Conn, codec *proto.Codec, key ssh.Signer) (*tclient, string) {
	f, err := codec.Read()
	if err != nil {
		return nil, "read: " + err.Error()
	}
	if f.Type == proto.TBye {
		var b proto.Bye
		f.Decode(&b)
		return nil, b.Code
	}
	var w proto.Welcome
	if f.Type != proto.TWelcome || f.Decode(&w) != nil {
		t.Fatalf("expected WELCOME, got %#x", f.Type)
	}
	codec.SetMaxFrame(proto.MaxFrame)
	c := &tclient{t: t, conn: conn, codec: codec, frames: make(chan proto.Frame, 1024), signer: key, user: w.User, welcome: w}
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

func brNonce() []byte {
	n := make([]byte, 16)
	rand.Read(n)
	return n
}

// §15.2: an agent handle is "unique across user names, group names and
// every handle ever used". Renaming an agent frees its old handle, so
// another user can register an agent (or join) under it and inherit the
// name that old messages (Message.agent = "rocky") still carry.
func TestAgents_BreakRenameFreesOldHandle(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, brAgentCaps)
	bob := ts.registerCaps(t, "bob", false, brAgentCaps)
	alice.syncFresh()
	bob.syncFresh()

	alice.ok(proto.OpAgentRegister, proto.AgentRegisterArgs{Handle: "rocky"}, nil)
	alice.ok(proto.OpAgentUpdate, proto.AgentUpdateArgs{Handle: "rex"}, nil)

	r := bob.req(proto.OpAgentRegister, proto.AgentRegisterArgs{Handle: "rocky"})
	if r.OK {
		t.Fatal("bob registered agent @rocky: a handle alice's agent used before its rename was free again")
	}
	if r.Err == nil || r.Err.Code != proto.ErrConflict {
		t.Fatalf("bob agent.register rocky: %v, want conflict", r.Err)
	}
}

// Same hole through the invite path: a new user may take a renamed agent's
// former handle as their name.
func TestAgents_BreakRenameFreesOldHandleForUsers(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, brAgentCaps)
	alice.syncFresh()
	alice.ok(proto.OpAgentRegister, proto.AgentRegisterArgs{Handle: "rocky"}, nil)
	alice.ok(proto.OpAgentUpdate, proto.AgentUpdateArgs{Handle: "rex"}, nil)

	code, err := ts.CreateInvite(context.Background(), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := proto.ParseInvite(code)
	if err != nil {
		t.Fatal(err)
	}
	if c, bye := ts.connectCaps(t, newKey(t), inv.Token, "rocky", nil); bye == "" {
		t.Fatalf("a new user joined as %q, the former handle of alice's agent", c.user.Name)
	}
}

// The agentRefused comment says an agent "acts in its owner's name only by
// sending, reacting and pinning". But the server lets an agent session edit
// and delete its owner's own (human) messages in any channel it may read:
// they share an author id. The edited text then shows as the owner's
// (Message.agent stays ""), so the agent can put words in its owner's mouth.
func TestAgents_BreakAgentEditsOwnersHumanMessage(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, brAgentCaps)
	snap, _ := alice.syncFresh()
	gen := generalID(t, snap)
	alice.ok(proto.OpAgentRegister, proto.AgentRegisterArgs{Handle: "rocky"}, nil)
	alice.ok(proto.OpAgentAccess, proto.AgentAccessArgs{Add: []uint64{gen}}, nil)
	human := alice.send(gen, "I approve the budget")

	agent, bye := brAgentHandshake(t, ts, alice.signer, "rocky")
	if bye != "" {
		t.Fatalf("agent login: BYE %s", bye)
	}
	agent.syncFresh()

	r := agent.req(proto.OpEdit, proto.EditArgs{Msg: human.Msg, Body: "I reject the budget"})
	if r.OK {
		t.Fatal("the agent edited its owner's human message (it now reads as the owner's words)")
	}
	r = agent.req(proto.OpDelete, proto.DeleteArgs{Msg: human.Msg})
	if r.OK {
		t.Fatal("the agent deleted its owner's human message")
	}
}

// TOCTOU: agentLogin checks the agent is active outside s.mu, register adds
// the session later; opAgentRetire revokes only the agent sessions already
// registered. A login that passed the check before the retire commits and
// registers after it survives as a live session of a retired agent.
// Stress: many concurrent agent logins racing one retire, repeated.
func TestAgents_BreakRetireRacesAgentLogin(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, brAgentCaps)
	alice.syncFresh()
	const rounds, logins = 28, 40
	for i := 0; i < rounds; i++ {
		h := fmt.Sprintf("r%d", i)
		alice.ok(proto.OpAgentRegister, proto.AgentRegisterArgs{Handle: h}, nil)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for j := 0; j < logins; j++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				<-start
				time.Sleep(time.Duration(j) * 100 * time.Microsecond)
				brRawAgentLogin(ts, alice.signer, h)
			}(j)
		}
		close(start)
		time.Sleep(time.Duration(i%8) * 300 * time.Microsecond)
		alice.ok(proto.OpAgentRetire, proto.Empty{}, nil)
		wg.Wait()
		time.Sleep(300 * time.Millisecond)
		ts.mu.Lock()
		var live []string
		for sess := range ts.sessions {
			if sess.agent == nil {
				continue
			}
			sess.mu.Lock()
			closed := sess.closed
			sess.mu.Unlock()
			if !closed {
				live = append(live, sess.agent.Handle)
			}
		}
		ts.mu.Unlock()
		if len(live) > 0 {
			t.Fatalf("round %d: agent sessions %v still live after their agent was retired", i, live)
		}
	}
}

// Rolling the server back to the previous release must keep the database
// openable. The old schema creates channels_name ON channels(name) WHERE
// kind != 'dm' (IF NOT EXISTS, and migrateAgents dropped it); every agent
// DM has name "", so two owners' agent DMs make that index unbuildable and
// the old binary fails to start on the new DB.
func TestAgents_BreakAgentDMsBlockRollback(t *testing.T) {
	dir := t.TempDir()
	ts := startServerCfg(t, "tcp", Config{DataDir: dir})
	for _, n := range []string{"alice", "bob"} {
		c := ts.registerCaps(t, n, n == "alice", brAgentCaps)
		c.syncFresh()
		c.ok(proto.OpAgentRegister, proto.AgentRegisterArgs{Handle: n + "-ai"}, nil)
		c.ok(proto.OpAgentDM, proto.Empty{}, nil)
	}
	ts.stop()
	ts.Close()
	st, err := openStore(filepath.Join(dir, "tc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	if _, err := st.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS channels_name ON channels(name) WHERE kind != 'dm'`); err != nil {
		t.Fatalf("the previous release's schema fails on this DB: %v", err)
	}
}

// brRawAgentLogin is a goroutine-safe agent login (no t.Fatal); the
// connection stays open until the test ends.
func brRawAgentLogin(ts *testServer, key ssh.Signer, handle string) {
	conn, err := transport.DialTCP(context.Background(), ts.addr, ts.Pin())
	if err != nil {
		return
	}
	go func() { time.Sleep(3 * time.Second); conn.Close() }()
	codec := proto.NewCodec(conn)
	exp, err := conn.Exporter()
	if err != nil {
		return
	}
	sig, err := identity.Sign(key, exp)
	if err != nil {
		return
	}
	codec.Write(proto.THello, proto.Hello{Client: "test", Caps: brAgentCaps, Agent: handle,
		Auth: proto.Auth{Pubkey: key.PublicKey().Marshal(), Sig: sig}})
	codec.SetMaxFrame(proto.MaxFrame)
	go func() {
		for {
			if _, err := codec.Read(); err != nil {
				return
			}
		}
	}()
}
