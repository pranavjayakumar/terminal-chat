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
	req := updateReq{check: *check, toVoice: *toVoice && !voice.Available(), runs: runsHere}
	return runUpdate(ctx, os.Stdout, src, exe, version, runtime.GOOS, runtime.GOARCH, req)
}

// updateReq is what tc update was asked to do.
type updateReq struct {
	check bool
	// toVoice switches a plain build to the voice build, even when it's
	// already the latest release.
	toVoice bool
	// runs checks that a downloaded binary starts on this system; nil skips
	// the check.
	runs func(ctx context.Context, dir string, bin []byte) error
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
		if err := req.runs(ctx, filepath.Dir(exe), bin); err != nil {
			hint := ""
			if goos == "linux" {
				hint = " (it needs glibc 2.35 or newer)"
			}
			return fmt.Errorf("the voice build doesn't run on this system%s: %v; kept the plain build", hint, err)
		}
	}
	if err := release.Replace(exe, bin); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %s: %s → %s%s. Restart tc to use it.\n", exe, current, latest, flavor)
	return nil
}

// runsHere runs `tc version` from bin, written to a private temp file in dir
// (next to the installed tc, so the same mount options apply).
func runsHere(ctx context.Context, dir string, bin []byte) error {
	f, err := os.CreateTemp(dir, ".tc-update-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	_, werr := f.Write(bin)
	if err := f.Chmod(0o700); werr == nil {
		werr = err
	}
	if err := f.Close(); werr == nil {
		werr = err
	}
	if werr != nil {
		return werr
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, name, "version").CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(sanitize.Line(string(b))); msg != "" {
			if r := []rune(msg); len(r) > 200 {
				msg = string(r[:200]) + "…"
			}
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
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
