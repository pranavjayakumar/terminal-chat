package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HerdrCLI talks to the herdr instance tc's pane lives in. herdr's CLI
// expects the positional argument (pane id, title) right after the
// subcommand, options after it.
type HerdrCLI struct {
	Pane, Tab, Workspace string
	bin                  string
	timeout              time.Duration // 0: execTimeout
}

// ErrNoHerdr means tc isn't running inside herdr.
var ErrNoHerdr = errors.New("not running inside herdr")

// DetectHerdr returns the herdr CLI for this pane, or nil when tc isn't
// running in herdr (HERDR_ENV, HERDR_PANE_ID and the herdr binary are all
// required).
func DetectHerdr(e Env) *HerdrCLI {
	if e.Getenv("HERDR_ENV") != "1" || e.Getenv("HERDR_PANE_ID") == "" {
		return nil
	}
	bin, err := e.LookPath("herdr")
	if err != nil {
		return nil
	}
	return &HerdrCLI{
		Pane: e.Getenv("HERDR_PANE_ID"), Tab: e.Getenv("HERDR_TAB_ID"),
		Workspace: e.Getenv("HERDR_WORKSPACE_ID"), bin: bin,
	}
}

// Pane is one herdr pane, as `herdr pane list` reports it.
type Pane struct {
	ID        string `json:"pane_id"`
	Tab       string `json:"tab_id"`
	Workspace string `json:"workspace_id"`
	Label     string `json:"label"` // user-set name; often empty
	Title     string `json:"title"` // reported metadata title
	Agent     string `json:"agent"`
	Terminal  string `json:"terminal_title_stripped"`
	Focused   bool   `json:"focused"`
}

// Name is the best human name for the pane, sanitized.
func (p Pane) Name() string {
	for _, s := range []string{p.Label, p.Title, p.Agent, p.Terminal, p.ID} {
		if s = clean(s, 40); s != "" {
			return s
		}
	}
	return "pane"
}

const maxHerdrOutput = 4 << 20

func (h *HerdrCLI) out(ctx context.Context, args ...string) ([]byte, error) {
	d := h.timeout
	if d == 0 {
		d = execTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	b, err := output(ctx, append([]string{h.bin}, args...), maxHerdrOutput)
	if err != nil {
		return nil, fmt.Errorf("herdr %s: %w", strings.Join(args[:min(len(args), 2)], " "), err)
	}
	return b, nil
}

// Panes lists the panes in this workspace.
func (h *HerdrCLI) Panes(ctx context.Context) ([]Pane, error) {
	args := []string{"pane", "list"}
	if h.Workspace != "" {
		args = append(args, "--workspace", h.Workspace)
	}
	b, err := h.out(ctx, args...)
	if err != nil {
		return nil, err
	}
	return ParsePanes(b)
}

// ParsePanes decodes `herdr pane list` output.
func ParsePanes(b []byte) ([]Pane, error) {
	var v struct {
		Result struct {
			Panes []Pane `json:"panes"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("parsing herdr pane list: %w", err)
	}
	return v.Result.Panes, nil
}

// ReadPane returns the last n lines of a pane's scrollback as text.
func (h *HerdrCLI) ReadPane(ctx context.Context, id string, n int) (string, error) {
	b, err := h.out(ctx, "pane", "read", id, "--source", "recent", "--lines", strconv.Itoa(n), "--format", "text")
	if err != nil {
		return "", err
	}
	return ParseRead(b), nil
}

// ParseRead accepts both the JSON envelope ({"result":{"text":...}}) and
// the plain text herdr 0.9 prints when stdout isn't a terminal.
func ParseRead(b []byte) string {
	var v struct {
		Result *struct {
			Text string `json:"text"`
		} `json:"result"`
	}
	if len(b) > 0 && b[0] == '{' && json.Unmarshal(b, &v) == nil && v.Result != nil {
		return v.Result.Text
	}
	return string(b)
}

// SetTitle reports this pane's display title ("" clears it).
func (h *HerdrCLI) SetTitle(ctx context.Context, title string) error {
	args := []string{"pane", "report-metadata", h.Pane, "--source", "tc"}
	if title = clean(title, maxTitle); title == "" {
		args = append(args, "--clear-title")
	} else {
		args = append(args, "--title", title)
	}
	_, err := h.out(ctx, args...)
	return err
}

// Self is this pane's id.
func (h *HerdrCLI) Self() string { return h.Pane }

// SelfTab is this pane's tab id.
func (h *HerdrCLI) SelfTab() string { return h.Tab }
