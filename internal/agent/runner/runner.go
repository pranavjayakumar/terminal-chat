// Package runner runs one turn of a personal agent on an AI engine. The
// supervisor (internal/agent) decides what to run; a Runner only starts the
// engine with the locked-down launch for the ask's mode, reports what it is
// doing as it goes, and returns what it said and what it cost.
package runner

import (
	"context"
	"errors"
)

// Mode is how much an ask may do.
type Mode string

const (
	// Talk answers only: no shell, no file edits, file reads confined to
	// the agent home.
	Talk Mode = "talk"
	// Work may run the shell (sandboxed) and edit files in the granted
	// folders.
	Work Mode = "work"
	// Edit may read and edit files in the working directory only: no
	// shell (the memory dream).
	Edit Mode = "edit"
)

// Request is one turn.
type Request struct {
	Dir    string // working directory: the agent home (Work: its worktrees folder)
	Prompt string
	// Session continues an earlier session when Resume is set; otherwise
	// it names the new one (a UUID), so the caller knows the id before the
	// engine says it.
	Session string
	Resume  bool
	Mode    Mode
	Model   string  // "" = sonnet
	MaxUSD  float64 // per-run spend cap; 0 = none
	AddDirs []string
	// Deny are extra permission deny rules for this run only
	// ("Read(//abs/path/**)"), on top of the settings file's.
	Deny []string
	// MCPConfig is the --mcp-config JSON (the tc server plus any the owner
	// added); only these servers load. It holds the run's token and the
	// owner's MCP secrets, so it goes to the engine as a 0600 file in
	// StateDir, never on the command line.
	MCPConfig string
	StateDir  string // where the MCP config file goes; "" = the OS temp dir
	Settings  string // settings.json path (hooks, deny rules, sandbox)
	// System is appended to the engine's system prompt (the code).
	System string
	// PermissionTool answers permission prompts ("mcp__tc__approve");
	// "" denies anything that would prompt.
	PermissionTool string
	Env            []string // extra environment, KEY=value
}

// EventKind says what an Event carries.
type EventKind int

const (
	// EvInit: the engine started; Session is its session id.
	EvInit EventKind = iota
	// EvText: more reply text streamed (Text is the delta).
	EvText
	// EvTool: a tool call started (Tool, Input).
	EvTool
	// EvToolDone: a tool call returned.
	EvToolDone
)

// Event is something the engine did during a turn.
type Event struct {
	Kind    EventKind
	Session string
	Text    string
	Tool    string
	Input   map[string]any
}

// Result is how a turn ended.
type Result struct {
	Session string
	Text    string // the final reply text
	CostUSD float64
	IsError bool
	// Subtype is the engine's end reason ("success", "error_max_turns",
	// "error_max_budget_usd", …).
	Subtype string
	Turns   int
	// ContextTokens is how much of the context window the last model call
	// used; ContextWindow is the window's size (0 when unknown).
	ContextTokens int
	ContextWindow int
	Denials       int // tool calls the permission rules refused
}

// ContextPct is the share of the context window in use, 0..100.
func (r Result) ContextPct() int {
	if r.ContextWindow <= 0 {
		return 0
	}
	return r.ContextTokens * 100 / r.ContextWindow
}

// Runner runs one turn, calling on for each event as it happens (on may be
// nil). It returns once the engine has exited.
type Runner interface {
	Run(ctx context.Context, req Request, on func(Event)) (Result, error)
}

// ErrNoResult means the engine exited without reporting a result.
var ErrNoResult = errors.New("the engine exited without a result")
