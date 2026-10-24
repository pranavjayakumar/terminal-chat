package home

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testOpts(t *testing.T) Options {
	t.Helper()
	uh := t.TempDir()
	for _, d := range []string{".config/gh", ".config/git", ".config/tc/agent", ".config/tc/other"} {
		if err := os.MkdirAll(filepath.Join(uh, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return Options{TCBin: "/usr/local/bin/tc", UserHome: uh, ExtraDomains: []string{"chat.example.com"}}
}

func TestEnsureLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	if err := EnsureWith(dir, testOpts(t)); err != nil {
		t.Fatal(err)
	}
	p := PathsFor(dir)
	for _, f := range []string{p.Code, p.Settings, p.Soul, p.Me, p.Config,
		filepath.Join(p.Skills, "setup", "SKILL.md"),
		filepath.Join(p.Skills, "talk-in-tc", "SKILL.md"),
		filepath.Join(p.Skills, "tc-repo", "SKILL.md"),
		filepath.Join(p.Skills, "open-issue", "SKILL.md"),
		filepath.Join(p.Skills, "open-pr", "SKILL.md"),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	for _, d := range []string{p.DeviceHome, p.State, p.Worktrees} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("missing dir %s", d)
		}
	}
	if _, err := os.Stat(p.Memory); err == nil {
		t.Error("Ensure created memory/; the memory store owns it")
	}
	def, err := IsDefaultSoul(dir)
	if err != nil || !def {
		t.Errorf("IsDefaultSoul = %v, %v; want true", def, err)
	}
}

func TestEnsureRejectsRelative(t *testing.T) {
	if err := EnsureWith("agent", testOpts(t)); err == nil {
		t.Fatal("want error for relative dir")
	}
}

func TestEnsureReplacesOwnedKeepsOwner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	o := testOpts(t)
	if err := EnsureWith(dir, o); err != nil {
		t.Fatal(err)
	}
	p := PathsFor(dir)
	mustWrite(t, p.Code, "tampered")
	mustWrite(t, p.Soul, "my own soul")
	mustWrite(t, p.Me, "about me")
	cfg := DefaultConfig()
	cfg.Handle = "rocky"
	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(p.Skills, "old-skill", "SKILL.md")
	mustWrite(t, stale, "old")
	var names []string
	mb, _ := os.ReadFile(filepath.Join(dir, ownedManifest))
	_ = json.Unmarshal(mb, &names)
	names = append(names, ".claude/skills/old-skill/SKILL.md", "../outside.txt", "soul.md")
	nb, _ := json.Marshal(names)
	mustWrite(t, filepath.Join(dir, ownedManifest), string(nb))
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	mustWrite(t, outside, "keep")
	memNote := filepath.Join(p.Memory, "MEMORY.md")
	mustWrite(t, memNote, "mine")

	if err := EnsureWith(dir, o); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p.Code); string(b) == "tampered" {
		t.Error("code not replaced")
	}
	for f, want := range map[string]string{p.Soul: "my own soul", p.Me: "about me", outside: "keep", memNote: "mine"} {
		if b, _ := os.ReadFile(f); string(b) != want {
			t.Errorf("%s = %q, want %q", f, b, want)
		}
	}
	got, err := LoadConfig(dir)
	if err != nil || got.Handle != "rocky" {
		t.Errorf("config handle = %q, %v", got.Handle, err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale tc-owned skill not removed")
	}
	if _, err := os.Stat(filepath.Dir(stale)); err == nil {
		t.Error("empty stale skill dir not removed")
	}
}

func TestEnsureIdempotentNoRewrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	o := testOpts(t)
	if err := EnsureWith(dir, o); err != nil {
		t.Fatal(err)
	}
	p := PathsFor(dir)
	fi1, _ := os.Stat(p.Settings)
	if err := EnsureWith(dir, o); err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Stat(p.Settings)
	if !os.SameFile(fi1, fi2) {
		t.Error("settings rewritten although unchanged")
	}
}

