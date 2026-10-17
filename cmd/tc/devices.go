package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// devicesBackend is what tc devices needs from *client.Client.
type devicesBackend interface {
	Me() proto.User
	Users() []proto.User
	RefreshDevices(ctx context.Context, user uint64) error
	Devices(user uint64) []client.DeviceInfo
	FindDevice(user uint64, prefix string) (client.DeviceInfo, error)
	RevokeDevice(ctx context.Context, dev e2ecrypto.DevID, allowSelf bool) (int, error)
}

// tc devices [list] [@user] | tc devices revoke <id> [--yes] [--self]
func cmdDevices(args []string) error {
	if _, err := parseDevices(args); err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	c, err := connected(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return devices(ctx, c, args, os.Stdin, os.Stdout)
}

type devicesArgs struct {
	revoke    string // device id prefix; "" lists
	who       string // list: @user without the @; "" = me
	yes, self bool
}

func parseDevices(args []string) (devicesArgs, error) {
	var a devicesArgs
	if len(args) > 0 && (args[0] == "list" || args[0] == "ls") {
		args = args[1:]
	}
	if len(args) > 0 && args[0] == "revoke" {
		for _, x := range args[1:] {
			switch {
			case x == "--yes" || x == "-y":
				a.yes = true
			case x == "--self":
				a.self = true
			case !strings.HasPrefix(x, "-") && a.revoke == "":
				a.revoke = x
			default:
				return a, errUsage
			}
		}
		if a.revoke == "" {
			return a, errUsage
		}
		return a, nil
	}
	for _, x := range args {
		if !strings.HasPrefix(x, "@") || len(x) < 2 || a.who != "" {
			return a, errUsage
		}
		a.who = x[1:]
	}
	return a, nil
}

func devices(ctx context.Context, c devicesBackend, args []string, in io.Reader, out io.Writer) error {
	a, err := parseDevices(args)
	if err != nil {
		return err
	}
	me := c.Me()
	user, name := me.ID, me.Name
	if a.who != "" {
		user = 0
		for _, u := range c.Users() {
			if u.Name == a.who {
				user, name = u.ID, u.Name
			}
		}
		if user == 0 {
			return fmt.Errorf("no user @%s", a.who)
		}
	}
	if err := c.RefreshDevices(ctx, user); err != nil {
		return fmt.Errorf("loading devices: %w", err)
	}
	if a.revoke == "" {
		printDevices(out, c.Devices(user), name, user == me.ID)
		return nil
	}

	d, err := c.FindDevice(me.ID, a.revoke)
	if errors.Is(err, client.ErrNoDevice) {
		return fmt.Errorf("you have no device %q (see: tc devices)", a.revoke)
	}
	if err != nil {
		return err
	}
	switch {
	case d.Revoked:
		return fmt.Errorf("device %s is already revoked", d.Short())
	case !d.Verified:
		return fmt.Errorf("device %s isn't signed by a key this device trusts for you, so it can't be revoked from here", d.Short())
	case d.This && !a.self:
		return fmt.Errorf("device %s is this device: revoke it from another device, or pass --self", d.Short())
	}
	what := fmt.Sprintf("device %s (added %s)", d.Short(), d.Created.Format("2006-01-02 15:04"))
	if d.This {
		what = "THIS device " + d.Short() + " (it re-registers with new keys on its next connection)"
	}
	if !a.yes {
		fmt.Fprintf(out, "Revoke %s? It stops receiving keys for new encrypted messages; this can't be undone. [y/N] ", what)
		line, _ := bufio.NewReader(in).ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			fmt.Fprintln(out, "not revoked")
			return nil
		}
	}
	rotated, err := c.RevokeDevice(ctx, d.ID, a.self)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ revoked device %s", d.Short())
	if rotated > 0 {
		fmt.Fprintf(out, "; new keys in %s", plural(rotated, "encrypted channel"))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Note: this doesn't remove an SSH key. If that device held your SSH private key, it can still sign in and\n"+
		"register a new device, and tc can't remove one key yet: ask an admin to disable your account\n"+
		"(tc users disable @"+sanitize.Line(c.Me().Name)+") and to invite you again as a new account with a new SSH key.")
	return nil
}

func printDevices(out io.Writer, ds []client.DeviceInfo, name string, mine bool) {
	who := "@" + sanitize.Line(name)
	if mine {
		who += " (you)"
	}
	if len(ds) == 0 {
		fmt.Fprintf(out, "%s has no devices registered for encryption.\n", who)
		return
	}
	fmt.Fprintf(out, "Devices of %s:\n", who)
	fmt.Fprintf(out, "  %-8s  %-16s  %-20s  %s\n", "ID", "ADDED", "SSH KEY", "STATE")
	for _, d := range ds {
		mark := " "
		if d.This {
			mark = "*"
		}
		state := "active"
		switch {
		case d.This && !d.Revoked:
			state = "this device"
		case d.Revoked && !d.RevokedAt.IsZero():
			state = "revoked " + d.RevokedAt.Format("2006-01-02 15:04")
		case d.Revoked:
			state = "revoked"
		}
		if d.Former {
			state = "old id of this device, " + state
		}
		if !d.Verified {
			state += " (unverified: the server's claim)"
		}
		fp := sanitize.Line(d.KeyFP)
		if len(fp) > 20 {
			fp = fp[:19] + "…"
		}
		fmt.Fprintf(out, "%s %-8s  %-16s  %-20s  %s\n", mark, d.Short(), d.Created.Format("2006-01-02 15:04"), fp, state)
	}
	if mine {
		fmt.Fprintln(out, "Lost one? tc devices revoke <id>")
	}
}
