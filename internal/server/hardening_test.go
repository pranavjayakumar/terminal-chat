package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// Events published between WELCOME and sync must not be delivered twice
// (once by the sync, once from the live buffer).
func TestNoDuplicateEventsAroundSync(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	snap, _ := alice.syncFresh()
	gen := generalID(t, snap)
	bob := ts.register(t, "bob", false) // registered, not yet synced
	sent := alice.send(gen, "while bob was syncing")

	bsnap, sr := bob.syncFresh()
	var inSnap bool
	for _, m := range bsnap.Msgs {
		inSnap = inSnap || m.ID == sent.Msg
	}
	if !inSnap || sr.Head < sent.Msg {
		t.Fatalf("message not in snapshot (head %d, msg %d)", sr.Head, sent.Msg)
	}
	alice.send(gen, "after sync")
	for {
		ev := bob.event("")
		if ev.Seq <= sr.Head {
			t.Fatalf("event seq %d (%s) delivered again after sync head %d", ev.Seq, ev.Kind, sr.Head)
		}
		if ev.Kind == proto.KMsgNew {
			return
		}
	}
}

func TestFailedSyncCanRetry(t *testing.T) {
	ts := startServer(t, "tcp")
	a := ts.register(t, "alice", true)
	a.nextID++
	bad, _ := proto.Marshal("not a map")
	a.codec.Write(proto.TReq, proto.Req{ID: a.nextID, Op: proto.OpSync, Args: bad})
	if r := a.res(a.nextID); r.OK {
		t.Fatal("bad sync args accepted")
	}
	a.syncFresh()
	a.ok(proto.OpUsersList, proto.Empty{}, nil)
}

func TestReactionCaps(t *testing.T) {
	ts := startServer(t, "tcp")
	var users []*tclient
	for i := range 4 {
		u := ts.register(t, fmt.Sprintf("u%d", i), i == 0)
		u.syncFresh()
		users = append(users, u)
	}
	gen := uint64(1)
	msg := users[0].send(gen, "react to me").Msg
	n := 0
	for _, u := range users {
		for range maxReactionsPerUser {
			if n < maxEmojiPerMsg {
				u.ok(proto.OpReact, proto.ReactArgs{Msg: msg, Emoji: fmt.Sprintf("e%d", n)}, nil)
			}
			n++
		}
	}
	users[0].fail(proto.OpReact, proto.ReactArgs{Msg: msg, Emoji: "e-new"}, proto.ErrBadRequest)
	// Joining an existing reaction is still fine...
	users[3].ok(proto.OpReact, proto.ReactArgs{Msg: msg, Emoji: "e0"}, nil)
	// ...but not past the per-user cap.
	users[1].fail(proto.OpReact, proto.ReactArgs{Msg: msg, Emoji: "e0"}, proto.ErrBadRequest)
}

func TestPunctuationSearch(t *testing.T) {
	ts := startServer(t, "tcp")
	a := ts.register(t, "alice", true)
	a.syncFresh()
	for _, q := range []string{"!!!", `"`, "*", "-", "a OR", "NEAR(", strings.Repeat("x ", 50)} {
		a.ok(proto.OpSearch, proto.SearchArgs{Q: q}, nil)
	}
	a.fail(proto.OpSearch, proto.SearchArgs{Q: strings.Repeat("x", maxSearchQuery+1)}, proto.ErrBadRequest)
}

// A connection that never finishes the handshake must not hold up shutdown.
func TestShutdownWithHalfOpenConn(t *testing.T) {
	ts := startServer(t, "tcp")
	ts.dial(t) // TLS up, never sends HELLO
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		ts.stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return with a half-open connection")
	}
}

// A client that requests without reading must be cut off, not buffered
// without limit.
func TestOwnQueueIsBounded(t *testing.T) {
	ts := startServer(t, "tcp")
	code, _ := ts.CreateInvite(context.Background(), true, 0)
	inv, _ := proto.ParseInvite(code)
	c, bye := ts.handshake(t, newKey(t), inv.Token, "alice")
	if bye != "" {
		t.Fatal(bye)
	}
	big := proto.Ping{T: 1}
	payload, _ := proto.Marshal(map[string]any{"t": 1, "pad": strings.Repeat("x", 900<<10)})
	_ = big
	// 40 × 900 KiB of PONGs is far past the 8 MiB cap plus socket buffers.
	writeErr := make(chan error, 1)
	go func() {
		for range 40 {
			if err := c.codec.WriteRaw(proto.TPing, payload); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}()
	select {
	case <-writeErr:
	case <-time.After(10 * time.Second):
	}
	pongs := 0
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f, err := c.codec.Read()
		if err != nil {
			if errors.Is(err, io.EOF) || pongs < 40 {
				return // cut off: good
			}
			t.Fatal(err)
		}
		switch f.Type {
		case proto.TBye:
			return
		case proto.TPong:
			pongs++
			if pongs == 40 {
				t.Fatal("server buffered all 40 large PONGs for a non-reading client")
			}
		}
	}
	t.Fatal("connection neither closed nor completed")
}

func TestDisabledUserCannotAct(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	bob := ts.register(t, "bob", false)
	alice.syncFresh()
	bob.syncFresh()
	alice.ok(proto.OpUserDisable, proto.UserArgs{User: bob.user.ID}, nil)
	nonce := make([]byte, 16)
	rand.Read(nonce)
	bob.nextID++
	bob.codec.Write(proto.TReq, proto.Req{ID: bob.nextID, Op: proto.OpSend,
		Args: proto.MustRaw(proto.SendArgs{Chan: 1, Body: "still here?", Nonce: nonce})})
	for f := range bob.frames {
		if f.Type == proto.TRes {
			var r proto.Res
			f.Decode(&r)
			if r.OK {
				t.Fatal("disabled user's send succeeded")
			}
		}
	}
}
