package tui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// updateAvailMsg carries a newer release tag found by Options.UpdateCheck.
type updateAvailMsg string

// inviteArgs parses /invite [uses] [ttl]: a number of uses (1-100) and a
// duration like 72h, 3d or 30m, in either order.
func inviteArgs(args []string) (uses int, ttl time.Duration, err error) {
	const usage = "usage: /invite [uses 1-100] [ttl like 72h or 3d, at most 30d]"
	if len(args) > 2 {
		return 0, 0, errors.New(usage)
	}
	for _, a := range args {
		if n, err := strconv.Atoi(a); err == nil {
			if uses != 0 || n < 1 || n > proto.MaxInviteUses {
				return 0, 0, errors.New(usage)
			}
			uses = n
			continue
		}
		d, err := parseTTL(a)
		if err != nil || ttl != 0 || d < time.Minute || d > proto.MaxInviteTTL {
			return 0, 0, errors.New(usage)
		}
		ttl = d
	}
	return uses, ttl, nil
}

func parseTTL(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 1 || days > 30 {
			return 0, errors.New("bad days")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// GitHubInviteBackend is implemented by backends that can mint invites
// bound to a GitHub account's SSH keys (*client.Client does).
type GitHubInviteBackend interface {
	CreateGitHubInvite(ctx context.Context, login string, ttl time.Duration) (proto.InviteRes, error)
}

var _ GitHubInviteBackend = (*client.Client)(nil)

// keyArg removes "--key github:<login>" from args and returns the login
// ("" without --key).
func keyArg(args []string) (login string, rest []string, err error) {
	const usage = "usage: /invite --key github:user [ttl like 72h or 3d]"
	for i := 0; i < len(args); i++ {
		if args[i] != "--key" {
			rest = append(rest, args[i])
			continue
		}
		if login != "" || i+1 >= len(args) {
			return "", nil, errors.New(usage)
		}
		l, ok := proto.GitHubKeySpec(args[i+1])
		if !ok {
			return "", nil, errors.New(usage)
		}
		login = l
		i++
	}
	return login, rest, nil
}

// inviteErr turns the server's forbidden into what it means here.
func inviteErr(err error, what string) error {
	var pe *proto.Error
	if errors.As(err, &pe) && pe.Code == proto.ErrForbidden {
		return errors.New("only admins can " + what + " on this server")
	}
	return err
}

// inviteCommand mints an invite and copies the install one-liner with OSC
// 52; the notice shows the raw code for people who already have tc. With
// --key github:<login> the client fetches that account's SSH keys and binds
// the invite to them. Members may invite when the server allows it; the
// server enforces who and how much.
func (m *Model) inviteCommand(args []string) (tea.Cmd, error) {
	login, args, err := keyArg(args)
	if err != nil {
		return nil, err
	}
	uses, ttl, err := inviteArgs(args)
	if err != nil {
		return nil, err
	}
	var gh GitHubInviteBackend
	if login != "" {
		if uses > 1 {
			return nil, errors.New("--key invites are single-use: drop the uses")
		}
		var ok bool
		if gh, ok = m.c.(GitHubInviteBackend); !ok {
			return nil, errors.New("key-bound invites aren't supported here")
		}
	}
	be, out, version := m.c, m.opt.Out, m.opt.Version
	return m.callThen("invite", func(ctx context.Context) (tea.Msg, error) {
		var inv proto.InviteRes
		var err error
		if gh != nil {
			inv, err = gh.CreateGitHubInvite(ctx, login, ttl)
		} else {
			inv, err = be.CreateTeamInvite(ctx, uses, ttl)
		}
		if err != nil {
			return nil, inviteErr(err, "invite people")
		}
		line, err := proto.InstallCommand(inv.Code, version)
		if err != nil {
			return nil, err
		}
		if out != nil {
			_, _ = out.Write([]byte("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(line)) + "\a"))
		}
		what := "1 use"
		if inv.Uses > 1 {
			what = fmt.Sprintf("%d uses", inv.Uses)
		}
		if inv.Expires > 0 {
			what += " · expires " + time.UnixMilli(inv.Expires).Format("Jan 2 15:04")
		}
		if inv.Keys > 0 {
			what += fmt.Sprintf(" · only for github.com/%s's keys (%d)", login, inv.Keys)
		}
		return infoMsg("invite (" + what + ") · install command copied · code: " + sanitize.Line(inv.Code)), nil
	}), nil
}

// InviteBackend is implemented by backends that can list and revoke
// invites (*client.Client does); separate from Backend like HookBackend.
type InviteBackend interface {
	ListInvites(ctx context.Context) ([]proto.InviteInfo, error)
	RevokeInvite(ctx context.Context, id string) error
}

var _ InviteBackend = (*client.Client)(nil)

// inviteManageCommand runs /invites and /invite-revoke; ok is false for
// other commands.
func (m *Model) inviteManageCommand(c command) (cmd tea.Cmd, ok bool, err error) {
	if c.name != "invites" && c.name != "invite-revoke" {
		return nil, false, nil
	}
	be, has := m.c.(InviteBackend)
	if !has {
		return nil, true, errors.New("invite management isn't supported here")
	}
	if c.name == "invite-revoke" {
		if len(c.args) != 1 || !proto.ValidInviteID(strings.ToLower(c.args[0])) {
			return nil, true, errors.New("usage: /invite-revoke id (the 12-character id from /invites)")
		}
		id := strings.ToLower(c.args[0])
		return m.callThen("invite-revoke", func(ctx context.Context) (tea.Msg, error) {
			if err := be.RevokeInvite(ctx, id); err != nil {
				return nil, inviteErr(err, "revoke invites")
			}
			return infoMsg("invite " + id + " revoked"), nil
		}), true, nil
	}
	if len(c.args) != 0 {
		return nil, true, errors.New("usage: /invites")
	}
	names := map[uint64]string{}
	for id := range m.users {
		names[id] = m.atName(id)
	}
	return m.callThen("invites", func(ctx context.Context) (tea.Msg, error) {
		invs, err := be.ListInvites(ctx)
		if err != nil {
			return nil, inviteErr(err, "list invites")
		}
		if len(invs) == 0 {
			return infoMsg("no open invites"), nil
		}
		parts := make([]string, len(invs))
		for i, inv := range invs {
			parts[i] = describeInvite(inv, names)
		}
		return infoMsg("invites (/invite-revoke id): " + strings.Join(parts, " · ")), nil
	}), true, nil
}

// describeInvite is "<id> <kind> <n> left, exp <time>, by <who>". Every
// server-supplied field is sanitized or numeric.
func describeInvite(inv proto.InviteInfo, names map[uint64]string) string {
	kind := sanitize.Line(inv.Kind)
	switch inv.Kind {
	case proto.InviteKindUser:
		kind = "team"
	case proto.InviteKindKey:
		kind = "key"
		if n := names[inv.User]; n != "" {
			kind += " for " + n
		}
	}
	by := "server host"
	if inv.CreatedBy != 0 {
		if by = names[inv.CreatedBy]; by == "" {
			by = fmt.Sprintf("user%d", inv.CreatedBy)
		}
	}
	if inv.Keys > 0 {
		kind += fmt.Sprintf(" for %d keys", inv.Keys)
		if inv.KeySrc != "" {
			kind += " of " + sanitize.Line(inv.KeySrc)
		}
	}
	return fmt.Sprintf("%s %s %d left, exp %s, by %s", sanitize.Line(inv.ID), kind, inv.UsesLeft,
		time.UnixMilli(inv.Expires).Format("Jan 2 15:04"), by)
}

// updateCheckCmd runs Options.UpdateCheck off the UI goroutine; startup
// never waits for it.
func (m *Model) updateCheckCmd() tea.Cmd {
	check := m.opt.UpdateCheck
	if check == nil {
		return nil
	}
	return func() tea.Msg { return updateAvailMsg(check()) }
}

// sidebarTitle is "tc", plus the connection state when not online, else a
// dim update hint when a newer release exists. fit() truncates it.
func (m *Model) sidebarTitle() string {
	title := m.st.sideTitle.Render("tc")
	switch {
	case m.status.State != client.Online || m.status.Stopped:
		title += m.st.status.Render(" · " + m.statusText())
	case m.newRelease != "":
		title += m.st.status.Render(" · update " + sanitize.Line(m.newRelease))
	}
	return title
}
