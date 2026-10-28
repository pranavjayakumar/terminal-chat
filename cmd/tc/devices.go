package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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
	RevokeDeviceOpts(ctx context.Context, dev e2ecrypto.DevID, o client.RevokeOpts) (int, error)
	RegistrationsLeft() int
	DeviceError() error
}

// devicesTimeout bounds each network step of tc devices, so nothing waits
// for ever (the confirmation prompt isn't bounded).
const devicesTimeout = 60 * time.Second

// tc devices [list] [@user] | tc devices revoke <id> [--yes] [--self [--force]]
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
	revoke           string // device id prefix; "" lists
	endorse          string // SSH key fingerprint to vouch for
	who              string // list: @user without the @; "" = me
	yes, self, force bool
}

func parseDevices(args []string) (devicesArgs, error) {
	var a devicesArgs
	if len(args) > 0 && (args[0] == "list" || args[0] == "ls") {
		args = args[1:]
	}
	if len(args) > 0 && args[0] == "endorse" {
		if len(args) != 2 || !strings.HasPrefix(args[1], "SHA256:") {
			return a, errors.New("usage: tc devices endorse SHA256:… (the fingerprint tc devices shows on your other computer)")
		}
		a.endorse = args[1]
		return a, nil
	}
	if len(args) > 0 && args[0] == "revoke" {
		for _, x := range args[1:] {
			switch {
			case x == "--yes" || x == "-y":
				a.yes = true
			case x == "--self":
				a.self = true
			case x == "--force":
				a.force = true
			case !strings.HasPrefix(x, "-") && a.revoke == "":
				a.revoke = x
			default:
				return a, errUsage
			}
		}
		if a.revoke == "" || (a.force && !a.self) {
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
	if a.endorse != "" {
		e, ok := c.(endorseBackend)
		if !ok {
			return errors.New("endorsing keys isn't supported here")
		}
		return endorseKey(ctx, e, a.endorse, out)
	}
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
	rctx, cancel := context.WithTimeout(ctx, devicesTimeout)
	err = c.RefreshDevices(rctx, user)
	cancel()
	if err != nil {
		return fmt.Errorf("loading devices: %w", err)
	}
	if a.revoke == "" {
		printDevices(out, c.Devices(user), name, user == me.ID)
		if user == me.ID {
			if e, ok := c.(endorseBackend); ok {
				printOwnKey(out, e.OwnKeyFingerprint(), c.Devices(user))
			}
			printDeviceErr(out, c.DeviceError())
		}
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
		left := c.RegistrationsLeft()
		if left == 0 && !a.force {
			return errLastRegistration
		}
		what = "THIS device " + d.Short() + " (it registers again as a new device right away"
		switch {
		case left == 0:
			what = "THIS device " + d.Short() + " (your account has no device registrations left: it can never register again and loses encryption for good)"
		case left <= 5:
			what += fmt.Sprintf("; your account may register only %d more devices, ever)", left)
		default:
			what += ")"
		}
	}
	if !a.yes {
		fmt.Fprintf(out, "Revoke %s? It stops receiving keys for new encrypted messages; this can't be undone. [y/N] ", what)
		line, _ := bufio.NewReader(in).ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			fmt.Fprintln(out, "not revoked")
			return nil
		}
	}
	rctx, cancel = context.WithTimeout(ctx, devicesTimeout)
	defer cancel()
	rotated, err := c.RevokeDeviceOpts(rctx, d.ID, client.RevokeOpts{Self: a.self, Force: a.force})
	if errors.Is(err, client.ErrLastRegistration) {
		return errLastRegistration
	}
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
	if !d.This {
		printDeviceErr(out, c.DeviceError())
	}
	return nil
}

// endorseBackend is what tc devices endorse needs (*client.Client).
type endorseBackend interface {
	OwnKeyFingerprint() string
	EndorseKey(ctx context.Context, fp string) (client.EndorseResult, error)
}

var _ endorseBackend = (*client.Client)(nil)

// endorseKey makes this computer's SSH key vouch for another key of the
// user's (#146), then says what to run on the other computer so it trusts
// this one too.
func endorseKey(ctx context.Context, c endorseBackend, fp string, out io.Writer) error {
	rctx, cancel := context.WithTimeout(ctx, devicesTimeout)
	defer cancel()
	r, err := c.EndorseKey(rctx, fp)
	if errors.Is(err, client.ErrKeyTrusted) {
		fmt.Fprintf(out, "%s is already trusted on this computer.\n", sanitize.Line(fp))
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ this computer's SSH key %s now vouches for %s.\n", sanitize.Line(r.By), sanitize.Line(r.Key))
	fmt.Fprintln(out, "Your devices and everyone you chat with trust that key's devices from now on.")
	fmt.Fprintf(out, "On the computer with that key, run this so it trusts this one too:\n\n  tc devices endorse %s\n\n", sanitize.Line(r.By))
	return nil
}

// printOwnKey shows this computer's SSH key, which the user's other
// computers need for tc devices endorse, and says what to do about devices
// under a key of theirs this computer doesn't trust.
func printOwnKey(out io.Writer, fp string, ds []client.DeviceInfo) {
	if fp != "" {
		fmt.Fprintf(out, "This computer's SSH key: %s\n", sanitize.Line(fp))
	}
	for _, d := range ds {
		if !d.Verified && !d.Revoked {
			fmt.Fprintln(out, "Devices marked unverified use an SSH key this computer doesn't trust. If it is yours (an add-key invite),\n"+
				"run tc devices on that computer and here: tc devices endorse <the SSH key it shows>. Never endorse a key you don't recognize.")
			return
		}
	}
}

var errLastRegistration = fmt.Errorf("your account has no device registrations left (%d per account, ever): revoked, this device could never register again and would lose encryption for good. Add --force to revoke it anyway", proto.MaxDeviceRegistrations)

// printDeviceErr says why this device isn't registered for encryption.
func printDeviceErr(out io.Writer, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(out, "⚠ %s\n", sanitize.Line(err.Error()))
	if errors.Is(err, client.ErrTooManyDevices) {
		fmt.Fprintln(out, "  Until then this device can't read or send encrypted messages.")
	}
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
