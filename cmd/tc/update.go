package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/skailama/tc/internal/release"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/voice"
)

// cmdUpdate: tc update [--check] [--voice].
func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only report whether a newer release exists")
	toVoice := fs.Bool("voice", false, "switch to the voice build (voice huddles)")
	fs.Parse(args)
	if fs.NArg() > 0 {
		return errUsage
	}
	exe, err := release.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	ctx, cancelT := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelT()
	src := release.NewSource()
	src.Voice = voice.Available()
	req := updateReq{check: *check, toVoice: *toVoice && !voice.Available(), runs: runsHere, restart: restartBackground, fonts: refreshFonts}
	return runUpdate(ctx, os.Stdout, src, exe, version, runtime.GOOS, runtime.GOARCH, req)
}

// updateReq is what tc update was asked to do.
type updateReq struct {
	check bool
	// toVoice switches a plain build to the voice build, even when it's
	// already the latest release.
	toVoice bool
	// runs runs a downloaded binary's `tc version` on this system and
	// returns its output; nil skips the check.
	runs func(ctx context.Context, dir string, bin []byte) (string, error)
	// restart restarts tc background (if installed) once the new binary is
	// in place and says what it did; nil: nothing to restart.
	restart func(ctx context.Context) string
	// fonts refreshes an installed icon font the binary at exe carries a
	// newer build of, and says so; nil: leave fonts alone.
	fonts func(ctx context.Context, exe string) string
}

func runUpdate(ctx context.Context, out io.Writer, src *release.Source, exe, current, goos, goarch string, req updateReq) error {
	if req.toVoice && release.IsHomebrew(exe) {
		return errors.New("tc was installed with Homebrew, which has no voice build. For voice: brew uninstall tc, then\n  curl -fsSL https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/main/install.sh | sh -s -- --voice")
	}
	latest, err := src.Latest(ctx)
	if err != nil {
		return err
	}
	newer := release.Newer(latest, current)
	if release.IsHomebrew(exe) {
		if newer {
			fmt.Fprintf(out, "tc %s is available (you have %s). tc was installed with Homebrew: run `brew upgrade tc`.\n", latest, current)
		} else {
			fmt.Fprintf(out, "tc %s is up to date (installed with Homebrew; upgrade with `brew upgrade tc`).\n", current)
		}
		return nil
	}
	if req.toVoice && release.IsRelease(current) && release.Newer(current, latest) {
		// Switching flavor is not a way to downgrade.
		return fmt.Errorf("the latest release (%s) is older than this tc (%s); not switching", latest, current)
	}
	if req.toVoice && !release.Supported(goos, goarch) {
		return fmt.Errorf("no voice build for %s/%s", goos, goarch)
	}
	if req.check {
		switch {
		case req.toVoice:
			fmt.Fprintf(out, "The voice build of tc %s is available. Run: tc update --voice\n", latest)
		case newer:
			fmt.Fprintf(out, "Update available: %s → %s. Run: tc update\n", current, latest)
		case !release.IsRelease(current):
			fmt.Fprintf(out, "This is a %s build; the latest release is %s.\n", current, latest)
		default:
			fmt.Fprintf(out, "tc %s is up to date.\n", current)
		}
		return nil
	}
	if req.toVoice {
		src.Voice = true
	} else if !newer && release.IsRelease(current) {
		fmt.Fprintf(out, "tc %s is up to date.\n", current)
		req.sayFonts(ctx, out, exe)
		return nil
	}
	if !release.Supported(goos, goarch) {
		return fmt.Errorf("no release builds for %s/%s", goos, goarch)
	}
	if err := writable(exe); err != nil {
		return err
	}
	flavor := ""
	if src.Voice {
		flavor = " (voice build)"
	}
	fmt.Fprintf(out, "Downloading tc %s%s for %s/%s…\n", latest, flavor, goos, goarch)
	bin, err := src.Fetch(ctx, latest, goos, goarch)
	if err != nil {
		return err
	}
	// The voice build links libopus and, on Linux, needs glibc 2.35+: one
	// that can't start here must not replace a working tc (install.sh does
	// the same).
	if req.toVoice && req.runs != nil {
		got, err := req.runs(ctx, filepath.Dir(exe), bin)
		if err != nil {
			hint := ""
			if goos == "linux" {
				hint = " (it needs glibc 2.35 or newer)"
			}
			return fmt.Errorf("the voice build doesn't run on this system%s: %v; kept the plain build", hint, err)
		}
		// Like CI's smoke test: what we got must say it's this release's
		// voice build, not just start.
		if !strings.Contains(got, latest+" (voice)") {
			return fmt.Errorf("the downloaded voice build says %q, not %q; kept the plain build", sanitize.Line(strings.TrimSpace(got)), "tc "+latest+" (voice)")
		}
	}
	if err := release.Replace(exe, bin); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %s: %s → %s%s. Restart tc to use it.\n", exe, current, latest, flavor)
	if req.restart != nil {
		if msg := req.restart(ctx); msg != "" {
			fmt.Fprintln(out, msg)
		}
	}
	req.sayFonts(ctx, out, exe)
	return nil
}

// sayFonts refreshes the icon font if that's needed, in one line at most.
func (req updateReq) sayFonts(ctx context.Context, out io.Writer, exe string) {
	if req.fonts == nil {
		return
	}
	if msg := req.fonts(ctx, exe); msg != "" {
		fmt.Fprintln(out, msg)
	}
}

// runsHere runs `tc version` from bin and returns its output. bin goes in a
// fresh 0700 directory next to the installed tc: the same mount options
// apply, and in a shared install directory nobody else can swap the file
// between writing and running it.
func runsHere(ctx context.Context, dir string, bin []byte) (string, error) {
	pd, err := os.MkdirTemp(dir, ".tc-update-probe-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(pd)
	name := filepath.Join(pd, "tc")
	if err := os.WriteFile(name, bin, 0o700); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, "version")
	cmd.WaitDelay = 2 * time.Second
	b, err := cmd.CombinedOutput()
	msg := strings.TrimSpace(sanitize.Line(string(b)))
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "…"
	}
	if err != nil {
		if msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return msg, nil
}

// writable fails early (before downloading) when exe's directory can't take
// a new file, e.g. a root-owned /usr/local/bin.
func writable(exe string) error {
	f, err := os.CreateTemp(filepath.Dir(exe), ".tc-update-probe-*")
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return fmt.Errorf("can't update %s: its directory isn't writable (%v); reinstall with install.sh, or run tc update as the user that installed it", exe, err)
	}
	f.Close()
	os.Remove(f.Name())
	return nil
}
