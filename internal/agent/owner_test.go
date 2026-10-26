package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/agent/memory"
	"github.com/skailama/tc/internal/agent/runner"
	"github.com/skailama/tc/internal/mcp"
	"github.com/skailama/tc/internal/proto"
)

func blockingHarness(t *testing.T, cfg home.Config, tweak func(*Supervisor)) (*harness, chan struct{}) {
	h := newHarness(t, cfg, tweak)
	block := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-block:
		default:
			close(block)
		}
	})
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-block
		return runner.Result{Session: req.Session, Text: NoReply}, nil
	}
	return h, block
}

func TestPostWhereThread(t *testing.T) {
	h, _ := blockingHarness(t, cfgDefault(), nil)
	ask := h.chat.say(bobID, general, 0, "@rocky write up the whole plan", 0)
	if r := h.call(mcp.ToolPostReply, map[string]any{"chan": "#general", "where": "thread", "text": "the long plan"}); r.Text != "posted" {
		t.Fatalf("post %+v", r)
	}
	if p := h.chat.posts()[0]; p.ch != general || p.thread != ask.ID {
		t.Fatalf("posted %+v, want a thread on %d", p, ask.ID)
	}
}

func TestPeopleListsAgents(t *testing.T) {
	h, _ := blockingHarness(t, cfgDefault(), nil)
	h.chat.say(bobID, general, 0, "hi all", 0)
	h.chat.say(carolID, general, 0, "@rocky hey", 0)
	h.waitFor("run", func() bool { return len(h.run.requests()) == 1 })
	p := h.run.requests()[0].Prompt
	for _, want := range []string{"@carol", "@bob", "@bobbot (bob's agent)"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestOwnerEditsMemory(t *testing.T) {
	h, block := blockingHarness(t, cfgDefault(), nil)
	h.chat.say(bobID, general, 0, "@rocky forget everything about me", 0)
	if r := h.call(mcp.ToolForget, map[string]any{"note": "people/bob.md"}); !r.Error {
		t.Fatal("bob made it forget")
	}
	if r := h.call(mcp.ToolUpdateMemory, map[string]any{"note": "bob", "text": "x"}); !r.Error {
		t.Fatal("bob wrote memory")
	}
	close(block)
	h.waitFor("bob's run done", func() bool { h.s.mu.Lock(); defer h.s.mu.Unlock(); return len(h.s.runs) == 0 })

	block2 := make(chan struct{})
	defer close(block2)
	h.run.reply = func(req runner.Request, on func(runner.Event)) (runner.Result, error) {
		<-block2
		return runner.Result{Session: req.Session, Text: NoReply}, nil
	}
	h.chat.say(ownerID, agentDM, 0, "remember fahim prefers mornings, and forget the deploy note", 0)
	if r := h.call(mcp.ToolUpdateMemory, map[string]any{"note": "@fahim", "text": "prefers mornings"}); r.Error {
		t.Fatal(r.Text)
	}
	rel := memory.PersonPath("fahim")
	h.mem.mu.Lock()
	n := h.mem.notes[rel]
	h.mem.notes["facts/deploy.md"] = memory.Note{Path: "facts/deploy.md", Body: "deploy with make ship"}
	h.mem.mu.Unlock()
	if n.Body != "prefers mornings" || n.Trust != memory.TrustOwner || !n.Private {
		t.Fatalf("note %+v", n)
	}
	if r := h.call(mcp.ToolForget, map[string]any{"note": "make ship"}); r.Error || !strings.Contains(r.Text, "facts/deploy.md") {
		t.Fatalf("forget by words: %+v", r)
	}
	if r := h.call(mcp.ToolForget, map[string]any{"note": rel}); r.Error {
		t.Fatal(r.Text)
	}
	h.mem.mu.Lock()
	left := len(h.mem.notes)
	commits := h.mem.commits
	h.mem.mu.Unlock()
	if left != 0 || commits < 3 {
		t.Fatalf("notes left %d, commits %d", left, commits)
	}
}

func TestRefreshOnNewBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "tc")
	os.WriteFile(bin, []byte("v1"), 0o755)
	var refreshed atomic.Int32
	h := newHarness(t, cfgDefault(), func(s *Supervisor) {
		s.TCBin = bin
		s.Refresh = func() error { refreshed.Add(1); return nil }
	})
	h.chat.say(bobID, general, 0, "@rocky one", 0)
	h.waitFor("reply", func() bool { return len(h.chat.posts()) == 1 })
	if refreshed.Load() != 0 {
		t.Fatal("refreshed with no change")
	}
	os.WriteFile(bin, []byte("v2, a new tc"), 0o755)
	os.Chtimes(bin, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	h.chat.say(bobID, general, 0, "@rocky two", 0)
	h.waitFor("reply", func() bool { return len(h.chat.posts()) == 2 })
	if refreshed.Load() != 1 {
		t.Fatalf("refreshed %d times", refreshed.Load())
	}
}

func TestEnsureLeavesMemoryAlone(t *testing.T) {
	dir := t.TempDir()
	note := filepath.Join(home.PathsFor(dir).Memory, "people", "fahim.md")
	os.MkdirAll(filepath.Dir(note), 0o700)
	os.WriteFile(note, []byte("mine"), 0o600)
	os.WriteFile(home.PathsFor(dir).Code, []byte("an old code"), 0o600)
	if err := home.EnsureWith(dir, home.Options{TCBin: "/bin/tc", UserHome: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(note); string(b) != "mine" {
		t.Fatalf("memory touched: %q", b)
	}
	if b, _ := os.ReadFile(home.PathsFor(dir).Code); string(b) == "an old code" {
		t.Fatal("the code wasn't refreshed")
	}
}

func TestValidIcon(t *testing.T) {
	for _, ok := range []string{"orb", "owl:5", "kite:0", "⚡", "🦊", "👩‍💻"} {
		if !ValidIcon(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "orb:8", "orb:x", "owl:12", "a fox", "x", "⚡:3", strings.Repeat("🦊", 9)} {
		if ValidIcon(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if !strings.Contains(IconHelp(), "pebble ▟▙") || len(IconPresets) != 8 {
		t.Fatal(IconHelp())
	}
	_ = proto.MaxAgentIcon
}

func TestSetIdentityIcon(t *testing.T) {
	h, _ := blockingHarness(t, cfgDefault(), nil)
	h.chat.say(ownerID, agentDM, 0, "use the owl", 0)
	if r := h.call(mcp.ToolSetIdentity, map[string]string{"icon": "a fox"}); !r.Error || !strings.Contains(r.Text, "owl ◕◕") {
		t.Fatalf("bad icon: %+v", r)
	}
	if r := h.call(mcp.ToolSetIdentity, map[string]string{"icon": "owl:5"}); r.Error {
		t.Fatal(r.Text)
	}
	if cfg, _ := home.LoadConfig(h.s.Dir); cfg.Icon != "owl:5" {
		t.Fatalf("icon %q", cfg.Icon)
	}
}
