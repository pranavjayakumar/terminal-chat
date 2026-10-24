package runner

import (
	"path/filepath"
	"strings"
)

// StatusLine is a short live status for a tool call ("reading the
// thread…"), or "" when the call isn't worth showing. Tool input comes
// from the model, so only file base names and short commands are shown.
func StatusLine(tool string, input map[string]any) string {
	str := func(k string) string {
		s, _ := input[k].(string)
		return s
	}
	short := func(s string, n int) string {
		s = strings.Join(strings.Fields(s), " ")
		if r := []rune(s); len(r) > n {
			return string(r[:n-1]) + "…"
		}
		return s
	}
	if name, ok := strings.CutPrefix(tool, "mcp__tc__"); ok {
		switch name {
		case "read_thread":
			return "reading the thread…"
		case "read_channel":
			return "reading the channel…"
		case "search":
			return "searching…"
		case "list_channels":
			return "looking around…"
		case "recall":
			return "remembering…"
		case "remember":
			return "taking a note…"
		case "ask_owner", "approve":
			return "waiting for an ok…"
		case "open_issue":
			return "opening an issue…"
		case "open_pr":
			return "opening a draft PR…"
		case "work_start":
			return "setting up a worktree…"
		}
		return ""
	}
	switch tool {
	case "Read":
		if p := str("file_path"); p != "" {
			return "reading " + short(filepath.Base(p), 40) + "…"
		}
		return "reading…"
	case "Grep", "Glob":
		return "searching files…"
	case "Edit", "Write":
		if p := str("file_path"); p != "" {
			return "editing " + short(filepath.Base(p), 40) + "…"
		}
		return "editing…"
	case "Bash":
		if c := str("command"); c != "" {
			return "running " + short(c, 48)
		}
		return "running a command…"
	case "WebFetch", "WebSearch":
		return "looking it up…"
	case "TodoWrite":
		return "planning…"
	}
	if strings.HasPrefix(tool, "mcp__") {
		return "using " + short(strings.TrimPrefix(tool, "mcp__"), 40) + "…"
	}
	return ""
}
