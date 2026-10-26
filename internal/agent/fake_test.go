package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/agent/memory"
	"github.com/skailama/tc/internal/agent/runner"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/mcp"
	"github.com/skailama/tc/internal/proto"
)

const (
	ownerID = 1
	bobID   = 2
	carolID = 3
	general = 10
	secret  = 11
	agentDM = 12
	bobDM   = 13
)

type posted struct {
	ch, thread uint64
	body       string
}

type liveMsg struct {
	ch, thread             uint64
	state, status, preview string
	at                     time.Time
}

// fakeChat is the agent device's client, in memory.
type fakeChat struct {
	mu      sync.Mutex
	updates chan client.Update
	chans   []client.ChannelView
	users   map[uint64]proto.User
	msgs    []client.Message
	sent    []posted
	live    []liveMsg
	nextID  uint64
	echo    bool // posted messages come back as our own msg.new
	handle  string
}

func newFakeChat() *fakeChat {
	return &fakeChat{
		updates: make(chan client.Update, 64),
		users: map[uint64]proto.User{
			ownerID: {ID: ownerID, Name: "fizan"}, bobID: {ID: bobID, Name: "bob"}, carolID: {ID: carolID, Name: "carol"},
		},
		chans: []client.ChannelView{
			{Channel: proto.Channel{ID: general, Name: "general", Kind: proto.ChanPublic}, Member: true},
			{Channel: proto.Channel{ID: secret, Name: "secret", Kind: proto.ChanPrivate}, Member: true},
			{Channel: proto.Channel{ID: agentDM, Kind: proto.ChanAgentDM}, Member: true},
			{Channel: proto.Channel{ID: bobDM, Kind: proto.ChanDM}, Display: "bob", Member: true},
		},
		nextID: 1000,
		echo:   true,
	}
}

func (f *fakeChat) Me() proto.User { return f.users[ownerID] }
func (f *fakeChat) User(id uint64) proto.User {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[id]
}
func (f *fakeChat) Channels() []client.ChannelView { return f.chans }
func (f *fakeChat) Channel(id uint64) (client.ChannelView, bool) {
	for _, c := range f.chans {
		if c.ID == id {
			return c, true
		}
	}
	return client.ChannelView{}, false
}
func (f *fakeChat) Messages(ch uint64, limit int) []client.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []client.Message
	for _, m := range f.msgs {
		if m.Chan == ch && m.Thread == 0 {
			m.Replies = f.replies(m.ID)
			out = append(out, m)
		}
	}
	return mcp.LastN(out, limit)
}
func (f *fakeChat) replies(id uint64) int {
	n := 0
	for _, m := range f.msgs {
		if m.Thread == id {
			n++
		}
	}
	return n
}
func (f *fakeChat) Thread(root uint64) []client.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []client.Message
	for _, m := range f.msgs {
		if m.ID == root || m.Thread == root {
			out = append(out, m)
		}
	}
	return out
}
func (f *fakeChat) Search(_ context.Context, q string, ch uint64) ([]proto.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []proto.Message
	for _, m := range f.msgs {
		if strings.Contains(m.Body, q) && (ch == 0 || m.Chan == ch) {
			out = append(out, m.Message)
		}
	}
	return out, nil
}
func (f *fakeChat) AgentOf(user uint64) (proto.Agent, bool) {
	if user == bobID {
		return proto.Agent{Handle: "bobbot", Owner: bobID}, true
	}
	return proto.Agent{}, false
}
func (f *fakeChat) UpdateAgent(_ context.Context, handle, icon string) (proto.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if handle != "" {
		f.handle = handle
	}
	return proto.Agent{Handle: f.AgentHandleLocked(), Icon: icon, Owner: ownerID}, nil
}
func (f *fakeChat) AgentHandleLocked() string {
	if f.handle == "" {
		return "rocky"
	}
	return f.handle
}
func (f *fakeChat) Agents() []proto.Agent {
	return []proto.Agent{{Handle: f.AgentHandle(), Owner: ownerID}, {Handle: "bobbot", Owner: bobID}}
}
func (f *fakeChat) Updates() <-chan client.Update { return f.updates }
func (f *fakeChat) Status() client.Status         { return client.Status{State: client.Online} }
func (f *fakeChat) AgentHandle() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.AgentHandleLocked()
}
func (f *fakeChat) OpenAgentDM(context.Context) (proto.Channel, error) {
	return proto.Channel{ID: agentDM, Kind: proto.ChanAgentDM}, nil
}
func (f *fakeChat) Send(ch uint64, body string, thread uint64) error {
	f.mu.Lock()
	f.sent = append(f.sent, posted{ch, thread, body})
	echo := f.echo
	var m client.Message
	if echo {
		f.nextID++
		m = client.Message{Message: proto.Message{ID: f.nextID, Chan: ch, Author: ownerID, Thread: thread, Body: body, Agent: f.AgentHandleLocked(), Time: time.Now().UnixMilli()}}
		f.msgs = append(f.msgs, m)
	}
	f.mu.Unlock()
	if echo {
		f.updates <- client.Update{New: []client.Message{m}, Msgs: map[uint64]bool{ch: true}}
	}
	return nil
}
func (f *fakeChat) SendAgentStatus(ch, thread uint64, state, status, preview string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live = append(f.live, liveMsg{ch, thread, state, status, preview, time.Now()})
	return nil
}

