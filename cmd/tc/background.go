package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skailama/tc/internal/alert"
	"github.com/skailama/tc/internal/background"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/sanitize"
)

// cmdBackground: tc background on|off|status (and the hidden run).
func cmdBackground(args []string) error {
	if len(args) != 1 {
		return errUsage
	}
	home := client.DefaultHome()
	ctx, cancel := signalContext()
	defer cancel()
	switch args[0] {
	case "run":
		return backgroundRun(ctx, home)
	case "on", "off", "status":
	default:
		return errUsage
	}
	m, err := background.Local(home)
	if err != nil {
		return err
	}
	switch args[0] {
	case "on":
		return backgroundOn(ctx, os.Stdout, m, home)
	case "off":
		return backgroundOff(ctx, os.Stdout, m, home)
	}
	backgroundStatus(os.Stdout, m, home, time.Now())
	return nil
}

func backgroundOn(ctx context.Context, out io.Writer, m background.Manager, home string) error {
	if _, err := os.Stat(filepath.Join(home, "config.json")); err != nil {
		return errors.New("not logged in here yet: run tc join <invite> (or tc login) first, then tc background on")
	}
	if _, err := background.OSSender(); err != nil {
		return err
	}
	if err := m.Install(ctx); err != nil {
		return err
	}
	running := waitFor(5*time.Second, func() bool { return background.Running(home) })
	fmt.Fprintln(out, "tc background is on: mentions, DMs and huddle rings pop up while tc is closed")
	fmt.Fprintln(out, "(it stays quiet while tc is open, and starts again when you log in).")
	if !running {
		fmt.Fprintln(out, "It isn't running yet; check with: tc background status")
	}
	fmt.Fprintln(out, "Turn it off with: tc background off")
	return nil
}

func backgroundOff(ctx context.Context, out io.Writer, m background.Manager, home string) error {
	was := m.Installed()
	if err := m.Uninstall(ctx); err != nil {
		return err
	}
	stopped := waitFor(5*time.Second, func() bool { return !background.Running(home) })
	for _, f := range []string{background.StatusFile, background.RunLockFile, background.LogFile, background.LogFile + ".1", "background.err.log"} {
		_ = os.Remove(filepath.Join(home, f))
	}
	switch {
	case !stopped:
		fmt.Fprintln(out, "Removed the autostart entry, but a tc background run started by hand is still running; stop it yourself.")
	case was:
		fmt.Fprintln(out, "tc background is off: no notifications while tc is closed.")
	default:
		fmt.Fprintln(out, "tc background was already off.")
	}
	return nil
}

func backgroundStatus(out io.Writer, m background.Manager, home string, now time.Time) {
	installed, running := m.Installed(), background.Running(home)
	if installed {
		fmt.Fprintf(out, "tc background: on (%s)\n", sanitize.Line(m.S.File))
	} else {
		fmt.Fprintln(out, "tc background: off (turn on: tc background on)")
	}
	st, err := background.ReadStatus(home)
	if !running {
		fmt.Fprintln(out, "running: no")
		if installed && err == nil && st.Err != "" {
			fmt.Fprintf(out, "last error: %s (%s)\n", sanitize.Line(st.Err), ago(now, st.ErrAt))
		}
		return
	}
	if err != nil {
		fmt.Fprintln(out, "running: yes")
		return
	}
	fmt.Fprintf(out, "running: yes (pid %d, %s, started %s)\n", st.PID, sanitize.Line(st.Version), ago(now, st.Started))
	conn := sanitize.Line(st.State)
	if st.Transport != "" {
		conn += " via " + sanitize.Line(st.Transport)
	}
	fmt.Fprintf(out, "connection: %s (%s)\n", conn, ago(now, st.Since))
	if st.State == "online" && !st.Passive {
		fmt.Fprintln(out, "note: this server predates passive sessions, so you show as online while tc background runs")
	}
	if st.Err != "" {
		fmt.Fprintf(out, "last error: %s (%s)\n", sanitize.Line(st.Err), ago(now, st.ErrAt))
	}
	n := fmt.Sprintf("notifications shown: %d", st.Notified)
	if st.LastNote > 0 {
		n += " (last " + ago(now, st.LastNote) + ")"
	}
	fmt.Fprintln(out, n)
	if background.UIOpen(home) {
		fmt.Fprintln(out, "tc is open, so tc background is staying quiet")
	}
	fmt.Fprintf(out, "log: %s\n", sanitize.Line(filepath.Join(home, background.LogFile)))
}

// ago is "12s ago", "5m ago", "3h ago", or a date.
func ago(now time.Time, ms int64) string {
	if ms <= 0 {
		return "never"
	}
	d := now.Sub(time.UnixMilli(ms))
	switch {
	case d < 0:
		return time.UnixMilli(ms).Local().Format("Jan 2 15:04")
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return time.UnixMilli(ms).Local().Format("Jan 2 15:04")
}

func waitFor(d time.Duration, cond func() bool) bool {
	end := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// backgroundRun is the long-running process the service manager starts.
func backgroundRun(ctx context.Context, home string) error {
	logw, err := background.OpenLog(home)
	if err != nil {
		return err
	}
	defer logw.Close()
	log := slog.New(slog.NewTextHandler(logw, nil))
	exe := os.Args[0]
	if !filepath.IsAbs(exe) {
		exe, _ = os.Executable()
	}
	err = background.Run(ctx, background.Config{Home: home, Version: version, Env: alert.OSEnv(), Log: log, Exe: exe})
	if errors.Is(err, background.ErrBinaryChanged) {
		return nil // the service manager starts the new binary
	}
	if err != nil {
		log.Error("exiting", "err", err)
	}
	return err
}

// restartBackground restarts an installed background notifier so it runs
// the binary tc update just installed. "" when there's nothing to say.
func restartBackground(ctx context.Context) string {
	home := client.DefaultHome()
	m, err := background.Local(home)
	if err != nil || !m.Installed() {
		return ""
	}
	if err := m.Restart(ctx); err != nil {
		return "Couldn't restart tc background (" + strings.TrimSpace(sanitize.Line(err.Error())) + "); run tc background on to restart it."
	}
	return "Restarted tc background."
}
