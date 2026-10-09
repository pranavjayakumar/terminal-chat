package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// refreshMentionNames re-reads the groups from the cache and recomputes the
// names that @mention me (mine, then my groups). The log's markers are laid
// out again when they change.
func (m *Model) refreshMentionNames() {
	names := client.MentionNamesOf(m.me, m.c.Groups())
	if slices.Equal(names, m.mentionAs) {
		return
	}
	m.mentionAs = names
	m.main.ver++
	m.thread.ver++
}

// groupCommand runs /group and /groups; ok is false for other commands.
func (m *Model) groupCommand(c command) (cmd tea.Cmd, ok bool, err error) {
	switch c.name {
	case "groups":
		return m.showGroups(), true, nil
	case "group":
	default:
		return nil, false, nil
	}
	if len(c.args) == 0 {
		return nil, true, errors.New("usage: /group name @user… (no users deletes the group)")
	}
	if !m.me.Admin {
		return nil, true, errors.New("only admins can change groups")
	}
	name := strings.ToLower(strings.TrimPrefix(c.args[0], "@"))
	if !proto.ValidName(name) || strings.HasSuffix(name, proto.BotSuffix) {
		return nil, true, errors.New("group names are 1-32 of a-z 0-9 _ - (lowercase)")
	}
	if _, isUser := m.findUser(name); isUser {
		return nil, true, fmt.Errorf("%s is a user's name", sanitize.Line(name))
	}
	var ids []uint64
	for _, a := range c.args[1:] {
		id, found := m.findUser(a)
		if !found {
			return nil, true, fmt.Errorf("unknown user %s", sanitize.Line(a))
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) > proto.MaxGroupMembers {
		return nil, true, errors.New("a group has at most 50 members")
	}
	exists := slices.ContainsFunc(m.c.Groups(), func(g proto.Group) bool { return g.Name == name })
	if len(ids) == 0 && !exists {
		return nil, true, fmt.Errorf("no group @%s — /group %s @user… creates it", name, name)
	}
	be := m.c
	return m.callThen("group", func(ctx context.Context) (tea.Msg, error) {
		if err := be.SetGroup(ctx, name, ids); err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return infoMsg("deleted group @" + name), nil
		}
		return infoMsg(fmt.Sprintf("@%s is now %d %s", name, len(ids), plural(len(ids), "person", "people"))), nil
	}), true, nil
}

// showGroups opens the list of groups (from the cache, so it works offline).
func (m *Model) showGroups() tea.Cmd {
	gs := m.c.Groups()
	if len(gs) == 0 {
		m.setInfo("no groups — an admin can /group name @user… to make one")
		return nil
	}
	rows := make([]string, len(gs))
	for i, g := range gs {
		names := make([]string, len(g.Members))
		for j, id := range g.Members {
			names[j] = m.r.userName(id)
		}
		slices.Sort(names)
		mark := ""
		if slices.Contains(g.Members, m.me.ID) {
			mark = " (you're in it)"
		}
		rows[i] = fmt.Sprintf("@%s: %s%s", g.Name, strings.Join(names, ", "), mark)
	}
	m.list = listState{title: fmt.Sprintf("%d %s — mention one to ping everyone in it", len(gs), plural(len(gs), "group", "groups")), rows: rows}
	m.mode = modeList
	return nil
}
