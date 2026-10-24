package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Claude runs Claude Code headless (`claude -p`) on the owner's
// subscription. Every flag below exists in Claude Code 2.1.310; some
// (--permission-prompt-tool, --append-system-prompt-file) are hidden from
// --help.
type Claude struct {
	Bin string // "" = claude on PATH
	// Grace is how long the engine gets to exit after ctx ends before it
	// is killed (default 5 s).
	Grace time.Duration
}

// Talk-mode tools: look, don't touch. Writing happens through tc's MCP tools.
var talkTools = []string{"Read", "Glob", "Grep"}

// Edit-mode tools: files in the working directory, nothing else.
var editTools = []string{"Read", "Edit", "Write", "Glob", "Grep"}

// Work-mode tools: the shell runs in the OS sandbox from the settings.
var workTools = []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep", "TodoWrite"}

// Args is the command line for req, without the binary.
//
// Not used on purpose: --bare (it reads only ANTHROPIC_API_KEY, never the
// subscription login, and skips the memory hooks), and
// --setting-sources (--restricted already drops user and project settings
// in Talk; in Work the agent home is the project, so its own
// .claude/settings.json is the project setting).
func (c Claude) Args(req Request) []string {
	model := req.Model
	if model == "" {
		model = "sonnet"
	}
	a := []string{"-p",
		"--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--model", model,
		"--strict-mcp-config",
		// tc's own tools never prompt; servers the owner added go through
		// the permission tool like everything else.
		"--allowed-tools", "mcp__tc",
	}
	if req.MCPConfig != "" {
		a = append(a, "--mcp-config", req.MCPConfig)
	}
	if req.Settings != "" {
		a = append(a, "--settings", req.Settings)
	}
	if req.MaxUSD > 0 {
		a = append(a, "--max-budget-usd", strconv.FormatFloat(req.MaxUSD, 'f', 2, 64))
	}
	if req.System != "" {
		a = append(a, "--append-system-prompt", req.System)
	}
	switch req.Mode {
	case Work:
		a = append(a, "--setting-sources", "project",
			"--tools", strings.Join(workTools, ","),
			"--permission-mode", "acceptEdits")
		for _, d := range req.AddDirs {
			a = append(a, "--add-dir", d)
		}
	case Edit:
		a = append(a, "--restricted", "--tools", strings.Join(editTools, ","), "--permission-mode", "acceptEdits")
	default:
		a = append(a, "--restricted", "--tools", strings.Join(talkTools, ","))
	}
	if req.PermissionTool != "" {
		a = append(a, "--permission-prompt-tool", req.PermissionTool)
	} else {
		a = append(a, "--permission-prompts", "none")
	}
	if req.Session != "" {
		if req.Resume {
			a = append(a, "--resume", req.Session)
		} else {
			a = append(a, "--session-id", req.Session)
		}
	}
	// The prompt goes on stdin: it may start with "-" and can be long.
	return a
}

// Run implements Runner.
func (c Claude) Run(ctx context.Context, req Request, on func(Event)) (Result, error) {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	grace := c.Grace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	cmd := exec.Command(bin, c.Args(req)...)
	cmd.Dir = req.Dir
	cmd.Env = append(engineEnv(os.Environ()), req.Env...)
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &limitWriter{w: &stderr, n: 16 << 10}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("starting %s: %w", bin, err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-stop:
			return
		case <-ctx.Done():
		}
		// The whole group: hooks and MCP servers are its children.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-stop:
		case <-time.After(grace):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	res, perr := Parse(out, on)
	_, _ = io.Copy(io.Discard, out)
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if perr != nil {
		if werr != nil {
			return res, fmt.Errorf("%w (%v: %s)", perr, werr, lastLine(stderr.String()))
		}
		return res, perr
	}
	return res, nil
}

// engineEnv drops variables that would make the engine think it runs
// inside another Claude Code session or a herdr pane it doesn't own.
func engineEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "CLAUDECODE", k == "CLAUDE_CODE_ENTRYPOINT", strings.HasPrefix(k, "HERDR_"):
			continue
		}
		out = append(out, kv)
	}
	return out
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		q := p
		if len(q) > l.n {
			q = q[:l.n]
		}
		l.n -= len(q)
		_, _ = l.w.Write(q)
	}
	return len(p), nil
}

// ---- stream-json ----

type streamLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Event     *streamEvent    `json:"event"`
	Message   *streamMessage  `json:"message"`
	Parent    *string         `json:"parent_tool_use_id"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
	CostUSD   float64         `json:"total_cost_usd"`
	NumTurns  int             `json:"num_turns"`
	Denials   json.RawMessage `json:"permission_denials"`
	Model     map[string]struct {
		ContextWindow int `json:"contextWindow"`
	} `json:"modelUsage"`
}

type streamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

type streamMessage struct {
	Content []struct {
		Type  string         `json:"type"`
		Name  string         `json:"name"`
		Input map[string]any `json:"input"`
	} `json:"content"`
	Usage *struct {
		Input       int `json:"input_tokens"`
		CacheCreate int `json:"cache_creation_input_tokens"`
		CacheRead   int `json:"cache_read_input_tokens"`
		Output      int `json:"output_tokens"`
	} `json:"usage"`
}

// Parse reads Claude Code's stream-json from r until EOF, reporting events
// to on, and returns the result line. Lines it doesn't know are skipped:
// the format grows.
func Parse(r io.Reader, on func(Event)) (Result, error) {
	if on == nil {
		on = func(Event) {}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	var res Result
	got := false
	for sc.Scan() {
		var l streamLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if l.SessionID != "" {
			res.Session = l.SessionID
		}
		sub := l.Parent != nil && *l.Parent != ""
		switch l.Type {
		case "system":
			if l.Subtype == "init" {
				on(Event{Kind: EvInit, Session: l.SessionID})
			}
		case "stream_event":
			if l.Event != nil && !sub && l.Event.Type == "content_block_delta" && l.Event.Delta.Type == "text_delta" && l.Event.Delta.Text != "" {
				on(Event{Kind: EvText, Session: res.Session, Text: l.Event.Delta.Text})
			}
		case "assistant":
			if l.Message == nil {
				continue
			}
			if u := l.Message.Usage; u != nil && !sub {
				res.ContextTokens = u.Input + u.CacheCreate + u.CacheRead + u.Output
			}
			for _, c := range l.Message.Content {
				if c.Type == "tool_use" {
					on(Event{Kind: EvTool, Session: res.Session, Tool: c.Name, Input: c.Input})
				}
			}
		case "user":
			if l.Message == nil {
				continue
			}
			for _, c := range l.Message.Content {
				if c.Type == "tool_result" {
					on(Event{Kind: EvToolDone, Session: res.Session})
				}
			}
		case "result":
			got = true
			res.Text = l.Result
			res.IsError = l.IsError
			res.Subtype = l.Subtype
			res.CostUSD = l.CostUSD
			res.Turns = l.NumTurns
			var d []json.RawMessage
			if json.Unmarshal(l.Denials, &d) == nil {
				res.Denials = len(d)
			}
			for _, m := range l.Model {
				res.ContextWindow = max(res.ContextWindow, m.ContextWindow)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("reading engine output: %w", err)
	}
	if !got {
		return res, ErrNoResult
	}
	return res, nil
}

// IsBudgetStop reports whether the turn stopped at its spend cap.
func (r Result) IsBudgetStop() bool { return strings.Contains(r.Subtype, "budget") }
