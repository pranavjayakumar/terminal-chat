// Package agent is the personal agent's supervisor (`tc agent run`). It is
// logged in as the agent device, listens to the chat, and when someone
// asks the agent something it runs one engine turn for that thread
// (internal/agent/runner), with tc's tools served through `tc mcp --agent`
// over a unix socket the supervisor answers. The supervisor, not the
// engine, enforces who may read what, the budget, and the owner's
// approvals: the engine only ever sees what the supervisor hands it.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/agent/memory"
	"github.com/skailama/tc/internal/agent/runner"
	"github.com/skailama/tc/internal/agent/work"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/mcp"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Chat is the part of the agent device's *client.Client the supervisor
// uses.
type Chat interface {
	mcp.Chat
	Updates() <-chan client.Update
	Status() client.Status
	AgentHandle() string
	OpenAgentDM(ctx context.Context) (proto.Channel, error)
	Send(ch uint64, body string, thread uint64) error
	SendAgentStatus(ch, thread uint64, state, status, preview string) error
	UpdateAgent(ctx context.Context, handle, icon string) (proto.Agent, error)
	Agents() []proto.Agent
}

// Memory is the agent's memory store (memory.Store).
type Memory interface {
	Remember(f memory.Fact) error
	Recall(query string, includePrivate bool) ([]memory.Hit, error)
	WriteHandoff(key, text string) error
	Commit(msg string) error
	Read(rel string) (memory.Note, error)
	Write(n memory.Note) error
	Forget(rel string) error
	PrepareDream() (*memory.Dream, error)
	ApplyDream(d *memory.Dream) error
	DiscardDream(d *memory.Dream) error
}

// Work is the agent's tc repo (work.Work).
type Work interface {
	EnsureClone(ctx context.Context) error
	Worktree(ctx context.Context, slug string) (work.Worktree, error)
	OpenDraftPR(ctx context.Context, dir, branch, title, body string) (string, error)
	OpenIssue(ctx context.Context, title, body string) (string, error)
}

// Supervisor runs one agent.
type Supervisor struct {
	Dir    string // agent home
	Chat   Chat
	Runner runner.Runner
	Memory Memory
	Work   Work
	// TCBin is the tc binary the engine starts as its MCP server.
	TCBin  string
	Server string // the tc server's address, for the identity block
	Model  string // engine model; "" = the runner's default (sonnet)
	// Refresh rewrites the tc-owned files of the agent home (home.Ensure);
	// called before a run when the tc binary changed since the last one,
	// so a running supervisor picks up a tc update's code and skills.
	Refresh func() error
	Version string
	Log     *slog.Logger
	Now     func() time.Time

	// Tunables; zero values take the defaults.
	MaxRuns         int           // concurrent runs (2)
	ApprovalTimeout time.Duration // how long an approval card waits (30 min)
	StaleAfter      time.Duration // older asks get an apology instead (24 h)
	StatusEvery     time.Duration // live status throttle (250 ms: ≤ 4/s)
	DreamAfter      time.Duration // idle time before the daily dream (30 min)

	paths    home.Paths
	sock     string
	sessions *sessions
	ledger   *Ledger
	queue    *queue
	sem      chan struct{}

	mu       sync.Mutex
	owner    uint64
	runs     map[string]*run        // bridge token → run
	threads  map[string][]Ask       // session key → asks waiting their turn
	busy     map[string]bool        // session key → a run is going
	pending  map[string]*approval   // card code → approval
	branches map[string]string      // work branch → worktree dir
	engaged  map[string]*engagement // session key → listening without a mention
	left     map[string]bool        // session key → the run called leave_thread
	watches  map[string]*Watch      // watch key → the owner's watch
	status   Status
	lastRun  time.Time
	online   bool
	started  bool
	wg       sync.WaitGroup
	actMu    sync.Mutex
	dreaming bool
	exeStamp string // TCBin's size and mtime when the home was last refreshed
}

