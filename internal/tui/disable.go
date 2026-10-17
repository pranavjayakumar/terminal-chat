package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// /disable @user (admin) shuts an account out for good, e.g. after its SSH
// key was stolen. It can't be undone, so the question takes over the body
// (not the status line, where a background notice could replace it): y
// disables, any other key cancels. Like the trust overlays, keys typed in
// the first typeAhead are ignored (Esc still cancels).
type disableState struct {
	user   proto.User
	opened time.Time
}

// disableCommand runs /disable; ok is false for other commands.
func (m *Model) disableCommand(c command) (cmd tea.Cmd, ok bool, err error) {
	if c.name != "disable" {
		return nil, false, nil
	}
	if len(c.args) != 1 {
		return nil, true, errors.New("usage: /disable @user")
	}
	if !m.me.Admin {
		return nil, true, errors.New("only admins can disable users")
	}
	id, found := m.findUser(c.args[0])
	if !found {
		return nil, true, fmt.Errorf("unknown user %s", sanitize.Line(c.args[0]))
	}
	u := m.users[id]
	switch {
	case id == m.me.ID:
		return nil, true, errors.New("you can't disable yourself")
	case u.Disabled:
		return nil, true, fmt.Errorf("@%s is already disabled", sanitize.Line(u.Name))
	}
	m.dis = disableState{user: u, opened: m.now()}
	m.mode = modeDisable
	return nil, true, nil
}

func (m *Model) disableKey(key string) tea.Cmd {
	if m.now().Sub(m.dis.opened) < typeAhead && key != kEsc {
		return nil
	}
	u := m.dis.user
	m.dis = disableState{}
	m.mode = modeNormal
	name := sanitize.Line(u.Name)
	if key != kConfirmYes {
		m.setInfo("not disabled")
		return nil
	}
	be := m.c
	return m.callThen("disable", func(ctx context.Context) (tea.Msg, error) {
		if err := be.DisableUser(ctx, u.ID); err != nil {
			return nil, err
		}
		return infoMsg("✓ disabled @" + name + " — signed out everywhere; none of their keys can sign in"), nil
	})
}

func (m *Model) disableTitle(w int) string {
	name := "@" + sanitize.Line(m.dis.user.Name)
	if w < 60 {
		return "disable " + name + "? y: yes · other: no"
	}
	return "disable " + name + " for good? y: disable · any other key: cancel"
}

func (m *Model) disableLines(w, h int) []string {
	out := []string{fit(m.st.noticeErr.Render(truncate(m.disableTitle(w), w)), w), spaces(w)}
	for _, p := range []string{
		"· signs them out of every session now",
		"· none of their SSH keys can sign in again (an invite can't bring the account back)",
		"· deletes their unused invites and their webhooks",
		"· encrypted channels they were in get new keys, so they can't read what comes next",
		"",
		"It can't be undone. To let them back in, invite them again as a new account.",
	} {
		if p == "" {
			out = append(out, spaces(w))
			continue
		}
		for _, l := range wrapPlain(p, max(w-1, 1)) {
			out = append(out, fit(" "+strings.TrimSpace(l), w))
		}
	}
	return padLines(out, w, h)
}
