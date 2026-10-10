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

// testGitHubBase replaces https://github.com in tests.
var testGitHubBase string

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
	// Keys (theirs and our own) are pinned in the background right after
	// connecting.
	sn, err := pollSafety(ctx, func() (client.SafetyNumber, error) { return c.SafetyNumber(user) },
		10*time.Second, 100*time.Millisecond)
	if err != nil {
		return err
	}
	in2 := bufio.NewReader(in)
	ask := func(q string) bool {
		fmt.Fprint(out, q)
		line, _ := in2.ReadString('\n')
		a := strings.ToLower(strings.TrimSpace(line))
		return a == "y" || a == "yes"
	}

	if login != "" {
		if testGitHubBase != "" {
			c.SetGitHubBase(testGitHubBase)
		}
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
		fmt.Fprintf(out, "✓ every pinned key of %s is on %s\n", at, gh)
		if !ask(fmt.Sprintf("Verify %s because you confirmed %s is theirs? [y/N] ", at, gh)) {
			fmt.Fprintln(out, "not verified")
			return nil
		}
		if err := c.ConfirmGitHub(res); err != nil {
			return err
		}
		fmt.Fprintf(out, "✓ %s verified\n", at)
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
	fmt.Fprintf(out, "\nCompare it with %s over another channel (in person or a call).\n", at)
	fmt.Fprintf(out, "If the numbers differ, compare the key lists above too: a mismatch can also mean your devices pinned different key sets.\n")
	if !ask("Do all 60 digits match? [y/N] ") {
		fmt.Fprintln(out, "not verified")
		return nil
	}
	if err := c.MarkVerified(user, sn.Digits); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ %s verified\n", at)
	return nil
}

// pollSafety calls get until it stops failing because a key isn't pinned
// yet (theirs: ErrNoKeys, ours: ErrOwnKeyNotPinned), for at most timeout.
func pollSafety(ctx context.Context, get func() (client.SafetyNumber, error), timeout, every time.Duration) (client.SafetyNumber, error) {
	deadline := time.Now().Add(timeout)
	for {
		sn, err := get()
		if (!errors.Is(err, client.ErrNoKeys) && !errors.Is(err, client.ErrOwnKeyNotPinned)) || time.Now().After(deadline) {
			return sn, err
		}
		select {
		case <-ctx.Done():
			return sn, err
		case <-time.After(every):
		}
	}
}
