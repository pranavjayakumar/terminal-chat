package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// HookBackend is implemented by backends that support webhooks
// (*client.Client does). It's separate from Backend so test fakes and
// older backends needn't implement it.
type HookBackend interface {
	CreateHook(ctx context.Context, ch uint64, name string) (client.NewHook, error)
	ListHooks(ctx context.Context) ([]proto.Hook, error)
	DeleteHook(ctx context.Context, id uint64) error
}

const botTag = " bot"

// hookCommand runs /webhook, /webhooks and /webhook-delete; ok is false for
// other commands.
func (m *Model) hookCommand(c command) (cmd tea.Cmd, ok bool, err error) {
	switch c.name {
	case "webhook", "webhooks", "webhook-delete":
	default:
		return nil, false, nil
	}
	be, has := m.c.(HookBackend)
	if !has {
		return nil, true, errors.New("webhooks aren't supported here")
	}
	switch c.name {
	case "webhook":
		if len(c.args) != 1 {
			return nil, true, errors.New("usage: /webhook name")
		}
		name := strings.ToLower(strings.TrimPrefix(c.args[0], "@"))
		if _, ok := proto.BotName(name); !ok {
			return nil, true, errors.New(`webhook names are a-z 0-9 _ - (lowercase), at most 32 with "-bot" added`)
		}
		if m.active == 0 {
			return nil, true, errors.New("no channel open")
		}
		ch, opt := m.active, m.opt
		return m.callThen("webhook", func(ctx context.Context) (tea.Msg, error) {
			h, err := be.CreateHook(ctx, ch, name)
			if err != nil {
				return nil, err
			}
			how, _ := clip(ctx, opt, h.Curl)
			if how == "system" {
				// Never the URL unasked: it's the secret, and screens get shared.
				return infoMsg("webhook " + sanitize.Line(h.Name) + " created · curl command copied"), nil
			}
			// Only OSC 52 (or nothing) carried it, which many terminals
			// ignore: the server shows the URL once, so offer it on screen.
			return hookShownMsg{name: sanitize.Line(h.Name), curl: h.Curl}, nil
		}), true, nil
	case "webhooks":
		titles := make(map[uint64]string, len(m.chans))
		for _, v := range m.chans {
			titles[v.ID] = m.chanTitle(v.ID)
		}
		return m.callThen("webhooks", func(ctx context.Context) (tea.Msg, error) {
			hooks, err := be.ListHooks(ctx)
			if err != nil {
				return nil, err
			}
			if len(hooks) == 0 {
				return infoMsg("no webhooks"), nil
			}
			rows := make([]string, len(hooks))
			for i, h := range hooks {
				t := titles[h.Chan]
				if t == "" {
					t = "#?"
				}
				rows[i] = fmt.Sprintf("%d %s → %s", h.ID, h.Name, t)
			}
			return listMsg{title: fmt.Sprintf("%d %s — id name → channel · /webhook-delete id", len(hooks), plural(len(hooks), "webhook", "webhooks")), rows: rows}, nil
		}), true, nil
	default: // webhook-delete
		if len(c.args) != 1 {
			return nil, true, errors.New("usage: /webhook-delete id")
		}
		id, err := strconv.ParseUint(strings.TrimPrefix(c.args[0], "#"), 10, 64)
		if err != nil || id == 0 {
			return nil, true, errors.New("usage: /webhook-delete id (see /webhooks)")
		}
		return m.callThen("webhook-delete", func(ctx context.Context) (tea.Msg, error) {
			if err := be.DeleteHook(ctx, id); err != nil {
				return nil, err
			}
			return infoMsg("webhook deleted"), nil
		}), true, nil
	}
}

// hookShownMsg is a new webhook whose curl command may not have reached the
// clipboard.
type hookShownMsg struct{ name, curl string }

// showHook opens the viewer with the curl command hidden until s.
func (m *Model) showHook(h hookShownMsg) {
	m.view = viewer{
		title:  "webhook " + h.name,
		copy:   h.curl,
		back:   modeNormal,
		secret: true,
		wide:   true,
	}
	m.view.build = func(w int) []string {
		w = max(w, 8)
		out := wrapWords("webhook "+h.name+" created. The curl command went to the terminal clipboard (OSC 52), which some terminals ignore (Terminal.app; tmux without set-clipboard on).", w)
		out = append(out, "")
		if !m.view.shown {
			return append(out, wrapWords("s shows it here: it holds the webhook's secret URL, anyone with it can post as "+h.name+" — mind shared screens. y copies it again. Esc closes; it is never shown again.", w)...)
		}
		out = append(out, wrapWords("Lines are cut to fit; y copies the exact text. Esc closes; it is never shown again.", w)...)
		out = append(out, "")
		for _, l := range strings.Split(strings.TrimRight(sanitize.Text(h.curl), "\n"), "\n") {
			for r := []rune(l); ; {
				n := min(len(r), w)
				out = append(out, string(r[:n]))
				if r = r[n:]; len(r) == 0 {
					break
				}
			}
		}
		return out
	}
	m.mode = modeViewer
	m.setInfo("webhook " + h.name + " created")
}
