// Package home lays out a personal agent's home directory (~/.config/tc/agent)
// and owns its config file.
//
// The home has three owners. tc owns the code (CLAUDE.md), .claude/settings.json
// and .claude/skills, and replaces them whenever the shipped versions change.
// The person who owns the agent owns soul.md, me.md and config.json; tc writes
// defaults only when they are missing. The agent owns memory/, which this
// package never touches.
package home

import "path/filepath"

// Dir is the agent home for a tc home (TC_HOME, default ~/.config/tc).
func Dir(tcHome string) string { return filepath.Join(tcHome, "agent") }

// Paths names every file and folder in an agent home.
type Paths struct {
	Root     string // the agent home
	Code     string // CLAUDE.md, the code
	Settings string // .claude/settings.json
	// DreamSettings is .claude/dream-settings.json, the memory dream's.
	DreamSettings string
	Skills        string // .claude/skills
	Soul          string // soul.md
	Me            string // me.md
	Config        string // config.json
	DeviceHome    string // home/: the agent device's TC_HOME
	Memory        string // memory/: the agent's git-backed memory
	State         string // state/: sessions, ledger, activity, queue
	Repo          string // repo/: the tc clone
	Worktrees     string // worktrees/: one per piece of work
	Dream         string // dream/: the memory worktree a dream run edits
	// GoCache and GoModCache are the sandboxed shell's own Go caches: the
	// owner's would let it plant code the owner's next build runs.
	GoCache, GoModCache string
}

// PathsFor returns the layout of the agent home at dir.
func PathsFor(dir string) Paths {
	j := func(p ...string) string { return filepath.Join(append([]string{dir}, p...)...) }
	return Paths{
		Root:          dir,
		Code:          j("CLAUDE.md"),
		Settings:      j(".claude", "settings.json"),
		DreamSettings: j(".claude", "dream-settings.json"),
		Skills:        j(".claude", "skills"),
		Soul:          j("soul.md"),
		Me:            j("me.md"),
		Config:        j("config.json"),
		DeviceHome:    j("home"),
		Memory:        j("memory"),
		State:         j("state"),
		Repo:          j("repo"),
		Worktrees:     j("worktrees"),
		Dream:         j("dream"),
		GoCache:       j("cache", "go-build"),
		GoModCache:    j("cache", "gomod"),
	}
}