func TestSettingsJSON(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	b, err := SettingsJSON(dir, testOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string   `json:"type"`
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"hooks"`
		} `json:"hooks"`
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
		Sandbox struct {
			Enabled                  bool `json:"enabled"`
			AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
			Network                  struct {
				AllowedDomains []string `json:"allowedDomains"`
			} `json:"network"`
			Filesystem struct {
				DenyRead  []string `json:"denyRead"`
				AllowRead []string `json:"allowRead"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	for ev, name := range map[string]string{"SessionStart": "session-start", "Stop": "stop", "SessionEnd": "session-end", "PreCompact": "pre-compact"} {
		g := s.Hooks[ev]
		if len(g) != 1 || len(g[0].Hooks) != 1 {
			t.Fatalf("%s: bad hook shape", ev)
		}
		h := g[0].Hooks[0]
		want := []string{"agent", "hook", name, "--dir", dir}
		if h.Type != "command" || h.Command != "/usr/local/bin/tc" || strings.Join(h.Args, " ") != strings.Join(want, " ") {
			t.Errorf("%s hook = %+v", ev, h)
		}
	}
	deny := strings.Join(s.Permissions.Deny, "\n")
	for _, want := range []string{
		"Read(~/.ssh/**)", "Edit(~/.ssh/**)", "Read(~/.aws/**)", "Read(~/.gnupg/**)", "Read(~/Library/**)",
		"Read(~/.config/gh/**)", "Read(~/.config/git/**)", "Read(~/.config/tc/other/**)",
		"Read(/" + filepath.Join(dir, "home") + "/**)",
		"Edit(/" + filepath.Join(dir, "config.json") + ")",
		"Edit(/" + filepath.Join(dir, "soul.md") + ")",
		"Edit(/" + filepath.Join(dir, "memory") + "/**)",
	} {
		if !strings.Contains(deny, want) {
			t.Errorf("deny rules missing %s", want)
		}
	}
	for _, bad := range []string{"~/.config/tc/agent", "Read(~/.config/tc)", "Read(~/.config/**)"} {
		if strings.Contains(deny, bad+")") || strings.Contains(deny, bad+"/**)") {
			t.Errorf("deny rules block the agent home via %s", bad)
		}
	}
	if !s.Sandbox.Enabled || s.Sandbox.AllowUnsandboxedCommands {
		t.Error("sandbox must be on with no escape hatch")
	}
	doms := strings.Join(s.Sandbox.Network.AllowedDomains, " ")
	if !strings.Contains(doms, "github.com") || !strings.Contains(doms, "chat.example.com") {
		t.Errorf("domains = %s", doms)
	}
	if !contains(s.Sandbox.Filesystem.DenyRead, "~/.ssh") || !contains(s.Sandbox.Filesystem.DenyRead, "~/.config") || !contains(s.Sandbox.Filesystem.AllowRead, dir) {
		t.Errorf("sandbox filesystem = %+v", s.Sandbox.Filesystem)
	}
}

func TestConfigRoundTripAndDefaults(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := DefaultConfig()
	if c.Mode != ModeTalk || c.DailyCapUSD != 20 || c.RunCapUSD != 2 || c.AsksPerPerson != 15 || c.RolloverPct != 50 || c.IdleMinutes != 60 {
		t.Errorf("defaults = %+v", c)
	}
	c.Handle, c.Icon, c.Mode = "rocky", "⟡", ModeWork
	c.Access = Access{Folders: []string{"/tmp/x"}, Chans: []uint64{4, 9}, DMs: true}
	c.MCPServers = map[string]MCPServer{"notes": {Command: "notes-mcp", Args: []string{"--stdio"}}}
	c.HomeMachine, c.Paused = "mac-1", true
	if err := SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(got)
	cb, _ := json.Marshal(c)
	if string(gb) != string(cb) {
		t.Errorf("round trip:\n got %s\nwant %s", gb, cb)
	}
	fi, _ := os.Stat(PathsFor(dir).Config)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config perm = %v", fi.Mode().Perm())
	}

	mustWrite(t, PathsFor(dir).Config, `{"handle":"x"}`)
	got, err = LoadConfig(dir)
	if err != nil || got.Handle != "x" || got.DailyCapUSD != d.DailyCapUSD {
		t.Errorf("partial file: %+v, %v", got, err)
	}
	mustWrite(t, PathsFor(dir).Config, `{not json`)
	if _, err := LoadConfig(dir); err == nil {
		t.Error("want parse error")
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
		ok   bool
	}{
		{"default", func(*Config) {}, true},
		{"bad mode", func(c *Config) { c.Mode = "god" }, false},
		{"zero daily", func(c *Config) { c.DailyCapUSD = 0 }, false},
		{"run over daily", func(c *Config) { c.RunCapUSD = 50 }, false},
		{"zero asks", func(c *Config) { c.AsksPerPerson = 0 }, false},
		{"rollover low", func(c *Config) { c.RolloverPct = 5 }, false},
		{"rollover high", func(c *Config) { c.RolloverPct = 99 }, false},
		{"idle zero", func(c *Config) { c.IdleMinutes = 0 }, false},
		{"relative folder", func(c *Config) { c.Access.Folders = []string{"code"} }, false},
		{"crew", func(c *Config) { c.Mode = ModeCrew }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := DefaultConfig()
			tt.edit(&c)
			if err := c.Validate(); (err == nil) != tt.ok {
				t.Errorf("Validate() = %v, want ok=%v", err, tt.ok)
			}
			if err := SaveConfig(t.TempDir(), c); (err == nil) != tt.ok {
				t.Errorf("SaveConfig() = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestSoulAndMe(t *testing.T) {
	dir := t.TempDir()
	if s, err := ReadSoul(dir); err != nil || s != "" {
		t.Fatalf("missing soul = %q, %v", s, err)
	}
	if err := WriteSoul(dir, "  You are chill.\r\n"); err != nil {
		t.Fatal(err)
	}
	if s, _ := ReadSoul(dir); s != "You are chill.\n" {
		t.Errorf("soul = %q", s)
	}
	if def, _ := IsDefaultSoul(dir); def {
		t.Error("custom soul reported as default")
	}
	if err := WriteMe(dir, strings.Repeat("a", MeMaxBytes+1)); !errors.Is(err, ErrTooBig) {
		t.Errorf("big me: %v", err)
	}
	if err := WriteMe(dir, "   "); err == nil {
		t.Error("empty me accepted")
	}
	if err := WriteSoul(dir, "bad \xff"); err == nil {
		t.Error("invalid utf-8 accepted")
	}
	if err := WriteMe(dir, "Fizan. Builds tc."); err != nil {
		t.Fatal(err)
	}
	if s, _ := ReadMe(dir); s != "Fizan. Builds tc.\n" {
		t.Errorf("me = %q", s)
	}
}

func TestDir(t *testing.T) {
	if got := Dir("/h/.config/tc"); got != "/h/.config/tc/agent" {
		t.Errorf("Dir = %s", got)
	}
}

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
