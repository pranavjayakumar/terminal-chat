package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
// duration like 72h, 3d or 30m, in either order. The server applies the
// tighter member limits.
func inviteArgs(args []string) (uses int, ttl time.Duration, err error) {
	const usage = "usage: /invite [uses 1-100, members 1-10] [ttl like 72h or 3d, at most 30d, members 7d]"
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

func parseTTL(s string) (time.Duration, error) { return client.ParseTTL(s) }

// GitHubInviteBackend is implemented by backends that can mint invites
// bound to a GitHub account's SSH keys (*client.Client does).
type GitHubInviteBackend interface {
	CreateGitHubInvite(ctx context.Context, login string, ttl time.Duration) (proto.InviteRes, error)
}

var _ GitHubInviteBackend = (*client.Client)(nil)

// keyArg removes "--key github:<login>" from args and returns the login
// ("" without --key).
func keyArg(args []string) (login string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] != "--key" {
			rest = append(rest, args[i])
			continue
		}
		if login != "" {
			return "", nil, errors.New("give --key once")
		}
		if i+1 >= len(args) {
			return "", nil, errors.New("usage: /invite --key github:user [ttl like 72h or 3d]")
		}
		l, ok := proto.GitHubKeySpec(args[i+1])
		if !ok {
			if proto.ValidKeySrc("github:" + args[i+1]) {
				return "", nil, errors.New("--key needs github:<user> (did you mean github:" + args[i+1] + "?)")
			}
			return "", nil, errors.New("--key needs github:<user>, a GitHub user name")
		}
		login = l
		i++
	}
	return login, rest, nil
}

// inviteErr turns the server's forbidden into what it means here, keeping
// any reason the server gave (sanitized when shown).
func inviteErr(err error, what string) error {
	var pe *proto.Error
	if errors.As(err, &pe) && pe.Code == proto.ErrForbidden {
		if pe.Msg != "" {
			return errors.New(pe.Msg + " (only admins can " + what + " on this server)")
		}
		return errors.New("only admins can " + what + " on this server")
	}
	return err
}

// boundLabel is how a key binding reads everywhere: "github:carol only (2 keys)".
func boundLabel(src string, keys int64) string {
	n := "1 key"
	if keys != 1 {
		n = fmt.Sprintf("%d keys", keys)
	}
	if src == "" {
		return "bound to " + n
	}
	return sanitize.Line(src) + " only (" + n + ")"
}

// inviteCommand mints an invite, copies the install one-liner (OSC 52, tmux
// passthrough, pbcopy & co) and shows code and one-liner in full. With
// --key github:<login> the client fetches that account's SSH keys and binds
// the invite to them. Members may invite when the server allows it; the
// server enforces who and how much.
func (m *Model) inviteCommand(args []string) (tea.Cmd, error) {
	if slices.ContainsFunc(args, func(a string) bool { return a == "--for" || strings.HasPrefix(a, "--for=") }) {
		if len(args) == 1 && strings.HasPrefix(args[0], "--for=") {
			args = []string{"--for", strings.TrimPrefix(args[0], "--for=")}
		}
		return m.addKeyInviteCommand(args)
	}
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
	be, opt, version := m.c, m.opt, m.opt.Version
	if gh != nil {
		// The GitHub fetch can take seconds; say something meanwhile.
		m.setInfo("fetching github.com/" + login + "'s SSH keys…")
	}
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
		how, cerr := clip(ctx, opt, line)
		// What matters first: narrow panes cut the notice from the right.
		parts := []string{"invite created"}
		if inv.Keys > 0 {
			parts = append(parts, "for "+boundLabel("github:"+login, inv.Keys))
		}
		if inv.Uses > 1 {
			parts = append(parts, fmt.Sprintf("%d uses", inv.Uses))
		} else {
			parts = append(parts, "1 use")
		}
		if inv.Expires > 0 {
			parts = append(parts, "expires "+time.UnixMilli(inv.Expires).Format("Jan 2 15:04"))
		}
		return inviteShownMsg{summary: strings.Join(parts, " · "), code: sanitize.Line(inv.Code),
			line: line, how: how, copyErr: cerr}, nil
	}), nil
}