func (s *Supervisor) defaults() {
	if s.MaxRuns <= 0 {
		s.MaxRuns = 2
	}
	if s.ApprovalTimeout <= 0 {
		s.ApprovalTimeout = 30 * time.Minute
	}
	if s.StaleAfter <= 0 {
		s.StaleAfter = 24 * time.Hour
	}
	if s.StatusEvery <= 0 {
		s.StatusEvery = time.Second / proto.AgentEphRate
	}
	if s.DreamAfter <= 0 {
		s.DreamAfter = 30 * time.Minute
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Log == nil {
		s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.TCBin == "" {
		s.TCBin, _ = os.Executable()
	}
}

// Run supervises until ctx ends. The client must already be running.
func (s *Supervisor) Run(ctx context.Context) error {
	s.defaults()
	s.paths = home.PathsFor(s.Dir)
	if err := os.MkdirAll(s.paths.State, 0o700); err != nil {
		return err
	}
	var err error
	if s.sessions, err = openSessions(s.paths.State); err != nil {
		return err
	}
	if s.ledger, err = OpenLedger(s.paths.State); err != nil {
		return err
	}
	if s.queue, err = openQueue(s.paths.State, s.Now()); err != nil {
		return err
	}
	s.sem = make(chan struct{}, s.MaxRuns)
	s.runs, s.threads, s.busy = map[string]*run{}, map[string][]Ask{}, map[string]bool{}
	s.pending, s.branches = map[string]*approval{}, map[string]string{}
	s.engaged, s.left, s.watches = map[string]*engagement{}, map[string]bool{}, map[string]*Watch{}
	s.owner = s.Chat.Me().ID
	if s.handleNow() == "" {
		return errors.New("this tc home isn't an agent's (no agent handle in its config.json)")
	}

	// A short path: unix socket paths are limited to ~104 bytes.
	s.sock = filepath.Join(s.paths.State, SocketFile)
	_ = os.Remove(s.sock)
	ln, err := net.Listen("unix", s.sock)
	if err != nil {
		return fmt.Errorf("opening the tool socket: %w", err)
	}
	_ = os.Chmod(s.sock, 0o600)
	defer os.Remove(s.sock)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := mcp.ServeBridge(ctx, ln, s.handleTool); err != nil {
			s.Log.Error("tool socket", "err", err)
		}
	}()
	defer s.wg.Wait()

	now := s.Now().UnixMilli()
	s.mu.Lock()
	s.status = Status{PID: os.Getpid(), Version: s.Version, Started: now, State: "starting", Since: now}
	s.mu.Unlock()
	s.saveStatus()
	defer func() {
		s.setState("stopped")
	}()
	s.Log.Info("agent started", "handle", s.handleNow())

	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case u, ok := <-s.Chat.Updates():
			if !ok {
				return nil
			}
			s.handleUpdate(ctx, u)
		case <-tick.C:
			s.housekeeping(ctx)
		}
	}
}

// trigger is Trigger for this agent. Webhook bots never call it: their
// text comes from outside tc and nobody answers for its cost.
func (s *Supervisor) trigger(m client.Message, kind string) (Ask, bool) {
	if s.Chat.User(m.Author).Bot {
		return Ask{}, false
	}
	return Trigger(m, kind, s.owner, s.handleNow())
}

// handleNow is the agent's handle; it follows renames.
func (s *Supervisor) handleNow() string { return s.Chat.AgentHandle() }

func (s *Supervisor) handleUpdate(ctx context.Context, u client.Update) {
	st := s.Chat.Status()
	if st.Stopped {
		s.setErr("stopped", st.Err)
	}
	s.mu.Lock()
	wasOnline := s.online
	s.online = st.State == client.Online
	first := s.online && !s.started
	if first {
		s.started = true
	}
	s.mu.Unlock()
	if s.online != wasOnline {
		if s.online {
			s.setState(s.idleState())
		} else {
			s.setState("offline")
		}
	}
	for _, m := range u.New {
		if m.Agent != "" && m.Author == s.owner {
			s.noteOwnMessage(m)
			continue
		}
		cv, _ := s.Chat.Channel(m.Chan)
		if a, ok := s.trigger(m, cv.Kind); ok {
			s.accept(ctx, a)
		} else if a, ok := s.watchAsk(m, cv.Kind); ok {
			s.accept(ctx, a)
		} else if a, ok := s.followUp(m, cv.Kind); ok {
			s.accept(ctx, a)
		}
	}
	for ch := range u.Msgs {
		s.checkApprovals(ch)
	}
	if first {
		s.startup(ctx)
	}
}

// startup answers what came in while the agent was away, and greets the
// owner in a brand-new agent DM.
func (s *Supervisor) startup(ctx context.Context) {
	for _, a := range s.backlog() {
		s.accept(ctx, a)
	}
	if !s.queue.get().Greeted {
		s.greet(ctx)
	}
}

// backlog finds asks in the cache that arrived since the first start and
// were never handled (the replay in Updates covers most; this covers a
// cache that was already synced when the supervisor started).
func (s *Supervisor) backlog() []Ask {
	since := s.queue.since()
	var out []Ask
	for _, cv := range s.Chat.Channels() {
		if !cv.Member {
			continue
		}
		for _, m := range s.Chat.Messages(cv.ID, 50) {
			if m.Time < since {
				continue
			}
			msgs := []client.Message{m}
			if m.Replies > 0 {
				msgs = s.Chat.Thread(m.ID)
			}
			for _, x := range msgs {
				if x.Time < since {
					continue
				}
				if a, ok := s.trigger(x, cv.Kind); ok {
					out = append(out, a)
				}
			}
		}
	}
	return out
}

