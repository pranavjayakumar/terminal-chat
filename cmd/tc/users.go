package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// usersBackend is what tc users needs from *client.Client.
type usersBackend interface {
	Me() proto.User
	ListUsers(ctx context.Context) (proto.UsersRes, error)
	DisableUser(ctx context.Context, user uint64) error
}

// tc users | tc users disable @name [--yes]
func cmdUsers(args []string) error {
	if _, err := parseUsers(args); err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	c, err := connected(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return users(ctx, c, args, os.Stdin, os.Stdout)
}

type usersArgs struct {
	disable string // name without the @; "" lists
	yes     bool
}

func parseUsers(args []string) (usersArgs, error) {
	var a usersArgs
	if len(args) > 0 && (args[0] == "list" || args[0] == "ls") {
		args = args[1:]
	}
	if len(args) == 0 {
		return a, nil
	}
	if args[0] != "disable" {
		return a, errUsage
	}
	for _, x := range args[1:] {
		switch {
		case x == "--yes" || x == "-y":
			a.yes = true
		case strings.HasPrefix(x, "@") && len(x) > 1 && a.disable == "":
			a.disable = x[1:]
		default:
			return a, errUsage
		}
	}
	if a.disable == "" {
		return a, errUsage
	}
	return a, nil
}

func users(ctx context.Context, c usersBackend, args []string, in io.Reader, out io.Writer) error {
	a, err := parseUsers(args)
	if err != nil {
		return err
	}
	res, err := c.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("listing users: %w", err)
	}
	me := c.Me()
	if a.disable == "" {
		printUsers(out, res, me.Admin)
		return nil
	}
	if !me.Admin {
		return errors.New("only admins can disable users")
	}
	i := slices.IndexFunc(res.Users, func(u proto.User) bool { return u.Name == a.disable })
	if i < 0 {
		return fmt.Errorf("no user @%s (see: tc users)", sanitize.Line(a.disable))
	}
	u := res.Users[i]
	switch {
	case u.ID == me.ID:
		return errors.New("you can't disable yourself")
	case u.Disabled:
		fmt.Fprintf(out, "@%s is already disabled.\n", sanitize.Line(u.Name))
		return nil
	}
	if !a.yes {
		fmt.Fprintf(out, "Disable @%s? They are signed out at once, none of their SSH keys can sign in again, and their "+
			"invites and webhooks are deleted. This can't be undone. [y/N] ", sanitize.Line(u.Name))
		line, _ := bufio.NewReader(in).ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			fmt.Fprintln(out, "not disabled")
			return nil
		}
	}
	if err := c.DisableUser(ctx, u.ID); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ disabled @%s\n", sanitize.Line(u.Name))
	var invited []string
	for _, v := range res.Users {
		if by, ok := res.InvitedBy[v.ID]; ok && by == u.ID && !v.Disabled {
			invited = append(invited, "@"+sanitize.Line(v.Name))
		}
	}
	if len(invited) > 0 {
		fmt.Fprintf(out, "They invited %s: %s. Disable any you don't trust with: tc users disable @name\n",
			plural(len(invited), "account"), strings.Join(invited, ", "))
	}
	return nil
}

// printUsers is the tc users table; admins also see who invited whom.
func printUsers(out io.Writer, res proto.UsersRes, admin bool) {
	names := map[uint64]string{}
	for _, u := range res.Users {
		names[u.ID] = u.Name
	}
	name := func(id uint64) string { return names[id] }
	fmt.Fprintf(out, "%-24s  %-8s  %-10s", "NAME", "STATE", "JOINED")
	if admin {
		fmt.Fprint(out, "  INVITED BY")
	}
	fmt.Fprintln(out)
	for _, u := range res.Users {
		state := "member"
		switch {
		case u.Disabled:
			state = "disabled"
		case u.Bot:
			state = "bot"
		case u.Admin:
			state = "admin"
		}
		fmt.Fprintf(out, "%-24s  %-8s  %-10s", "@"+sanitize.Line(u.Name), state, time.UnixMilli(u.Created).Format("2006-01-02"))
		if admin {
			by, ok := res.InvitedBy[u.ID]
			switch {
			case !ok:
				fmt.Fprint(out, "  -")
			case by == 0:
				fmt.Fprint(out, "  server host")
			default:
				fmt.Fprint(out, "  "+who(by, name))
			}
		}
		fmt.Fprintln(out)
	}
}