// addKeyInviteCommand runs /invite --for @user (admins): an add-key invite
// that signs a new computer in as that user with its own SSH key, so no
// private key has to be copied between computers. The server refuses
// anyone but an admin.
func (m *Model) addKeyInviteCommand(args []string) (tea.Cmd, error) {
	const usage = "usage: /invite --for @user (add-key invites are single-use and last 7 days)"
	if len(args) != 2 || args[0] != "--for" {
		return nil, errors.New(usage)
	}
	id, ok := m.findUser(args[1])
	if !ok {
		return nil, errors.New("no user " + sanitize.Line(args[1]))
	}
	u := m.users[id]
	if id == m.me.ID && u.ID == 0 {
		u = m.me
	}
	name := sanitize.Line(u.Name)
	switch {
	case u.Bot:
		return nil, errors.New("@" + name + " is a bot; bots can't sign in")
	case u.Disabled:
		return nil, errors.New("@" + name + " is disabled; invite them as a new account instead")
	}
	be, opt, version := m.c, m.opt, m.opt.Version
	return m.callThen("invite", func(ctx context.Context) (tea.Msg, error) {
		code, err := be.CreateInvite(ctx, id)
		if err != nil {
			var pe *proto.Error
			if errors.As(err, &pe) && pe.Code == proto.ErrForbidden {
				return nil, errors.New("only admins can add an SSH key to an account")
			}
			return nil, err
		}
		line, err := proto.InstallCommand(code, version)
		if err != nil {
			return nil, err
		}
		how, cerr := clip(ctx, opt, line)
		return inviteShownMsg{
			summary: "add-key invite for @" + name + " · 1 use · lasts 7 days · run it on their new computer: it signs in as @" + name +
				" with that computer's own SSH key (if asked for a name, any valid one: it's ignored)",
			code: sanitize.Line(code), line: line, how: how, copyErr: cerr}, nil
	}), nil
}

// inviteShownMsg carries a new invite to the UI, which shows it in full:
// the status line truncates, and the copy can silently fail (tmux), so the
// code must stay readable and selectable on screen.
type inviteShownMsg struct {
	summary, code, line, how string
	copyErr                  error
}

func (m *Model) showInvite(s inviteShownMsg) {
	copied := "NOT copied: select the text below"
	switch s.how {
	case "system":
		copied = "copied to the clipboard"
	case "terminal":
		copied = "sent to terminal clipboard"
	}
	m.view = viewer{
		title: "invite",
		build: func(w int) []string { return inviteLines(s, copied, w) },
		copy:  s.line,
		back:  modeNormal,
		wide:  true,
	}
	m.mode = modeViewer
	m.setInfo("invite created")
}

// inviteLines lays the invite out for width w: copy result first, then the
// summary, the code in chunks, and the install command broken at spaces with
// backslash continuations so pasting several rows still runs in sh.
func inviteLines(s inviteShownMsg, copied string, w int) []string {
	w = max(w, 8)
	out := wrapWords(copied, w)
	out = append(out, "")
	out = append(out, wrapWords(s.summary, w)...)
	out = append(out, "", "code:")
	for r := []rune(s.code); len(r) > 0; {
		n := min(len(r), w)
		out = append(out, string(r[:n]))
		r = r[n:]
	}
	out = append(out, "", "install command:")
	return append(out, wrapShell(s.line, w)...)
}

// wrapShell breaks a shell command into rows of at most w columns that still
// run when pasted together: a backslash-newline is removed by sh even inside
// a word, and a quoted word is cut as adjacent quoted pieces.
func wrapShell(line string, w int) []string {
	var out []string
	row := ""
	flush := func() { out = append(out, row+" \\"); row = "" }
	for _, tok := range strings.Fields(line) {
		if row != "" && len(row)+1+len(tok)+2 <= w {
			row += " " + tok
			continue
		}
		if row != "" {
			flush()
		}
		if len(tok)+2 <= w {
			row = tok
			continue
		}
		quoted := len(tok) >= 2 && tok[0] == '\'' && tok[len(tok)-1] == '\''
		body, q, size := tok, "", w-1
		if quoted {
			body, q, size = tok[1:len(tok)-1], "'", w-3
		}
		size = max(size, 1)
		for len(body) > size {
			out = append(out, q+body[:size]+q+"\\")
			body = body[size:]
		}
		row = q + body + q
	}
	if row != "" {
		out = append(out, row)
	}
	return out
}

// wrapWords breaks text at spaces into rows of at most w columns (a longer
// word stays whole).
func wrapWords(text string, w int) []string {
	var out []string
	row := ""
	for _, tok := range strings.Fields(text) {
		switch {
		case row == "":
			row = tok
		case strWidth(row)+1+strWidth(tok) <= w:
			row += " " + tok
		default:
			out = append(out, row)
			row = tok
		}
	}
	return append(out, row)
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
				var pe *proto.Error
				if errors.As(err, &pe) && pe.Code == proto.ErrNotFound {
					return nil, errors.New("no open invite " + id + " of yours (see /invites)")
				}
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
		rows := make([]string, len(invs))
		for i, inv := range invs {
			rows[i] = describeInvite(inv, names) // id first: it survives a narrow pane
		}
		return listMsg{title: fmt.Sprintf("%d open %s · /invite-revoke id", len(invs), plural(len(invs), "invite", "invites")), rows: rows}, nil
	}), true, nil
}

// describeInvite is "<id> <kind>[, <binding>], <n> left, exp <time>, by
// <who>". Every server-supplied field is sanitized or numeric.
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
		kind += ", " + boundLabel(inv.KeySrc, inv.Keys)
	}
	return fmt.Sprintf("%s %s, %d left, exp %s, by %s", sanitize.Line(inv.ID), kind, inv.UsesLeft,
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
