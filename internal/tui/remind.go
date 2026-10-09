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

// remindCommand runs /remind, /reminders and /remind-delete; ok is false for
// other commands. The server keeps the reminders and posts them in my notes
// when due, so they fire with this program closed.
func (m *Model) remindCommand(c command) (cmd tea.Cmd, ok bool, err error) {
	switch c.name {
	case "remind", "reminders", "remind-delete":
	default:
		return nil, false, nil
	}
	be := m.c
	switch c.name {
	case "remind":
		at, text, err := client.ParseRemind(c.rest, m.now())
		if err != nil {
			return nil, true, err
		}
		ch := m.active
		if v, found := m.chanView(ch); found && v.Kind == proto.ChanDM && len(v.Members) == 1 {
			ch = 0 // my own notes: "(from …)" would only say where it lands
		}
		now := m.now()
		return m.callThen("remind", func(ctx context.Context) (tea.Msg, error) {
			id, err := be.Remind(ctx, at, text, ch)
			if err != nil {
				return nil, err
			}
			return infoMsg(fmt.Sprintf("reminder %d set for %s", id, client.FormatReminderTime(at.UnixMilli(), now))), nil
		}), true, nil
	case "reminders":
		titles := make(map[uint64]string, len(m.chans))
		for _, v := range m.chans {
			titles[v.ID] = m.chanTitle(v.ID)
		}
		now := m.now()
		return m.callThen("reminders", func(ctx context.Context) (tea.Msg, error) {
			rs, err := be.Reminders(ctx)
			if err != nil {
				return nil, err
			}
			if len(rs) == 0 {
				return infoMsg("no reminders — /remind 10m text, /remind tomorrow 9am text"), nil
			}
			rows := make([]string, len(rs))
			for i, r := range rs {
				rows[i] = fmt.Sprintf("%d  %s  %s", r.ID, client.FormatReminderTime(r.At, now), firstLine(r.Text))
				if t := titles[r.Chan]; r.Chan != 0 && t != "" {
					rows[i] += " (from " + t + ")"
				}
			}
			return listMsg{title: fmt.Sprintf("%d %s — id, when, text · /remind-delete id", len(rs), plural(len(rs), "reminder", "reminders")), rows: rows}, nil
		}), true, nil
	default: // remind-delete
		if len(c.args) != 1 {
			return nil, true, errors.New("usage: /remind-delete id (see /reminders)")
		}
		id, err := strconv.ParseUint(strings.TrimPrefix(c.args[0], "#"), 10, 64)
		if err != nil || id == 0 {
			return nil, true, errors.New("usage: /remind-delete id (see /reminders)")
		}
		return m.callThen("remind-delete", func(ctx context.Context) (tea.Msg, error) {
			if err := be.DeleteReminder(ctx, id); err != nil {
				return nil, err
			}
			return infoMsg(fmt.Sprintf("reminder %d deleted", id)), nil
		}), true, nil
	}
}

// firstLine is s up to its first line break, with "…" if more follows.
func firstLine(s string) string {
	l, rest, more := strings.Cut(s, "\n")
	if more && strings.TrimSpace(rest) != "" {
		l += " …"
	}
	return sanitize.Line(l)
}

// isReminder reports whether x is a delivered reminder in my notes.
func (m *Model) isReminder(x proto.Message) bool {
	v, ok := m.chanView(x.Chan)
	return ok && client.IsReminder(x, m.me.ID, v.Channel)
}