// say delivers a message from someone, as the server would.
func (f *fakeChat) say(author uint64, ch, thread uint64, body string, age time.Duration) client.Message {
	f.mu.Lock()
	f.nextID++
	m := client.Message{Message: proto.Message{ID: f.nextID, Chan: ch, Author: author, Thread: thread, Body: body, Time: time.Now().Add(-age).UnixMilli()}}
	f.msgs = append(f.msgs, m)
	f.mu.Unlock()
	f.updates <- client.Update{New: []client.Message{m}, Msgs: map[uint64]bool{ch: true}}
	return m
}

func (f *fakeChat) react(id uint64, emoji string, user uint64) {
	f.mu.Lock()
	var ch uint64
	for i := range f.msgs {
		if f.msgs[i].ID == id {
			r := map[string][]uint64{} // copy on write: readers hold the old map
			for k, v := range f.msgs[i].Reactions {
				r[k] = append([]uint64(nil), v...)
			}
			r[emoji] = append(r[emoji], user)
			f.msgs[i].Reactions = r
			ch = f.msgs[i].Chan
		}
	}
	f.mu.Unlock()
	f.updates <- client.Update{Msgs: map[uint64]bool{ch: true}}
}

func (f *fakeChat) posts() []posted {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]posted(nil), f.sent...)
}

func (f *fakeChat) lives() []liveMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]liveMsg(nil), f.live...)
}

// fakeRunner answers with a script per call.
type fakeRunner struct {
	mu    sync.Mutex
	reqs  []runner.Request
	reply func(req runner.Request, on func(runner.Event)) (runner.Result, error)
	cur   int
	most  int
}

func (r *fakeRunner) Run(ctx context.Context, req runner.Request, on func(runner.Event)) (runner.Result, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.cur++
	r.most = max(r.most, r.cur)
	f := r.reply
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.cur--
		r.mu.Unlock()
	}()
	if on == nil {
		on = func(runner.Event) {}
	}
	if f == nil {
		on(runner.Event{Kind: runner.EvText, Text: "hi"})
		return runner.Result{Session: req.Session, Text: "hi there", CostUSD: 0.1, ContextTokens: 1000, ContextWindow: 200000}, nil
	}
	return f(req, on)
}

func (r *fakeRunner) requests() []runner.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runner.Request(nil), r.reqs...)
}

type fakeMemory struct {
	mu       sync.Mutex
	facts    []memory.Fact
	handoffs map[string]string
	notes    map[string]memory.Note
	commits  int
}

