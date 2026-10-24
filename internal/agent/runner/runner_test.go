package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseRecorded(t *testing.T) {
	f, err := os.Open("testdata/read-file.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var text strings.Builder
	var tools []string
	var inits, dones int
	res, err := Parse(f, func(e Event) {
		switch e.Kind {
		case EvInit:
			inits++
		case EvText:
			text.WriteString(e.Text)
		case EvTool:
			tools = append(tools, e.Tool)
			if StatusLine(e.Tool, e.Input) != "reading hello.txt…" {
				t.Errorf("status line %q", StatusLine(e.Tool, e.Input))
			}
		case EvToolDone:
			dones++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if inits != 1 || dones != 1 || !slices.Equal(tools, []string{"Read"}) {
		t.Fatalf("inits %d dones %d tools %v", inits, dones, tools)
	}
	if text.String() != res.Text || !strings.Contains(res.Text, "hello from tc") {
		t.Fatalf("streamed %q, result %q", text.String(), res.Text)
	}
	if res.Session != "5021f3b6-3c8b-4f01-8ed3-141e05506613" || res.IsError || res.Subtype != "success" || res.Turns != 2 {
		t.Fatalf("result %+v", res)
	}
	if res.CostUSD != 0.00062082 {
		t.Fatalf("cost %v", res.CostUSD)
	}
	// The last call: 2 input + 203 cache creation + 3872 cache read + 3 output.
	if res.ContextTokens != 4080 || res.ContextWindow != 1000000 || res.ContextPct() != 0 {
		t.Fatalf("context %d / %d", res.ContextTokens, res.ContextWindow)
	}
}

func TestParseNoResult(t *testing.T) {
	_, err := Parse(strings.NewReader(`{"type":"system","subtype":"init","session_id":"s"}`+"\nnot json\n"), nil)
	if err != ErrNoResult {
		t.Fatalf("err %v", err)
	}
}

func TestParseSkipsSubagentText(t *testing.T) {
	in := `{"type":"stream_event","parent_tool_use_id":"toolu_1","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"inner"}}}
{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"outer"}}}
{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":2.01,"result":""}
`
	var got string
	res, err := Parse(strings.NewReader(in), func(e Event) {
		if e.Kind == EvText {
			got += e.Text
		}
	})
	if err != nil || got != "outer" || !res.IsBudgetStop() || !res.IsError {
		t.Fatalf("got %q res %+v err %v", got, res, err)
	}
}

func TestArgsTalk(t *testing.T) {
	a := Claude{}.Args(Request{Mode: Talk, Session: "s1", MaxUSD: 2, MCPConfig: "{}", Settings: "/h/.claude/settings.json", System: "code", PermissionTool: "mcp__tc__approve", AddDirs: []string{"/x"}})
	s := strings.Join(a, " ")
	for _, want := range []string{"-p ", "--output-format stream-json", "--verbose", "--include-partial-messages", "--model sonnet",
		"--strict-mcp-config", "--mcp-config {}", "--settings /h/.claude/settings.json", "--max-budget-usd 2.00",
		"--restricted", "--tools Read,Glob,Grep", "--permission-prompt-tool mcp__tc__approve", "--session-id s1", "--append-system-prompt code"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
	for _, bad := range []string{"--add-dir", "Bash", "--bare", "--resume", "--dangerously"} {
		if strings.Contains(s, bad) {
			t.Errorf("talk mode has %q: %s", bad, s)
		}
	}
}

func TestArgsWork(t *testing.T) {
	a := Claude{}.Args(Request{Mode: Work, Session: "s1", Resume: true, AddDirs: []string{"/a", "/b"}})
	s := strings.Join(a, " ")
	for _, want := range []string{"--tools Bash,", "--add-dir /a", "--add-dir /b", "--resume s1", "--permission-prompts none", "--setting-sources project"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
	if strings.Contains(s, "--restricted") || strings.Contains(s, "--session-id") {
		t.Errorf("work args: %s", s)
	}
}

// fakeClaude writes a script that records its arguments and stdin, then
// prints the recorded stream.
func fakeClaude(t *testing.T, body string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	fixture, _ := filepath.Abs("testdata/read-file.jsonl")
	bin = filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + dir + "/args\"\ncat > \"" + dir + "/stdin\"\n" +
		strings.ReplaceAll(body, "FIXTURE", fixture) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func TestRunFake(t *testing.T) {
	bin, dir := fakeClaude(t, `echo "TC_AGENT_CHAN=$TC_AGENT_CHAN" > "$(dirname "$0")/env"; cat FIXTURE`)
	t.Setenv("CLAUDECODE", "1")
	var n int
	res, err := Claude{Bin: bin}.Run(context.Background(), Request{Dir: dir, Prompt: "-hi there", Env: []string{"TC_AGENT_CHAN=7"}}, func(Event) { n++ })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "hello from tc") || n == 0 {
		t.Fatalf("res %+v events %d", res, n)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "stdin")); string(b) != "-hi there" {
		t.Fatalf("stdin %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "env")); strings.TrimSpace(string(b)) != "TC_AGENT_CHAN=7" {
		t.Fatalf("env %q", b)
	}
}

func TestRunFailsWithoutResult(t *testing.T) {
	bin, dir := fakeClaude(t, `echo "auth: not logged in" >&2; exit 1`)
	_, err := Claude{Bin: bin}.Run(context.Background(), Request{Dir: dir}, nil)
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err %v", err)
	}
}

func TestRunCancelKillsGroup(t *testing.T) {
	bin, dir := fakeClaude(t, `sleep 30 & wait`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Claude{Bin: bin, Grace: 100 * time.Millisecond}.Run(ctx, Request{Dir: dir}, nil)
	if err != context.DeadlineExceeded || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestEngineEnv(t *testing.T) {
	in := []string{"PATH=/bin", "HOME=/h", "SSH_AUTH_SOCK=/s", "SSH_AGENT_PID=1", "GH_TOKEN=x", "GITHUB_TOKEN=y",
		"ANTHROPIC_API_KEY=k", "AWS_SECRET_ACCESS_KEY=a", "OPENAI_API_KEY=o", "DB_PASSWORD=p", "HERDR_PANE_ID=w1",
		"CLAUDECODE=1", "CLAUDE_CODE_OAUTH_TOKEN=sub", "LANG=en", "TC_HOME=/t"}
	got := strings.Join(engineEnv(in), " ")
	if got != "PATH=/bin HOME=/h CLAUDE_CODE_OAUTH_TOKEN=sub LANG=en TC_HOME=/t" {
		t.Fatalf("env %s", got)
	}
}
