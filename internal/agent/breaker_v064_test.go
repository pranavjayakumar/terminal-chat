package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/agent/runner"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// An ask answered days ago drops out of queue.json's handled list after
// 72 h. A restart's backlog scan must not take it for a missed ask and
// apologise for "sleeping" through something it answered.
func TestBacklogOldAnsweredAsk_Break(t *testing.T) {
	now := time.Now()
	old := client.Message{Message: proto.Message{ID: 500, Chan: general, Author: carolID, Body: "@rocky what's the build status?",
		Time: now.Add(-4 * 24 * time.Hour).UnixMilli()}}
	h := newHarness(t, cfgDefault(), func(s *Supervisor) {
		st := home.PathsFor(s.Dir).State
		q, err := openQueue(st, now.Add(-10*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		q.update(func(q *queueState) {
			q.Since = now.Add(-10 * 24 * time.Hour).UnixMilli()
			// The DM's "hey" (48 h ago) and the old ask were both handled;
			// the old one's entry was since pruned by a later claim.
			q.Handled = map[uint64]int64{1: now.Add(-48 * time.Hour).UnixMilli()}
		})
	})
	h.chat.mu.Lock()
	h.chat.msgs = append(h.chat.msgs, old)
	h.chat.mu.Unlock()
	h.stop()
	h2 := restartHarness(t, h, nil)
	time.Sleep(300 * time.Millisecond)
	for _, p := range h2.chat.posts() {
		if strings.Contains(p.body, "asleep") {
			t.Fatalf("apologised on restart for an ask from 4 days ago: %+v", p)
		}
	}
}

// Someone past their asks for the day who keeps @mentioning the agent
// gets one "that's your asks" per conversation per window, not one per
// message: otherwise anyone can make the owner's agent post as fast as
// they can type (the stale apology has the same rule).
func TestRefusalFlood_Break(t *testing.T) {
	cfg := cfgDefault()
	cfg.AsksPerPerson = 1
	h := newHarness(t, cfg, nil)
	h.chat.say(bobID, general, 0, "@rocky first", 0)
	h.waitFor("answer", func() bool { return len(h.chat.posts()) == 1 })
	for i := 0; i < 10; i++ {
		h.chat.say(bobID, general, 0, "@rocky again", 0)
	}
	time.Sleep(500 * time.Millisecond)
	refusals := 0
	for _, p := range h.chat.posts() {
		if strings.Contains(p.body, "asks for today") {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("%d refusals for 10 asks past the cap in one place; posts %+v", refusals, h.chat.posts())
	}
}

// Two guest runs at once (MaxRuns 2) must share what is left of the guest
// pool, not each get all of it: together they could spend the owner's
// headroom, which GuestCap promises they never can. The same goes for the
// daily cap.
func TestConcurrentRunsShareCap_Break(t *testing.T) {
	cfg := cfgDefault()
	cfg.DailyCapUSD, cfg.RunCapUSD, cfg.GuestCapUSD, cfg.AsksPerPerson = 2, 1, 1, 100
	h := newHarness(t, cfg, nil)
	if err := h.s.ledger.Add(Spend{T: time.Now().UnixMilli(), Asker: bobID, USD: 0.4, Kind: "ask", Guest: true}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-release
		return runner.Result{Session: req.Session, Text: "ok"}, nil
	}
	h.chat.say(bobID, general, 0, "@rocky one", 0)
	h.chat.say(carolID, secret, 0, "@rocky two", 0)
	h.waitFor("two runs", func() bool { return len(h.run.requests()) == 2 })
	close(release)
	total := 0.0
	for _, r := range h.run.requests() {
		total += r.MaxUSD
	}
	if total > 0.6+0.011 {
		t.Fatalf("two guest runs may spend $%.2f together; only $0.60 of the guest pool is left", total)
	}

	// The owner's runs: the daily cap is shared the same way.
	h.waitFor("guest answers", func() bool { return len(h.chat.posts()) == 2 })
	h.run.mu.Lock()
	h.run.reqs = nil
	h.run.mu.Unlock()
	release2 := make(chan struct{})
	h.run.mu.Lock()
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-release2
		return runner.Result{Session: req.Session, Text: "ok"}, nil
	}
	h.run.mu.Unlock()
	h.chat.say(ownerID, general, 0, "@rocky three", 0)
	h.chat.say(ownerID, secret, 0, "@rocky four", 0)
	h.waitFor("two owner runs", func() bool { return len(h.run.requests()) == 2 })
	close(release2)
	total = 0
	for _, r := range h.run.requests() {
		total += r.MaxUSD
	}
	if left := 2 - h.s.ledger.Spent(time.Now()); total > left+0.011 {
		t.Fatalf("two owner runs may spend $%.2f together; $%.2f of the daily cap is left", total, left)
	}
}
