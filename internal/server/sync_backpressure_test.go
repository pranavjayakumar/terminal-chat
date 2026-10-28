package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// bulkMessages writes n messages of size bytes into ch as author, straight
// into the log (no rate limit), the way send does.
func bulkMessages(t *testing.T, s *Server, ch, author uint64, n, size int) {
	t.Helper()
	ctx := context.Background()
	body := strings.Repeat("x", size)
	for done := 0; done < n; {
		batch := min(n-done, 100)
		_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
			for range batch {
				now := s.now()
				m := proto.Message{Chan: ch, Author: author, Body: body, Time: now}
				ev, err := appendEvent(ctx, tx, proto.KMsgNew, ch, author, 0, now, m)
				if err != nil {
					return nil, err
				}
				m.ID = ev.Seq
				if err := patchEventData(ctx, tx, &ev, m); err != nil {
					return nil, err
				}
				nonce := make([]byte, 16)
				rand.Read(nonce)
				if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, chan, author, body, thread, time, nonce) VALUES (?, ?, ?, ?, 0, ?, ?)`,
					m.ID, ch, author, body, now, nonce); err != nil {
					return nil, err
				}
			}
			return nil, nil // not published: nobody is live in these tests
		})
		if err != nil {
			t.Fatal(err)
		}
		done += batch
	}
}

// otherSession waits for a session of user u that isn't in before.
func otherSession(t *testing.T, s *Server, u uint64, before map[*session]bool) *session {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		s.mu.Lock()
		for sess := range s.sessions {
			if sess.user.ID == u && !before[sess] {
				s.mu.Unlock()
				return sess
			}
		}
		s.mu.Unlock()
	}
	t.Fatal("no new session")
	return nil
}

func sessionsNow(s *Server) map[*session]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[*session]bool{}
	for sess := range s.sessions {
		m[sess] = true
	}
	return m
}

// maxQueuedFor samples sess's queue for d and returns the largest it saw.
func maxQueuedFor(sess *session, d time.Duration) int {
	most := 0
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		sess.mu.Lock()
		most = max(most, sess.queued)
		sess.mu.Unlock()
	}
	return most
}

// A member who syncs and never reads used to make the server queue the
// whole replay (or snapshot) in memory: ~640 MB per session at worst.
// Sync output must wait for the writer instead, and a client that does
// read must still get all of it.
func TestSyncIsBackpressured(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot bool
	}{{"replay", false}, {"snapshot", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ts := startServer(t, "tcp")
			alice := ts.register(t, "alice", true)
			snap, sr := alice.syncFresh()
			gen := generalID(t, snap)
			chans := []uint64{gen}
			if tc.snapshot {
				for i := range 3 {
					var r proto.ChanRes
					alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: fmt.Sprintf("big%d", i)}, &r)
					chans = append(chans, r.Chan.ID)
				}
			}
			// ~12 MiB to send: well past the queue and the socket buffers.
			per := 200 / len(chans)
			if tc.snapshot {
				per = proto.SnapshotPerChan
			}
			want := 0
			for _, ch := range chans {
				bulkMessages(t, ts.Server, ch, alice.user.ID, per, 60<<10)
				want += per
			}

			before := sessionsNow(ts.Server)
			c, bye := ts.handshake(t, alice.signer, "", "")
			if bye != "" {
				t.Fatal(bye)
			}
			sess := otherSession(t, ts.Server, alice.user.ID, before)
			args := proto.SyncArgs{Since: sr.Head, ServerID: alice.welcome.ServerID}
			if tc.snapshot {
				args = proto.SyncArgs{}
			}
			c.codec.Write(proto.TReq, proto.Req{ID: 1, Op: proto.OpSync, Args: proto.MustRaw(args)})
			if q := maxQueuedFor(sess, 1500*time.Millisecond); q > syncHighWater+proto.MaxFrame {
				t.Fatalf("a non-reading client's sync queued %d bytes (limit %d)", q, syncHighWater+proto.MaxFrame)
			}

			// Reading now gets everything, then the RES.
			got := 0
			for {
				f, err := c.codec.Read()
				if err != nil {
					t.Fatalf("after %d messages: %v", got, err)
				}
				switch f.Type {
				case proto.TEvent:
					var ev proto.Event
					f.Decode(&ev)
					if ev.Kind == proto.KMsgNew {
						got++
					}
				case proto.TSnapshot:
					var part proto.Snapshot
					f.Decode(&part)
					for _, m := range part.Msgs {
						if len(m.Body) == 60<<10 {
							got++
						}
					}
				case proto.TRes:
					var r proto.Res
					f.Decode(&r)
					if !r.OK {
						t.Fatal(r.Err)
					}
					if got != want {
						t.Fatalf("got %d messages, want %d", got, want)
					}
					return
				case proto.TBye:
					t.Fatal("BYE during a catch-up that was being read")
				}
			}
		})
	}
}

// A sync waiting on a client that never reads ends when the session is
// closed (what checkStuck does after syncStuck), so its goroutine and
// session go away.
func TestStuckSyncEndsWithSession(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	snap, sr := alice.syncFresh()
	bulkMessages(t, ts.Server, generalID(t, snap), alice.user.ID, 200, 60<<10)

	before := sessionsNow(ts.Server)
	c, bye := ts.handshake(t, alice.signer, "", "")
	if bye != "" {
		t.Fatal(bye)
	}
	sess := otherSession(t, ts.Server, alice.user.ID, before)
	c.codec.Write(proto.TReq, proto.Req{ID: 1, Op: proto.OpSync,
		Args: proto.MustRaw(proto.SyncArgs{Since: sr.Head, ServerID: alice.welcome.ServerID})})
	for deadline := time.Now().Add(5 * time.Second); !sess.syncing.Load(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("sync never started")
		}
	}
	time.Sleep(200 * time.Millisecond)
	sess.mu.Lock()
	sess.slowLocked()
	sess.mu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		ts.mu.Lock()
		_, still := ts.sessions[sess]
		ts.mu.Unlock()
		if !still {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("session of a stuck sync never went away")
		}
	}
}

// One account can't open sessions without bound.
func TestSessionsPerUserCapped(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	for i := 1; i < maxSessionsPerUser; i++ {
		if _, bye := ts.handshake(t, alice.signer, "", ""); bye != "" {
			t.Fatalf("session %d: BYE %s", i+1, bye)
		}
	}
	if _, bye := ts.handshake(t, alice.signer, "", ""); bye != proto.ByeTooManySessions {
		t.Fatalf("session %d: got %q, want BYE %s", maxSessionsPerUser+1, bye, proto.ByeTooManySessions)
	}
	// Others are unaffected.
	ts.register(t, "bob", false)
}

// Syncs waiting on clients that don't read hold no database connection:
// more of them than the pool has connections must not stall anyone else.
func TestStuckSyncsDontStarveDatabase(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	snap, sr := alice.syncFresh()
	gen := generalID(t, snap)
	bulkMessages(t, ts.Server, gen, alice.user.ID, 100, 60<<10)
	for range 12 { // the pool has 8
		before := sessionsNow(ts.Server)
		c, bye := ts.handshake(t, alice.signer, "", "")
		if bye != "" {
			t.Fatal(bye)
		}
		sess := otherSession(t, ts.Server, alice.user.ID, before)
		c.codec.Write(proto.TReq, proto.Req{ID: 1, Op: proto.OpSync,
			Args: proto.MustRaw(proto.SyncArgs{Since: sr.Head, ServerID: alice.welcome.ServerID})})
		for deadline := time.Now().Add(5 * time.Second); !sess.syncing.Load(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("sync never started")
			}
		}
	}
	time.Sleep(300 * time.Millisecond)
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	bob.send(gen, "still served")
}
