package tui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

// nameCell renders an author name padded to nameW cells, with a dim bot tag
// for webhook authors (the name gives up room for it).
func (r *renderer) nameCell(as lipgloss.Style, name string, nameW int, bot bool) string {
	room := nameW
	if bot && nameW > len(botTag) {
		room -= len(botTag)
	} else {
		bot = false
	}
	n := truncate(name, room)
	out := as.Render(n)
	if bot {
		out += r.st.dim.Render(botTag)
	}
	return out + spaces(room-min(strWidth(n), room))
}

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
		ch, out := m.active, m.opt.Out
		return m.callThen("webhook", func(ctx context.Context) (tea.Msg, error) {
			h, err := be.CreateHook(ctx, ch, name)
			if err != nil {
				return nil, err
			}
			if out != nil {
				_, _ = out.Write([]byte("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(h.Curl)) + "\a"))
			}
			// Never the URL: it's the secret, and screens get shared.
			return infoMsg("webhook " + sanitize.Line(h.Name) + " created · curl command copied"), nil
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
			parts := make([]string, len(hooks))
			for i, h := range hooks {
				t := titles[h.Chan]
				if t == "" {
					t = "#?"
				}
				parts[i] = fmt.Sprintf("%d %s → %s", h.ID, h.Name, t)
			}
			return infoMsg("webhooks (id name → channel): " + strings.Join(parts, " · ")), nil
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
