package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/skailama/tc/internal/iconfont"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/tui"
)

// exitCode ends tc with that status and no message (scripts read it).
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// cmdFonts: tc fonts install|status|test|remove. "refresh" (for tc
// update) replaces an out-of-date font silently; "status --quiet" only
// sets the exit status (0 installed, 1 not), for install.sh.
func cmdFonts(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "install":
		p, err := iconfont.Install()
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Installed tc icons: %s\n", p)
		fmt.Fprintln(out, "A running terminal may need a restart to show them.")
		if overSSH() {
			fmt.Fprintln(out, "You're over SSH: fonts are drawn where your terminal runs, so run tc fonts install on your own computer too.")
		}
		return nil
	case "status":
		s, p, err := iconfont.Status()
		if len(args) > 1 && args[1] == "--quiet" {
			if err != nil || s == iconfont.Missing {
				return exitCode(1)
			}
			return nil
		}
		if err != nil {
			return err
		}
		switch s {
		case iconfont.Current:
			fmt.Fprintf(out, "tc icons: installed and up to date (%s)\n", p)
		case iconfont.Stale:
			fmt.Fprintf(out, "tc icons: installed, out of date (%s); tc fonts install refreshes them\n", p)
		default:
			fmt.Fprintln(out, "tc icons: not installed; tc fonts install adds them")
		}
		return nil
	case "test":
		fontsTest(out)
		return nil
	case "remove":
		p, existed, err := iconfont.Remove()
		if err != nil {
			return err
		}
		if !existed {
			fmt.Fprintln(out, "tc icons aren't installed.")
			return nil
		}
		fmt.Fprintf(out, "Removed tc icons (%s). tc uses its Unicode icons from its next start.\n", p)
		return nil
	case "refresh":
		did, err := iconfont.Refresh()
		if err != nil {
			return err
		}
		if did {
			fmt.Fprintln(out, "Refreshed tc icons to this version.")
		}
		return nil
	}
	return errUsage
}

// fontsTest prints every icon from tc Symbols next to its name and its
// fallbacks, so you can see whether this terminal draws them.
func fontsTest(out io.Writer) {
	s, _, _ := iconfont.Status()
	fmt.Fprintf(out, "tc icons: %s. Each row: the tc Symbols icon, its name, then the Unicode and DOS fallbacks.\n", s)
	fmt.Fprintln(out, "If the first column shows boxes, question marks or blanks, run tc fonts install and restart your terminal.")
	fmt.Fprintln(out)
	for _, r := range tui.IconRows() {
		fmt.Fprintf(out, "  %s   %-10s %s   %s\n", r.Symbol, r.Name, r.Unicode, r.DOS)
	}
}

func overSSH() bool { return os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CONNECTION") != "" }

// refreshFonts runs the installed tc's `fonts refresh` (the new binary
// carries the new font) and returns what it says; "" when nothing changed.
func refreshFonts(ctx context.Context, exe string) string {
	b, err := exec.CommandContext(ctx, exe, "fonts", "refresh").CombinedOutput()
	msg := strings.TrimSpace(sanitize.Line(string(b)))
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return ""
		}
		return "Couldn't refresh tc icons; run tc fonts install."
	}
	return msg
}
