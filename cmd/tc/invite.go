package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// inviteCmd is a parsed `tc invite`: the client-side twin of `tc server
// invite`, for admins and (cap member_invites) members.
type inviteCmd struct {
	uses   int
	ttl    time.Duration
	key    string // GitHub login from --key github:<login>
	raw    bool
	list   bool
	revoke string
	// forUser (--for @name, admins) makes an add-key invite: it adds the
	// redeeming computer's SSH key to that existing account.
	forUser string
}

func parseInviteCmd(args []string) (inviteCmd, error) {
	var c inviteCmd
	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.IntVar(&c.uses, "uses", 0, fmt.Sprintf("how many people can join with it (1-%d; members 1-%d)", proto.MaxInviteUses, proto.MaxMemberInviteUses))
	fs.Func("ttl", "how long it stays valid, like 72h or 3d (default 7 days; at most 30d, members 7d)", func(v string) error {
		d, err := client.ParseTTL(v)
		if err != nil {
			return fmt.Errorf("%v (use a duration like 72h, 30m or 3d)", err)
		}
		c.ttl = d
		return nil
	})
	key := fs.String("key", "", "github:<login>: only a holder of one of that account's SSH keys can use it")
	fs.BoolVar(&c.raw, "raw", false, "print only the tc:// code")
	fs.BoolVar(&c.list, "list", false, "list open invites (yours, or all for admins)")
	fs.StringVar(&c.revoke, "revoke", "", "make the invite with this id (from --list) unusable at once")
	forUser := fs.String("for", "", "@user: (admins) add a new computer's SSH key to that account instead of inviting someone new")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return c, errUsage
		}
		return c, fmt.Errorf("invite: %v (see tc help)", err)
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("invite: unexpected argument %q (see tc help)", fs.Arg(0))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if c.list || set["revoke"] {
		if c.list && set["revoke"] {
			return c, errors.New("use --list or --revoke, not both")
		}
		for _, f := range []string{"uses", "ttl", "raw", "key", "for"} {
			if set[f] {
				return c, fmt.Errorf("--%s doesn't go with --list or --revoke", f)
			}
		}
		c.revoke = strings.ToLower(strings.TrimSpace(c.revoke))
		if set["revoke"] && !proto.ValidInviteID(c.revoke) {
			return c, fmt.Errorf("--revoke takes the %d-character id from --list", proto.InviteIDLen)
		}
		return c, nil
	}
	if set["for"] {
		name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(*forUser), "@"))
		if !proto.ValidName(name) {
			return c, errors.New("--for takes a user, e.g. --for @alice")
		}
		for _, f := range []string{"uses", "ttl", "key"} {
			if set[f] {
				return c, fmt.Errorf("--%s doesn't go with --for (add-key invites are single-use and last 7 days)", f)
			}
		}
		c.forUser = name
		return c, nil
	}
	if set["uses"] && (c.uses < 1 || c.uses > proto.MaxInviteUses) {
		return c, fmt.Errorf("--uses must be 1-%d", proto.MaxInviteUses)
	}
	if set["ttl"] && (c.ttl < time.Minute || c.ttl > proto.MaxInviteTTL) {
		return c, errors.New("--ttl must be between 1m and 30d")
	}
	if set["key"] {
		login, ok := proto.GitHubKeySpec(*key)
		if !ok {
			return c, errors.New("--key takes github:<login>, e.g. --key github:alice")
		}
		if c.uses > 1 {
			return c, errors.New("--key invites are single-use: drop --uses")
		}
		c.key = login
	}
	return c, nil
}

func cmdInvite(args []string) error {
	c, err := parseInviteCmd(args)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := connected(ctx)
	if err != nil {
		return err
	}
	defer cl.Close()
	return runInvite(ctx, cl, c, os.Stdout)
}

func runInvite(ctx context.Context, cl *client.Client, c inviteCmd, out io.Writer) error {
	switch {
	case c.list:
		invs, err := cl.ListInvites(ctx)
		if err != nil {
			return inviteErr(err, "list invites", "")
		}
		names := map[uint64]string{}
		for _, u := range cl.Users() {
			names[u.ID] = u.Name
		}
		printInvites(out, invs, func(id uint64) string { return names[id] }, "tc invite --revoke ID")
		return nil
	case c.revoke != "":
		if err := cl.RevokeInvite(ctx, c.revoke); err != nil {
			return inviteErr(err, "revoke invites", c.revoke)
		}
		fmt.Fprintf(out, "Revoked invite %s; it can't be used any more.\n", c.revoke)
		return nil
	}
	if c.forUser != "" {
		return runAddKeyInvite(ctx, cl, c, out)
	}
	var inv proto.InviteRes
	var err error
	if c.key != "" {
		if testGitHubBase != "" {
			cl.SetGitHubBase(testGitHubBase)
		}
		inv, err = cl.CreateGitHubInvite(ctx, c.key, c.ttl)
		err = githubKeysErr(err, c.key)
	} else {
		inv, err = cl.CreateTeamInvite(ctx, c.uses, c.ttl)
	}
	if err != nil {
		return inviteErr(err, "invite people", "")
	}
	if c.raw {
		fmt.Fprintln(out, sanitize.Line(inv.Code)) // from the server
		return nil
	}
	printInvite(out, inv, version)
	return nil
}

