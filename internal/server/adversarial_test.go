package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// fastClock makes every Now() call advance 1s, so rate limits never bite
// when a test needs to create a lot of history quickly.
func fastClock(ts *testServer) {
	var t atomic.Int64
	t.Store(time.Now().UnixNano())
	ts.mu.Lock()
	ts.cfg.Now = func() time.Time { return time.Unix(0, t.Add(int64(time.Second))) }
	ts.mu.Unlock()
}

// One invite redeemed by many connections at once: exactly one wins.
func TestConcurrentInviteRedemption(t *testing.T) {
	ts := startServer(t, "tcp")
	code, _ := ts.CreateInvite(context.Background(), false, 0)
	inv, _ := proto.ParseInvite(code)
	const n = 12
	var wg sync.WaitGroup
	var ok atomic.Int32
	byes := make(chan string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, bye := ts.connect(t, newKey(t), inv.Token, fmt.Sprintf("racer%d", i))
			if c != nil {
				ok.Add(1)
			} else {
				byes <- bye
			}
		}()
	}
	wg.Wait()
	close(byes)
	if ok.Load() != 1 {
		t.Fatalf("%d redemptions succeeded, want 1", ok.Load())
	}
	for b := range byes {
		if b != proto.ByeInviteInvalid {
			t.Fatalf("loser got %q", b)
		}
	}
	if ts.UserCount() != 1 {
		t.Fatalf("users = %d", ts.UserCount())
	}
}

// The same nonce sent concurrently from two sessions of one user creates
// exactly one message.
func TestConcurrentSameNonce(t *testing.T) {
	ts := startServer(t, "tcp")
	a1 := ts.register(t, "alice", true)
	a2, _ := ts.connect(t, a1.signer, "", "")
	snap, _ := a1.syncFresh()
	a2.syncFresh()
	gen := generalID(t, snap)
	nonce := make([]byte, 16)
	rand.Read(nonce)
	args := proto.SendArgs{Chan: gen, Body: "once", Nonce: nonce}
	var r1, r2 proto.SendRes
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a1.ok(proto.OpSend, args, &r1) }()
	go func() { defer wg.Done(); a2.ok(proto.OpSend, args, &r2) }()
	wg.Wait()
	if r1.Msg != r2.Msg {
		t.Fatalf("two messages for one nonce: %d, %d", r1.Msg, r2.Msg)
	}
	var hist proto.HistoryRes
	a1.ok(proto.OpHistory, proto.HistoryArgs{Chan: gen}, &hist)
	if len(hist.Msgs) != 1 {
		t.Fatalf("history has %d messages", len(hist.Msgs))
	}
}

