package client

import (
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

type startLog struct {
	mu sync.Mutex
	s  []HuddleStart
}

func (l *startLog) get() []HuddleStart {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]HuddleStart(nil), l.s...)
}

func collectStarts4(c *Client) *startLog {
	l := &startLog{}
	go func() {
		for u := range c.Updates() {
			l.mu.Lock()
			l.s = append(l.s, u.HuddleStarted...)
			l.mu.Unlock()
		}
	}()
	return l
}

func roster4(fc *fakeConn, ch uint64, users ...uint64) {
	var parts []proto.Part
	for i, u := range users {
		parts = append(parts, proto.Part{User: u, Src: uint32(100 + i)})
	}
	fc.write(proto.TEphemeral, proto.Ephemeral{Kind: proto.EphHuddle, Chan: ch, Parts: parts})
}

// "Reconnects never ring": rosters before the PONG to the client's
// post-sync PING are a baseline however late they come (the real server
// answers in order, so its sync rosters always precede that PONG). The
// first fix was a 2 s timer, which a slow link or a busy read loop beat.
func TestHuddleStart_BreakLateRosterAfterReconnectRings(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	starts := collectStarts4(c)
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))

	// Drop and resync: bob's DM huddle is already going. The roster dump
	// lands 2.5 s after the RES, but before the marker's PONG.
	fs.noPong.Store(true)
	fc.conn.Close()
	fc2 := fs.accept()
	var a proto.SyncArgs
	r := fc2.expectReq(proto.OpSync, &a)
	fc2.ok(r.ID, proto.SyncRes{Head: 20})
	waitFor(t, "online again", online(c))
	mark := fc2.next(proto.TPing)
	time.Sleep(baselineFor + 500*time.Millisecond)
	roster4(fc2, dm, bob)
	if err := fc2.codec.WriteRaw(proto.TPong, mark.Payload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "roster applied", func() bool { return len(c.Huddle(dm)) == 1 })
	time.Sleep(100 * time.Millisecond)
	if s := starts.get(); len(s) != 0 {
		t.Fatalf("a huddle already running at reconnect was reported as started: %+v", s)
	}
	// After the marker, an empty→one roster is a real start.
	roster4(fc2, general, bob)
	waitFor(t, "a live start", func() bool { return len(starts.get()) == 1 })
	if s := starts.get()[0]; s.Chan != general || s.By != bob {
		t.Fatalf("start %+v", s)
	}
}
