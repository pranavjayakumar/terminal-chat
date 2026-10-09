package tui

import (
	"errors"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/snippet"
)

type snippetMsg struct {
	body     string
	ch, root uint64
	err      error
}

// shareSnippet is /snippet path[:range] [message...]: it reads the file in a
// command (git may be consulted for a permalink) and then shows the same
// preview as /share-pane. Nothing is sent until Enter.
func (m *Model) shareSnippet(args []string) (tea.Cmd, error) {
	if len(args) == 0 {
		return nil, errors.New("usage: /snippet path[:start[-end]] [message]")
	}
	f := m.focus()
	if !f.open() {
		return nil, errors.New("no channel open")
	}
	o := snippet.Options{Ref: args[0], Context: snippet.DefaultContext, Message: strings.Join(args[1:], " "), Dir: m.opt.SnippetDir}
	ch, root := f.ch, f.root
	return func() tea.Msg {
		body, err := snippet.Build(o)
		return snippetMsg{body: body, ch: ch, root: root, err: err}
	}, nil
}

func (m *Model) gotSnippet(msg snippetMsg) {
	if msg.err != nil {
		m.setErr("snippet", errors.New(sanitize.Line(msg.err.Error())))
		return
	}
	if m.mode != modeNormal && m.mode != modeSelect {
		return // the user moved on to another overlay
	}
	if f := m.focus(); !f.open() || f.ch != msg.ch || f.root != msg.root {
		m.setInfo("snippet cancelled: you switched conversations")
		return
	}
	m.leaveSelect()
	m.share = shareState{body: msg.body, lines: strings.Split(msg.body, "\n"), ch: msg.ch, root: msg.root}
	m.mode = modeSharePreview
}
