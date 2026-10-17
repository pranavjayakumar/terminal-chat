package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
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
	// busy: a disable is in flight (it re-keys every shared encrypted
	// channel, which can take a while). Set here, cleared by the request's
	// goroutine, so it outlives the question.
	busy *atomic.Bool
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
	if m.dis.busy != nil && m.dis.busy.Load() {
		return nil, true, errors.New("a disable is still running")
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
	busy := m.dis.busy
	if busy == nil {
		busy = new(atomic.Bool)
	}
	m.dis = disableState{user: u, opened: m.now(), busy: busy}
	m.mode = modeDisable
	return nil, true, nil
}

func (m *Model) disableKey(key string) tea.Cmd {
	if m.now().Sub(m.dis.opened) < typeAhead && key != kEsc {
		return nil
	}
	u, busy := m.dis.user, m.dis.busy
	m.dis = disableState{busy: busy}
	m.mode = modeNormal
	name := sanitize.Line(u.Name)
	if key != kConfirmYes {
		m.setInfo("not disabled")
		return nil
	}
	if !busy.CompareAndSwap(false, true) {
		m.setInfo("a disable is still running")
		return nil
	}
	m.setInfo("disabling @" + name + "…")
	be, ctx := m.c, m.ctx
	return func() tea.Msg {
		defer busy.Store(false)
		ctx, cancel := context.WithTimeout(ctx, 2*reqTimeout)
		defer cancel()
		if err := be.DisableUser(ctx, u.ID); err != nil {
			return errMsg{"disable", err}
		}
		return infoMsg("✓ disabled @" + name + " — signed out everywhere; none of their keys can sign in")
	}
}

// disableLines puts the question and the keys first so they survive small
// sizes; the consequences follow as far as they fit.
func (m *Model) disableLines(w, h int) []string {
	name := "@" + sanitize.Line(m.dis.user.Name)
	red := func(s string) string { return fit(m.st.noticeErr.Render(truncate(s, w)), w) }
	keys := "y: disable · any other key: cancel"
	if w < 40 {
		keys = "y: yes · other: no"
	}
	out := []string{red("disable " + name + " for good?"), red(keys)}
	points := []string{
		"It can't be undone.",
		"Signs them out of every session now.",
		"None of their SSH keys can sign in again.",
		"Deletes their unused invites and their webhooks.",
		"Encrypted channels they were in get new keys, so they can't read what comes next.",
		"To let them back in, invite them as a new account (new key and name).",
	}
	if h >= len(points)+6 {
		out = append(out, spaces(w))
	}
	for i, p := range points {
		for j, l := range wrapPlain(p, max(w-3, 1)) {
			lead := "   "
			switch {
			case j == 0 && i == 0:
				lead = " "
			case j == 0:
				lead = " · "
			}
			out = append(out, fit(lead+strings.TrimSpace(l), w))
		}
	}
	return padLines(out, w, h)
}
