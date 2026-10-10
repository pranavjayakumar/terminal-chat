package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/skailama/tc/internal/release"
)

// cmdUpdate: tc update [--check].
func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only report whether a newer release exists")
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
	return runUpdate(ctx, os.Stdout, release.NewSource(), exe, version, runtime.GOOS, runtime.GOARCH, *check)
}

func runUpdate(ctx context.Context, out io.Writer, src *release.Source, exe, current, goos, goarch string, checkOnly bool) error {
	latest, err := src.Latest(ctx)
	if err != nil {
		return err
	}
	newer := release.Newer(latest, current)
	if checkOnly {
		switch {
		case newer:
			fmt.Fprintf(out, "Update available: %s → %s. Run: tc update\n", current, latest)
		case !release.IsRelease(current):
			fmt.Fprintf(out, "This is a %s build; the latest release is %s.\n", current, latest)
		default:
			fmt.Fprintf(out, "tc %s is up to date.\n", current)
		}
		return nil
	}
	if !newer && release.IsRelease(current) {
		fmt.Fprintf(out, "tc %s is up to date.\n", current)
		return nil
	}
	if !release.Supported(goos, goarch) {
		return fmt.Errorf("no release builds for %s/%s", goos, goarch)
	}
	if err := writable(exe); err != nil {
		return err
	}
	fmt.Fprintf(out, "Downloading tc %s for %s/%s…\n", latest, goos, goarch)
	bin, err := src.Fetch(ctx, latest, goos, goarch)
	if err != nil {
		return err
	}
	if err := release.Replace(exe, bin); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %s: %s → %s. Restart tc to use it.\n", exe, current, latest)
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
