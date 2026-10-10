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
	"github.com/skailama/tc/internal/sanitize"
)

// tc verify @user [--github login]: the CLI side of /verify.
func cmdVerify(args []string) error {
	ctx, cancel := signalContext()
	defer cancel()
	return verify(ctx, args, os.Stdin, os.Stdout)
}

func verify(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	var who, login string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--github" && i+1 < len(args) && login == "":
			login = args[i+1]
			i++
		case strings.HasPrefix(a, "@") && who == "":
			who = a[1:]
		default:
			return errUsage
		}
	}
	if who == "" {
		return errUsage
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c, err := connected(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	var user uint64
	for _, u := range c.Users() {
		if u.Name == who {
			user = u.ID
		}
	}
	if user == 0 {
		return fmt.Errorf("no user @%s", who)
	}
	at := "@" + sanitize.Line(who)
	// Keys are verified in the background right after connecting.
	var sn client.SafetyNumber
	for deadline := time.Now().Add(10 * time.Second); ; {
		sn, err = c.SafetyNumber(user)
		if !errors.Is(err, client.ErrNoKeys) || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return err
	}

	if login != "" {
		res, err := c.VerifyGitHub(ctx, user, login)
		if err != nil {
			return err
		}
		gh := "github.com/" + sanitize.Line(login)
		fmt.Fprintf(out, "Keys this device pinned for %s:\n", at)
		for _, fp := range res.Pinned {
			mark := "✓"
			for _, x := range res.Missing {
				if x == fp {
					mark = "✗"
				}
			}
			fmt.Fprintf(out, "  %s %s\n", mark, sanitize.Line(fp))
		}
		fmt.Fprintf(out, "Keys on %s:\n", gh)
		if len(res.GitHub) == 0 {
			fmt.Fprintln(out, "  (none)")
		}
		for _, fp := range res.GitHub {
			fmt.Fprintf(out, "  %s\n", sanitize.Line(fp))
		}
		if !res.Match {
			return fmt.Errorf("MISMATCH: %d of %s's pinned keys aren't on %s — nothing was verified", len(res.Missing), at, gh)
		}
		fmt.Fprintf(out, "✓ every pinned key of %s is on %s — %s is now verified\n", at, gh, at)
		return nil
	}

	fmt.Fprintf(out, "Safety number with %s:\n\n", at)
	gs := sn.Groups()
	for i := 0; i < len(gs); i += 4 {
		fmt.Fprintf(out, "    %s\n", strings.Join(gs[i:min(i+4, len(gs))], " "))
	}
	fmt.Fprintln(out)
	for _, p := range []client.SafetyParty{sn.Me, sn.Them} {
		label := "@" + p.Name
		if p.User == c.Me().ID {
			label = "you (" + label + ")"
		}
		fmt.Fprintf(out, "%s — SSH keys:\n", label)
		for _, fp := range p.Fingerprints {
			fmt.Fprintf(out, "  %s\n", sanitize.Line(fp))
		}
	}
	fmt.Fprintf(out, "\nCompare it with %s over another channel (in person or a call).\nDo all 60 digits match? [y/N] ", at)
	line, _ := bufio.NewReader(in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		fmt.Fprintln(out, "not verified")
		return nil
	}
	if err := c.MarkVerified(user, sn.Digits); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ %s verified\n", at)
	return nil
}
