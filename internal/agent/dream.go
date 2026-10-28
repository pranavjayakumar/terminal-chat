package agent

import (
	"context"
	"strings"
	"time"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/agent/runner"
)

// maybeDream runs the daily memory dream once the agent has been idle for
// DreamAfter: a separate engine run in a worktree of the memory repo, which
// the memory store checks and merges (or throws away).
func (s *Supervisor) maybeDream(ctx context.Context) {
	if s.Memory == nil || ctx.Err() != nil {
		return
	}
	now := s.Now()
	today := now.Format("2006-01-02")
	s.mu.Lock()
	idle := len(s.runs) == 0 && !s.lastRun.IsZero() && now.Sub(s.lastRun) >= s.DreamAfter
	ok := idle && s.online && !s.dreaming && s.queue.get().LastDream != today
	if ok {
		s.dreaming = true
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	cfg, err := home.LoadConfig(s.Dir)
	if err != nil || cfg.Paused || s.ledger.Spent(now) >= cfg.DailyCapUSD {
		s.mu.Lock()
		s.dreaming = false
		s.mu.Unlock()
		return
	}
	s.queue.update(func(q *queueState) { q.LastDream = today })
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			s.dreaming = false
			s.mu.Unlock()
		}()
		s.dream(ctx, cfg)
	}()
}

func (s *Supervisor) dream(ctx context.Context, cfg home.Config) {
	d, err := s.Memory.PrepareDream()
	if err != nil {
		s.Log.Warn("dream", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	res, err := s.Runner.Run(ctx, runner.Request{
		Dir:    d.Dir,
		Prompt: d.Prompt,
		Mode:   runner.Edit,
		// The dream can't touch the worktree's .git that tc's git reads next.
		Settings: home.PathsFor(s.Dir).DreamSettings,
		System:   s.identity(Ask{}),
		MaxUSD:   min(cfg.RunCapUSD, cfg.DailyCapUSD-s.ledger.Spent(s.Now())),
		Env:      []string{"TC_AGENT_DREAM=1"},
	}, nil)
	_ = s.ledger.Add(Spend{T: s.Now().UnixMilli(), USD: res.CostUSD, Kind: "dream"})
	act := Activity{T: s.Now().UnixMilli(), What: "dream", USD: res.CostUSD}
	if err != nil || res.IsError {
		_ = s.Memory.DiscardDream(d)
		act.Detail = "discarded: the run failed"
		if err != nil {
			act.Detail += ": " + short(strings.TrimSpace(err.Error()), 200)
		}
	} else if err := s.Memory.ApplyDream(d); err != nil {
		act.Detail = "rejected: " + short(err.Error(), 200)
	} else {
		act.Detail = "merged"
	}
	s.logActivity(act)
}