// Many connections that never start TLS must not stop a real user getting in.
func TestHalfOpenFloodDoesNotBlockLogin(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	alice.conn.Close()
	for range 60 {
		c, err := net.Dial("tcp", ts.addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
	}
	start := time.Now()
	if _, bye := ts.connect(t, alice.signer, "", ""); bye != "" {
		t.Fatalf("login during flood: %q", bye)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("login took %v during flood", d)
	}
}

// A replay of thousands of events while another user is sending: the
// syncing client sees strictly increasing seqs with nothing missing.
func TestSyncUnderConcurrentWrites(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.register(t, "alice", true)
	bob := ts.register(t, "bob", false)
	snap, _ := alice.syncFresh()
	_, bsr := bob.syncFresh()
	gen := generalID(t, snap)
	bob.conn.Close()

	// History bob will have to replay.
	for i := range 3000 {
		nonce := make([]byte, 16)
		rand.Read(nonce)
		if _, err := ts.opSend(context.Background(), alice.user, proto.SendArgs{Chan: gen, Body: fmt.Sprint("old", i), Nonce: nonce}); err != nil {
			t.Fatal(err)
		}
	}

	bob2, _ := ts.connect(t, bob.signer, "", "")
	stop := make(chan struct{})
	senderDone := make(chan struct{})
	var sent atomic.Int32
	go func() {
		defer close(senderDone)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			nonce := make([]byte, 16)
			rand.Read(nonce)
			ts.opSend(context.Background(), alice.user, proto.SendArgs{Chan: gen, Body: fmt.Sprint("live", i), Nonce: nonce})
			sent.Add(1)
		}
	}()
	bob2.nextID++
	bob2.codec.Write(proto.TReq, proto.Req{ID: bob2.nextID, Op: proto.OpSync,
		Args: proto.MustRaw(proto.SyncArgs{Since: bsr.Head, ServerID: ts.st.serverID})})

	var last uint64
	seen := map[uint64]bool{}
	var head uint64
	gotRes := false
	deadline := time.After(30 * time.Second)
	for !gotRes || len(seen) < 3000+int(sent.Load()) {
		if gotRes && len(seen) >= 3000 {
			select {
			case <-stop:
			default:
				close(stop)
			}
		}
		var f proto.Frame
		select {
		case f = <-bob2.frames:
		case <-deadline:
			t.Fatalf("timeout: %d events, res=%v, sent live %d", len(seen), gotRes, sent.Load())
		}
		switch f.Type {
		case proto.TEvent:
			var ev proto.Event
			f.Decode(&ev)
			if ev.Seq <= last {
				t.Fatalf("seq %d after %d (dup or reorder)", ev.Seq, last)
			}
			if gotRes && ev.Seq <= head {
				t.Fatalf("event %d at or below sync head %d arrived after RES", ev.Seq, head)
			}
			last = ev.Seq
			if ev.Kind == proto.KMsgNew {
				seen[ev.Seq] = true
			}
		case proto.TRes:
			var r proto.Res
			f.Decode(&r)
			var sr proto.SyncRes
			proto.Unmarshal(r.Data, &sr)
			head, gotRes = sr.Head, true
		}
	}
	// Stop the sender, then everything the server stored must reach bob.
	select {
	case <-stop:
	default:
		close(stop)
	}
	<-senderDone
	var n int
	settle := time.After(10 * time.Second)
	for {
		ts.st.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE chan = ?`, gen).Scan(&n)
		if len(seen) >= n {
			break
		}
		select {
		case f := <-bob2.frames:
			if f.Type == proto.TEvent {
				var ev proto.Event
				f.Decode(&ev)
				if ev.Seq <= last {
					t.Fatalf("seq %d after %d (dup or reorder)", ev.Seq, last)
				}
				last = ev.Seq
				if ev.Kind == proto.KMsgNew {
					seen[ev.Seq] = true
				}
			}
		case <-settle:
			rows, _ := ts.st.db.Query(`SELECT id FROM messages WHERE chan = ?`, gen)
			var missing []uint64
			for rows.Next() {
				var id uint64
				rows.Scan(&id)
				if !seen[id] {
					missing = append(missing, id)
				}
			}
			rows.Close()
			t.Fatalf("bob saw %d messages, server has %d; missing seqs %v (head %d)", len(seen), n, missing, head)
		}
	}
	if len(seen) != n {
		t.Fatalf("bob saw %d messages, server has %d", len(seen), n)
	}
}

// Connecting and disconnecting repeatedly must not leak goroutines.
func TestNoGoroutineLeak(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	alice.syncFresh()
	alice.conn.Close()
	time.Sleep(300 * time.Millisecond)
	base := runtime.NumGoroutine()
	for range 30 {
		c, _ := ts.connect(t, alice.signer, "", "")
		c.syncFresh()
		c.conn.Close()
	}
	var now int
	for range 50 {
		time.Sleep(100 * time.Millisecond)
		now = runtime.NumGoroutine()
		if now <= base+5 {
			return
		}
	}
	buf := make([]byte, 1<<16)
	n := runtime.Stack(buf, true)
	t.Fatalf("goroutines %d → %d after 30 sessions\n%s", base, now, buf[:n])
}