// runAddKeyInvite makes an add-key invite for c.forUser: whoever redeems it
// signs in as that user with their own SSH key, so a second computer
// never needs a copy of the first one's private key.
func runAddKeyInvite(ctx context.Context, cl *client.Client, c inviteCmd, out io.Writer) error {
	var u proto.User
	if c.forUser == proto.NameMe {
		u = cl.Me()
	} else {
		users := cl.Users()
		i := slices.IndexFunc(users, func(u proto.User) bool { return strings.ToLower(u.Name) == c.forUser })
		if i < 0 {
			return fmt.Errorf("no user @%s (see: tc users)", sanitize.Line(c.forUser))
		}
		u = users[i]
	}
	switch {
	case u.Bot:
		return fmt.Errorf("@%s is a bot; bots can't sign in", sanitize.Line(u.Name))
	case u.Disabled:
		return fmt.Errorf("@%s is disabled; invite them as a new account instead", sanitize.Line(u.Name))
	}
	code, err := cl.CreateInvite(ctx, u.ID)
	if err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) && pe.Code == proto.ErrForbidden {
			return fmt.Errorf("only admins can add an SSH key to an account; ask an admin to run: tc invite --for @%s", sanitize.Line(u.Name))
		}
		return inviteErr(err, "add SSH keys", "")
	}
	if c.raw {
		fmt.Fprintln(out, sanitize.Line(code)) // from the server
		return nil
	}
	printAddKeyInvite(out, code, u.Name, version)
	return nil
}

// printAddKeyInvite shows an add-key invite: who it is for and where to run it.
func printAddKeyInvite(w io.Writer, code, name, version string) {
	name = sanitize.Line(name)
	install, err := proto.InstallCommand(code, version)
	if err != nil || !proto.ShellSafe(code) || !proto.ValidName(name) {
		fmt.Fprintf(w, "\nAdd-key invite for @%s has unsafe characters, so it isn't shown as a command: %s\n\n", name, sanitize.Line(code))
		return
	}
	fmt.Fprintf(w, "\nAdd-key invite for @%s (single use, lasts 7 days).\n", name)
	fmt.Fprintf(w, "Run this on @%s's new computer: it installs tc and signs that computer in as @%s with its own SSH key\n", name, name)
	fmt.Fprintln(w, "(no need to copy a private key from another computer):")
	fmt.Fprintf(w, "\n  %s\n\n", sanitize.Line(install))
	fmt.Fprintf(w, "Already have tc there? Run:\n\n  tc join '%s' --name %s\n\n", code, name)
	fmt.Fprintf(w, "If it asks for a name, give any valid one: it's ignored, and the key joins @%s's account.\n", name)
	fmt.Fprintf(w, "People in encrypted chats with @%s see a key change for them and accept it with /accept-key @%s.\n", name, name)
}

// inviteErr says what the server's bare forbidden / not_found mean for
// invites, like the TUI does; anything else passes through.
func inviteErr(err error, what, id string) error {
	var pe *proto.Error
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case proto.ErrForbidden:
		if pe.Msg != "" {
			return errors.New(pe.Msg + " (only admins can " + what + " on this server)")
		}
		return errors.New("only admins can " + what + " on this server")
	case proto.ErrNotFound:
		if id != "" {
			return fmt.Errorf("no open invite %s of yours (see tc invite --list)", id)
		}
	case proto.ErrConflict:
		// The server's text points at the TUI's /invites.
		return fmt.Errorf("%s: see them with tc invite --list and revoke one with tc invite --revoke ID", sanitize.Line(pe.Msg))
	}
	return err
}

// githubKeysErr explains a GitHub login that doesn't exist (the key fetch
// answers 404) instead of printing the bare status.
func githubKeysErr(err error, login string) error {
	if errors.Is(err, client.ErrNoGitHubUser) {
		return fmt.Errorf("GitHub has no user %q; check the login (it's the name in github.com/<login>)", sanitize.Line(login))
	}
	return err
}