func (m *fakeMemory) Remember(f memory.Fact) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.facts = append(m.facts, f)
	return nil
}
func (m *fakeMemory) Recall(q string, private bool) ([]memory.Hit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.notes == nil {
		return []memory.Hit{{Path: "facts/x.md", Line: 1, Text: q}}, nil
	}
	var out []memory.Hit
	for p, n := range m.notes {
		if strings.Contains(n.Body, q) {
			out = append(out, memory.Hit{Path: p, Line: 1, Text: n.Body})
		}
	}
	return out, nil
}
func (m *fakeMemory) WriteHandoff(key, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handoffs == nil {
		m.handoffs = map[string]string{}
	}
	m.handoffs[key] = text
	return nil
}
func (m *fakeMemory) Commit(string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commits++
	return nil
}
func (m *fakeMemory) Read(rel string) (memory.Note, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.notes[rel]
	if !ok {
		return memory.Note{}, os.ErrNotExist
	}
	return n, nil
}
func (m *fakeMemory) Write(n memory.Note) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.notes == nil {
		m.notes = map[string]memory.Note{}
	}
	m.notes[n.Path] = n
	return nil
}
func (m *fakeMemory) Forget(rel string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.notes[rel]; !ok {
		return os.ErrNotExist
	}
	delete(m.notes, rel)
	return nil
}
func (m *fakeMemory) PrepareDream() (*memory.Dream, error) {
	return &memory.Dream{Dir: os.TempDir()}, nil
}
func (m *fakeMemory) ApplyDream(*memory.Dream) error   { return nil }
func (m *fakeMemory) DiscardDream(*memory.Dream) error { return nil }

type harness struct {
	t      *testing.T
	s      *Supervisor
	chat   *fakeChat
	run    *fakeRunner
	mem    *fakeMemory
	cancel context.CancelFunc
	done   chan error
}

// shortDir is a temp dir short enough for the unix socket under it.
func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("/tmp", "tcag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func newHarness(t *testing.T, cfg home.Config, tweak func(*Supervisor)) *harness {
	t.Helper()
	dir := shortDir(t)
	cfg.Handle = "rocky"
	if err := home.SaveConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, chat: newFakeChat(), run: &fakeRunner{}, mem: &fakeMemory{}, done: make(chan error, 1)}
	h.s = &Supervisor{Dir: dir, Chat: h.chat, Runner: h.run, Memory: h.mem, TCBin: "/bin/tc", StatusEvery: 10 * time.Millisecond}
	// The DM already has a message: no greeting unless a test wants one.
	h.chat.msgs = append(h.chat.msgs, client.Message{Message: proto.Message{ID: 1, Chan: agentDM, Author: ownerID, Body: "hey", Time: time.Now().Add(-48 * time.Hour).UnixMilli()}})
	if tweak != nil {
		tweak(h.s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- h.s.Run(ctx) }()
	h.chat.updates <- client.Update{Status: true}
	t.Cleanup(h.stop)
	h.waitFor("supervisor up", func() bool {
		st, err := ReadStatus(home.PathsFor(dir).State)
		return err == nil && st.State != "starting"
	})
	return h
}

func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		h.t.Error("Run didn't stop")
	}
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	end := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(end) {
			h.t.Fatalf("timed out waiting for %s; posts %+v", what, h.chat.posts())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// call makes a tool call as the engine of the run in progress would.
func (h *harness) call(tool string, args any) mcp.BridgeRes {
	h.t.Helper()
	return h.callIn(0, tool, args)
}

// callIn calls tool as the run answering an ask in ch (0 = any run).
func (h *harness) callIn(ch uint64, tool string, args any) mcp.BridgeRes {
	h.t.Helper()
	var token string
	h.waitFor("a run", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		for k, r := range h.s.runs {
			if ch == 0 || r.ask.Chan == ch {
				token = k
			}
		}
		return token != ""
	})
	b, _ := json.Marshal(args)
	res, err := mcp.BridgeCall(context.Background(), h.s.sock, mcp.BridgeReq{Token: token, Tool: tool, Args: b})
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func cfgDefault() home.Config { return home.DefaultConfig() }