// accept takes an ask: stale ones get an apology, the rest wait their turn
// in their thread.
func (s *Supervisor) accept(ctx context.Context, a Ask) {
	now := s.Now()
	if a.Msg.Time < s.queue.since() || !s.queue.claim(a.Msg.ID, now) {
		return
	}
	if s.stopAuto(a) {
		return
	}
	if a.FollowUp && now.Sub(time.UnixMilli(a.Msg.Time)) > s.StaleAfter {
		return
	}
	if now.Sub(time.UnixMilli(a.Msg.Time)) > s.StaleAfter {
		s.reply(a.Chan, a.Thread, "sorry, I was asleep 💤 ask me again?")
		s.activity(a, "stale", "asked while asleep", 0)
		return
	}
	s.mu.Lock()
	s.threads[a.Key] = append(s.threads[a.Key], a)
	start := !s.busy[a.Key]
	if start {
		s.busy[a.Key] = true
	}
	s.mu.Unlock()
	if start {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.drain(ctx, a.Key)
		}()
	}
}

// drain runs a thread's asks one at a time.
func (s *Supervisor) drain(ctx context.Context, key string) {
	for {
		s.mu.Lock()
		q := s.threads[key]
		if len(q) == 0 || ctx.Err() != nil {
			delete(s.threads, key)
			s.busy[key] = false
			s.mu.Unlock()
			return
		}
		a := q[0]
		s.threads[key] = q[1:]
		s.mu.Unlock()

		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			continue
		}
		s.answer(ctx, a)
		<-s.sem
	}
}

func (s *Supervisor) housekeeping(ctx context.Context) {
	s.setState(s.idleState())
	s.pruneEngaged()
	s.expireWatches()
	s.maybeDream(ctx)
}

// idleState is awake, working, idle (nothing for IdleMinutes: no engine
// warm, sessions are only ids on disk) or paused.
func (s *Supervisor) idleState() string {
	cfg, err := home.LoadConfig(s.Dir)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.online:
		return "offline"
	case err == nil && cfg.Paused:
		return "paused"
	case len(s.runs) > 0:
		return "working"
	case err == nil && !s.lastRun.IsZero() && s.Now().Sub(s.lastRun) > time.Duration(cfg.IdleMinutes)*time.Minute:
		return "idle"
	case s.lastRun.IsZero() && s.Now().Sub(time.UnixMilli(s.status.Started)) > time.Duration(cfg.IdleMinutes)*time.Minute:
		return "idle"
	}
	return "awake"
}

func (s *Supervisor) setState(state string) {
	s.mu.Lock()
	if s.status.State != state {
		s.status.State, s.status.Since = state, s.Now().UnixMilli()
	}
	s.mu.Unlock()
	s.saveStatus()
}

func (s *Supervisor) setErr(state, msg string) {
	s.mu.Lock()
	s.status.Err = sanitize.Line(msg)
	s.mu.Unlock()
	s.setState(state)
}

func (s *Supervisor) saveStatus() {
	cfg, _ := home.LoadConfig(s.Dir)
	s.mu.Lock()
	s.status.Runs = len(s.runs)
	s.status.CapUSD = cfg.DailyCapUSD
	if !s.lastRun.IsZero() {
		s.status.LastRun = s.lastRun.UnixMilli()
	}
	st := s.status
	s.mu.Unlock()
	st.Watches = s.Watches()
	if s.queue != nil {
		st.Grants = s.Grants()
	}
	if s.ledger != nil {
		st.SpentUSD = s.ledger.Spent(s.Now())
	}
	if err := writeJSON(filepath.Join(s.paths.State, StatusFile), st); err != nil {
		s.Log.Warn("writing status", "err", err)
	}
}

func (s *Supervisor) activity(a Ask, what, detail string, usd float64) {
	where := ""
	if cv, ok := s.Chat.Channel(a.Chan); ok {
		where = mcp.ChanLabel(cv)
	}
	s.logActivity(Activity{T: s.Now().UnixMilli(), Who: s.askerName(a), Where: where, Chan: a.Chan, Thread: a.Thread, What: what, Detail: sanitize.Line(detail), USD: usd})
}

func (s *Supervisor) logActivity(act Activity) {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	if err := appendLine(filepath.Join(s.paths.State, ActivityFile), act); err != nil {
		s.Log.Warn("writing activity", "err", err)
	}
}

func (s *Supervisor) askerName(a Ask) string {
	n := sanitize.Line(s.Chat.User(a.Asker).Name)
	if n == "" {
		n = fmt.Sprintf("user%d", a.Asker)
	}
	if a.Via != "" {
		n += " (via " + sanitize.Line(a.Via) + ")"
	}
	return n
}
