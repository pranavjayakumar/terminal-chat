package server

import (
	"io"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// TestSessionCap_BreakAgentLocksOutOwner: an agent logs in with its own
// key, on a machine its owner trusts less than their own (§15), yet its
// sessions counted against the owner's maxSessionsPerUser. An agent (buggy
// reconnect loop, or a compromised host) holding 32 sessions locked its
// owner out of every device: BYE too_many_sessions, and with no session
// the owner can't even retire it.
func TestSessionCap_BreakAgentLocksOutOwner(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, agentCaps)
	alice.syncFresh()
	alice.ok(proto.OpAgentRegister, proto.AgentRegisterArgs{Handle: "rocky"}, nil)
	key := newKey(t)
	alice.ok(proto.OpAgentKey, agentKeyArgs(ts, alice, key), nil)
	for i := 0; ; i++ {
		_, bye := ts.handshakeCaps(t, key, "", "", agentCaps)
		if bye != "" {
			break
		}
		if i > 2*maxSessionsPerUser {
			t.Fatal("agent sessions aren't capped at all")
		}
	}
	if _, bye := ts.handshake(t, alice.signer, "", ""); bye != "" {
		t.Fatalf("owner's own second device refused with BYE %s: its agent's sessions used up the account", bye)
	}
}

// TestSyncStuck_BreakSlowQUICLink: on QUIC the OS can't report a send
// backlog, so checkStuck judged a syncing session by the time alone: one
// write in progress for syncStuck (45 s) was a slow consumer, however
// steadily the bytes moved. A 512 KiB snapshot part takes that long below
// ~11.6 KB/s, and an interrupted reset snapshot starts over on reconnect,
// so a client on a slow link never finished syncing. A write that is still
// moving must be left alone, as on TCP.
//
// The 45 s are simulated: the checks run with now 46 s after the current
// write started, while a reader drains the stream slowly but steadily.
func TestSyncStuck_BreakSlowQUICLink(t *testing.T) {
	ts := startServer(t, "quic")
	alice := ts.register(t, "alice", true)
	snap, _ := alice.syncFresh()
	bulkMessages(t, ts.Server, generalID(t, snap), alice.user.ID, proto.SnapshotPerChan, 60<<10)

	before := sessionsNow(ts.Server)
	c, bye := ts.handshake(t, alice.signer, "", "")
	if bye != "" {
		t.Fatal(bye)
	}
	if c.conn.Kind() != "quic" {
		t.Fatalf("transport %s, want quic", c.conn.Kind())
	}
	sess := otherSession(t, ts.Server, alice.user.ID, before)
	c.codec.Write(proto.TReq, proto.Req{ID: 1, Op: proto.OpSync, Args: proto.MustRaw(proto.SyncArgs{})})

	// A slow, steady reader: 4 KiB every 5 ms (~800 KB/s).
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		buf := make([]byte, 4<<10)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := io.ReadFull(c.conn, buf); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	for deadline := time.Now().Add(3 * time.Second); sess.writing.Load() == 0 || !sess.syncing.Load(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no sync write in progress")
		}
	}
	// readLoop's ticker first runs 5 s after the session started; these
	// checks stand in for it well before then.
	for range 3 {
		time.Sleep(200 * time.Millisecond)
		w := sess.writing.Load()
		if w == 0 {
			continue
		}
		sess.checkStuck(time.Unix(0, w).Add(syncStuck+time.Second), syncStuck)
	}
	sess.mu.Lock()
	closed := sess.closed
	sess.mu.Unlock()
	if closed {
		t.Fatal("a syncing QUIC session whose writes were still moving was closed as a slow consumer")
	}
}
