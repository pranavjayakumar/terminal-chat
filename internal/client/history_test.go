package client

import (
	"context"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

// History paging past long threads (#81), against a scripted server so a
// thread of thousands of replies costs nothing to set up.

// testPages is the page cap here: small, so threads past it stay cheap.
const testPages = 4

const (
	longChan = 200
	longTop  = 1000 // the top-level message before the thread
	longRoot = 1001 // the thread root; replies follow it
)

// longHistory is channel longChan: longTop, longRoot, then replies
// replies to longRoot, served 100 at a time like the real server.
type longHistory struct {
	replies int
	fc      *fakeConn
	befores chan uint64 // each history REQ's before, in order
	fail    func(page int) bool
}

func (h *longHistory) msg(id uint64) proto.Message {
	m := proto.Message{ID: id, Chan: longChan, Author: bob, Body: "reply"}
	switch id {
	case longTop:
		m.Body = "top 1"
	case longRoot:
		m.Body = "root"
	default:
		m.Thread = longRoot
	}
	return m
}

// serve answers history REQs until the connection closes.
func (h *longHistory) serve() {
	last := uint64(longRoot + h.replies)
	for page := 0; ; page++ {
		var r proto.Req
		var a proto.HistoryArgs
		for {
			f, ok := <-h.fc.in
			if !ok {
				return
			}
			if f.Type != proto.TReq || f.Decode(&r) != nil || r.Op != proto.OpHistory {
				continue
			}
			if proto.Unmarshal(r.Args, &a) == nil {
				break
			}
		}
		h.befores <- a.Before
		if h.fail != nil && h.fail(page) {
			h.fc.fail(r.ID, proto.ErrInternal)
			continue
		}
		before := a.Before
		if before == 0 || before > last+1 {
			before = last + 1
		}
		var msgs []proto.Message
		lo := max(uint64(longTop), before-min(before, 100))
		for id := lo; id < before; id++ {
			if id >= longTop {
				msgs = append(msgs, h.msg(id))
			}
		}
		h.fc.ok(r.ID, proto.HistoryRes{Msgs: msgs, More: len(msgs) > 0 && msgs[0].ID > longTop})
	}
}

func startLong(t *testing.T, replies int) (*Client, *longHistory) {
	t.Helper()
	testEnv(t)
	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	c.timing.historyPages = testPages
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))
	fc.event(21, proto.KChanNew, longChan, bob, proto.Channel{ID: longChan, Name: "long", Kind: proto.ChanPublic, Members: []uint64{alice, bob}})
	waitFor(t, "channel", func() bool { return lastSeq(t, c) == 21 })
	h := &longHistory{replies: replies, fc: fc, befores: make(chan uint64, 1000)}
	go h.serve()
	return c, h
}

// oldestTop is what the TUI pages by: the oldest top-level message it shows.
func oldestTop(c *Client) uint64 {
	for _, m := range c.Messages(longChan, 1000) {
		if m.ID != 0 {
			return m.ID
		}
	}
	return 0
}

func hasTop(c *Client) bool {
	for _, m := range c.Messages(longChan, 1000) {
		if m.ID == longTop {
			return true
		}
	}
	return false
}

func drainBefores(ch chan uint64) []uint64 {
	var out []uint64
	for {
		select {
		case b := <-ch:
			out = append(out, b)
		default:
			return out
		}
	}
}

func TestHistoryPagesPastReplies(t *testing.T) {
	c, _ := startLong(t, 250)
	more, err := c.History(context.Background(), longChan, oldestTop(c))
	if err != nil {
		t.Fatal(err)
	}
	if !hasTop(c) || more {
		t.Fatalf("after one call: has top-level message = %v, more = %v", hasTop(c), more)
	}
}

// A thread longer than the page cap: the next call with the same cursor
// resumes where the first stopped instead of walking the same pages.
func TestHistoryResumesAfterPageCap(t *testing.T) {
	c, h := startLong(t, (testPages+3)*100)
	ctx := context.Background()
	more, err := c.History(ctx, longChan, oldestTop(c))
	if err != nil || !more || hasTop(c) {
		t.Fatalf("first call: more=%v err=%v top=%v; want it stopped at the page cap", more, err, hasTop(c))
	}
	if n := len(drainBefores(h.befores)); n != testPages {
		t.Fatalf("first call fetched %d pages, want %d", n, testPages)
	}
	if _, err := c.History(ctx, longChan, oldestTop(c)); err != nil {
		t.Fatal(err)
	}
	if !hasTop(c) {
		t.Fatal("second call walked the same pages again")
	}
	if n := len(drainBefores(h.befores)); n > 5 {
		t.Fatalf("second call fetched %d pages, want only the rest (~4)", n)
	}
}

// A call that fails partway (a timeout on a slow link, a dropped
// connection) keeps the pages it walked.
func TestHistoryFailureKeepsProgress(t *testing.T) {
	c, h := startLong(t, (testPages+1)*100)
	h.fail = func(page int) bool { return page == 2 }
	ctx := context.Background()
	if _, err := c.History(ctx, longChan, oldestTop(c)); err == nil {
		t.Fatal("first call should have failed")
	}
	got := drainBefores(h.befores)
	stopped := got[len(got)-1] // the failed page's before: where the walk had got to
	if _, err := c.History(ctx, longChan, oldestTop(c)); err != nil {
		t.Fatal(err)
	}
	if !hasTop(c) {
		t.Fatal("the failed call's pages were walked again and the cap hit")
	}
	if b := drainBefores(h.befores); len(b) == 0 || b[0] != stopped {
		t.Fatalf("second call started at %v, want before=%d", b, stopped)
	}
}

// Leaving drops the cached messages, so the resume point must go too: once
// the stop point is cached again (the thread opened after a rejoin), the
// messages above it would otherwise be skipped.
func TestHistoryResumeDroppedOnLeave(t *testing.T) {
	c, h := startLong(t, (testPages+3)*100)
	ctx := context.Background()
	if _, err := c.History(ctx, longChan, 0); err != nil {
		t.Fatal(err)
	}
	drainBefores(h.befores)
	var stop uint64
	if err := c.db.QueryRow(`SELECT MIN(id) FROM msgs WHERE chan = ?`, longChan).Scan(&stop); err != nil {
		t.Fatal(err)
	}
	h.fc.event(22, proto.KChanLeave, longChan, alice, proto.UserRef{User: alice})
	waitFor(t, "left", func() bool { return lastSeq(t, c) == 22 })
	h.fc.event(23, proto.KChanJoin, longChan, alice, proto.ChanJoin{User: alice})
	waitFor(t, "rejoined", func() bool { return lastSeq(t, c) == 23 })
	// The stop point comes back into the cache, e.g. by opening the thread
	// (ThreadHistory); the messages above it don't.
	if err := c.saveMsgs([]proto.Message{h.msg(stop)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.History(ctx, longChan, 0); err != nil {
		t.Fatal(err)
	}
	if b := drainBefores(h.befores); len(b) == 0 || b[0] != 0 {
		t.Fatalf("after leave and rejoin History started at %v, want before=0", b)
	}
}
