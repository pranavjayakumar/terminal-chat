package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
		for _, f := range []string{"uses", "ttl", "raw", "key"} {
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
	var inv proto.InviteRes
	var err error
	if c.key != "" {
		if testGitHubBase != "" {
			cl.SetGitHubBase(testGitHubBase)
		}
		inv, err = cl.CreateGitHubInvite(ctx, c.key, c.ttl)
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

// inviteErr says what the server's bare forbidden / not_found mean for
// invites, like the TUI does; anything else passes through.
func inviteErr(err error, what, id string) error {
	var pe *proto.Error
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case proto.ErrForbidden:
		msg := "only admins can " + what + " on this server"
		if pe.Msg != "" {
			msg += " (" + pe.Msg + ")"
		}
		return errors.New(msg)
	case proto.ErrNotFound:
		if id != "" {
			return fmt.Errorf("no open invite %s of yours (see tc invite --list)", id)
		}
	}
	return err
}
