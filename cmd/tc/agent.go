package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/skailama/tc/internal/agent"
	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/agent/memory"
	"github.com/skailama/tc/internal/agent/runner"
	"github.com/skailama/tc/internal/agent/work"
	"github.com/skailama/tc/internal/background"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// cmdAgent: tc agent setup|run|status|pause|resume|logs|hook.
func cmdAgent(args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	tcHome := client.DefaultHome()
	dir := home.Dir(tcHome)
	switch args[0] {
	case "setup":
		return agentSetup(args[1:], tcHome, os.Stdin, os.Stdout)
	case "run":
		ctx, cancel := signalContext()
		defer cancel()
		return agentRun(ctx, tcHome)
	case "status":
		return agentStatus(os.Stdout, tcHome, time.Now())
	case "pause", "resume":
		return agentPause(os.Stdout, dir, args[0] == "pause")
	case "logs":
		fs := flag.NewFlagSet("logs", flag.ExitOnError)
		n := fs.Int("n", 30, "how many entries")
		fs.Parse(args[1:])
		return agentLogs(os.Stdout, dir, *n)
	case "hook":
		return agentHook(args[1:], dir, os.Stdin, os.Stdout, os.Stderr)
	}
	return errUsage
}

// agentHook runs a Claude Code hook. It must never exit 2: that blocks
// the engine's turn.
func agentHook(args []string, dir string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	name := args[0]
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	d := fs.String("dir", dir, "agent home")
	if err := fs.Parse(args[1:]); err != nil {
		fmt.Fprintln(stderr, "tc agent hook:", err)
		return exitCode(1)
	}
	if err := memory.RunHook(name, stdin, stdout, *d); err != nil {
		fmt.Fprintln(stderr, "tc agent hook:", sanitize.Line(err.Error()))
		return exitCode(1)
	}
	return nil
}

func agentSetup(args []string, tcHome string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("agent setup", flag.ExitOnError)
	handle := fs.String("handle", "", "the agent's @handle")
	icon := fs.String("icon", "", "the agent's icon (an emoji or a tc symbol)")
	fs.Parse(args)
	ctx, cancel := signalContext()
	defer cancel()
	c, err := connected(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	dir := home.Dir(tcHome)
	in := bufio.NewReader(stdin)

	a, have := c.MyAgent()
	if !have {
		if all, err := c.FetchAgents(ctx); err == nil {
			for _, x := range all {
				if x.Owner == c.Me().ID && x.Retired == 0 {
					a, have = x, true
				}
			}
		}
	}
	if have {
		nh := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(*handle)), "@")
		if nh == a.Handle {
			nh = ""
		}
		ni := strings.TrimSpace(*icon)
		if ni == a.Icon {
			ni = ""
		}
		if nh != "" && !proto.ValidAgentHandle(nh) {
			return fmt.Errorf("@%s can't be a handle: use lowercase letters, digits and dashes", sanitize.Line(nh))
		}
		if nh != "" || ni != "" {
			old := a.Handle
			if a, err = c.UpdateAgent(ctx, nh, ni); err != nil {
				return fmt.Errorf("renaming @%s: %w", old, err)
			}
			fmt.Fprintf(out, "@%s is now @%s %s.\n", sanitize.Line(old), sanitize.Line(a.Handle), sanitize.Line(a.Icon))
		} else {
			fmt.Fprintf(out, "You already have an agent: @%s %s. Setting it up on this computer.\n", sanitize.Line(a.Handle), sanitize.Line(a.Icon))
		}
	} else {
		if *handle == "" {
			def := strings.ToLower(sanitize.Line(c.Me().Name)) + "-agent"
			if !proto.ValidAgentHandle(def) {
				def = "my-agent"
			}
			*handle = ask(in, out, "Name your agent (its @handle)", def)
		}
		*handle = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(*handle)), "@")
		if !proto.ValidAgentHandle(*handle) {
			return fmt.Errorf("@%s can't be a handle: use lowercase letters, digits and dashes", sanitize.Line(*handle))
		}
		if *icon == "" {
			*icon = ask(in, out, "Pick an icon (an emoji)", "🤖")
		}
		if len(*icon) > proto.MaxAgentIcon {
			return errors.New("that icon is too long")
		}
		if a, err = c.RegisterAgent(ctx, *handle, *icon); err != nil {
			return fmt.Errorf("registering @%s: %w", *handle, err)
		}
		fmt.Fprintf(out, "Registered @%s %s.\n", sanitize.Line(a.Handle), sanitize.Line(a.Icon))
	}

	if err := home.EnsureWith(dir, home.Options{ExtraDomains: serverHosts(c.Config().Addr)}); err != nil {
		return err
	}
	cfg, err := home.LoadConfig(dir)
	if err != nil {
		return err
	}
	mid, err := agent.MachineID(tcHome)
	if err != nil {
		return err
	}
	moved := cfg.HomeMachine != "" && cfg.HomeMachine != mid
	cfg.Handle, cfg.Icon, cfg.HomeMachine = a.Handle, a.Icon, mid
	if err := home.SaveConfig(dir, cfg); err != nil {
		return err
	}
	// An agent device already signed in here keeps its device and keys;
	// only a missing login or one under an old handle is redone.
	dev := home.PathsFor(dir).DeviceHome
	switch cur := deviceHandle(dev, c.Me().ID); {
	case cur == a.Handle:
	case cur != "":
		// Renamed: the device stays; its next login names the new handle.
		// A fresh login would wipe the cache under a running agent.
		if err := renameDevice(dev, a.Handle); err != nil {
			return err
		}
	default:
		if _, err := client.LoginAgent(ctx, tcHome, dev, a.Handle); err != nil {
			return fmt.Errorf("signing the agent in: %w", err)
		}
	}
	if _, err := c.OpenAgentDM(ctx); err != nil {
		fmt.Fprintf(out, "(couldn't open your DM with @%s yet: %s)\n", sanitize.Line(a.Handle), sanitize.Line(err.Error()))
	}
	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Fprintln(out, "Note: Claude Code (claude) isn't on your PATH. The agent runs on it with your Claude subscription:")
		fmt.Fprintln(out, "  install it from https://claude.com/claude-code, then run claude once to sign in.")
	}
	if moved {
		fmt.Fprintln(out, "This computer is now the agent's home: it stops answering from the old one.")
	}
	fmt.Fprintf(out, "@%s lives on this computer and wakes when someone @mentions it or you DM it.\n", sanitize.Line(a.Handle))
	m, err := background.Local(tcHome)
	if err == nil && m.Installed() {
		if err := m.Restart(ctx); err != nil {
			fmt.Fprintln(out, "Restart tc background to start it: tc background on")
		} else {
			fmt.Fprintln(out, "tc background runs it from now on. Say hi in your agent DM.")
		}
	} else {
		fmt.Fprintln(out, "Turn on tc background on this computer: tc background on")
		fmt.Fprintln(out, "It runs the agent, and lets it see channels where someone @mentions it while tc is closed.")
	}
	return nil
}

// sandboxMissing explains why Work and Crew asks will fail here: the
// agent's settings require Claude Code's OS sandbox, which needs
// bubblewrap on Linux.
func sandboxMissing(mode home.Mode) string {
	if runtime.GOOS != "linux" || mode == home.ModeTalk {
		return ""
	}
	if _, err := exec.LookPath("bwrap"); err == nil {
		return ""
	}
	return "Work and Crew asks need bubblewrap for the sandbox: install it (apt install bubblewrap, dnf install bubblewrap), or switch the agent to Talk mode"
}

// deviceHandle is the handle the agent device at dev logs in with, ""
// when it isn't an agent login of user.
func deviceHandle(dev string, user uint64) string {
	var cfg client.Config
	b, err := os.ReadFile(filepath.Join(dev, "config.json"))
	if err != nil || json.Unmarshal(b, &cfg) != nil || cfg.UserID != user {
		return ""
	}
	return cfg.Agent
}

// renameDevice points the agent device's login at a new handle, keeping
// every other field of its config.json.
func renameDevice(dev, handle string) error {
	p := filepath.Join(dev, "config.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("parsing %s: %w", p, err)
	}
	m["agent"] = handle
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return home.WriteFileAtomic(p, append(out, '\n'), 0o600)
}

func ask(in *bufio.Reader, out io.Writer, q, def string) string {
	fmt.Fprintf(out, "%s [%s]: ", q, def)
	line, _ := in.ReadString('\n')
	if line = strings.TrimSpace(line); line == "" {
		return def
	}
	return line
}

// serverHosts is the tc server's host, for the sandbox network allowlist.
func serverHosts(addr string) []string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil || h == "" {
		return nil
	}
	return []string{h}
}

// agentRun is `tc agent run`, the supervisor (tc background starts it).
func agentRun(ctx context.Context, tcHome string) error {
	dir := home.Dir(tcHome)
	paths := home.PathsFor(dir)
	cfg, err := home.LoadConfig(dir)
	if err != nil {
		return err
	}
	if cfg.Handle == "" {
		return errors.New("no agent here yet: run tc agent setup")
	}
	if err := os.MkdirAll(paths.State, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(paths.State, "run.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("the agent is already running on this computer")
	}
	logw, err := background.OpenLogAt(filepath.Join(paths.State, "agent.log"))
	if err != nil {
		return err
	}
	defer logw.Close()
	log := slog.New(slog.NewTextHandler(logw, nil))

	c, err := client.Open(paths.DeviceHome)
	if err != nil {
		return fmt.Errorf("the agent isn't signed in on this computer (run tc agent setup): %w", err)
	}
	defer c.Close()
	if !c.IsAgentSession() {
		return errors.New("the agent's tc home isn't an agent login: run tc agent setup")
	}
	if err := home.EnsureWith(dir, home.Options{ExtraDomains: serverHosts(c.Config().Addr)}); err != nil {
		return err
	}
	if msg := sandboxMissing(cfg.Mode); msg != "" {
		log.Warn(msg)
		fmt.Fprintln(os.Stderr, "tc agent:", msg)
	}
	mem, err := memory.Open(dir)
	if err != nil {
		return err
	}
	exe, _ := os.Executable()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.Run(cctx)
	s := &agent.Supervisor{
		Dir: dir, Chat: c, Runner: runner.Claude{}, Memory: mem, Work: work.New(dir),
		TCBin: exe, Server: c.Config().Addr, Version: version, Log: log, Model: os.Getenv("TC_AGENT_MODEL"),
		Refresh: func() error {
			return home.EnsureWith(dir, home.Options{ExtraDomains: serverHosts(c.Config().Addr)})
		},
	}
	err = s.Run(cctx)
	if err != nil {
		log.Error("exiting", "err", err)
	}
	return err
}

func agentStatus(out io.Writer, tcHome string, now time.Time) error {
	dir := home.Dir(tcHome)
	cfg, err := home.LoadConfig(dir)
	if err != nil {
		return err
	}
	if cfg.Handle == "" {
		fmt.Fprintln(out, "No agent yet. Make one: tc agent setup")
		return nil
	}
	fmt.Fprintf(out, "@%s %s · mode %s", sanitize.Line(cfg.Handle), sanitize.Line(cfg.Icon), cfg.Mode)
	if cfg.Paused {
		fmt.Fprint(out, " · paused")
	}
	fmt.Fprintln(out)
	if !agent.HomeHere(tcHome) {
		fmt.Fprintln(out, "It doesn't live on this computer (run tc agent setup here to move it).")
	}
	paths := home.PathsFor(dir)
	l, _ := agent.OpenLedger(paths.State)
	spent := 0.0
	if l != nil {
		spent = l.Spent(now)
	}
	fmt.Fprintf(out, "spent today: $%.2f of $%.2f (per ask ≤ $%.2f, %d asks per person)\n", spent, cfg.DailyCapUSD, cfg.RunCapUSD, cfg.AsksPerPerson)
	st, err := agent.ReadStatus(paths.State)
	running := false
	if lf, lerr := os.OpenFile(filepath.Join(paths.State, "run.lock"), os.O_RDWR, 0); lerr == nil {
		if syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			running = true
		}
		lf.Close()
	}
	switch {
	case !running:
		fmt.Fprintln(out, "supervisor: not running (tc background on, or tc agent run)")
	case err != nil:
		fmt.Fprintln(out, "supervisor: running")
	default:
		fmt.Fprintf(out, "supervisor: %s since %s (pid %d)\n", sanitize.Line(st.State), ago(now, st.Since), st.PID)
		if st.LastRun > 0 {
			fmt.Fprintf(out, "last answered: %s\n", ago(now, st.LastRun))
		}
	}
	if running && err == nil {
		for _, g := range st.Grants {
			if g.Day == now.Format(time.DateOnly) {
				fmt.Fprintf(out, "♾️ today: %s (stop: say \"%s\" in your agent DM)\n", sanitize.Line(agent.GrantText(g)), agent.StopAutoPhrase)
			}
		}
		for _, w := range st.Watches {
			fmt.Fprintf(out, "watching %s until %s: %s\n", sanitize.Line(w.Where), time.UnixMilli(w.Until).Local().Format("15:04"), sanitize.Line(w.About))
		}
	}
	if err == nil && st.Err != "" {
		fmt.Fprintf(out, "last error: %s\n", sanitize.Line(st.Err))
	}
	fmt.Fprintf(out, "log: %s\n", sanitize.Line(filepath.Join(paths.State, "agent.log")))
	return nil
}

func agentPause(out io.Writer, dir string, pause bool) error {
	cfg, err := home.LoadConfig(dir)
	if err != nil {
		return err
	}
	if cfg.Handle == "" {
		return errors.New("no agent yet: run tc agent setup")
	}
	cfg.Paused = pause
	if err := home.SaveConfig(dir, cfg); err != nil {
		return err
	}
	if pause {
		fmt.Fprintf(out, "@%s is paused: it answers asks with \"paused\" until tc agent resume.\n", sanitize.Line(cfg.Handle))
	} else {
		fmt.Fprintf(out, "@%s is back.\n", sanitize.Line(cfg.Handle))
	}
	return nil
}

func agentLogs(out io.Writer, dir string, n int) error {
	acts, err := agent.ReadActivity(home.PathsFor(dir).State, max(n, 1))
	if err != nil {
		return err
	}
	if len(acts) == 0 {
		fmt.Fprintln(out, "Nothing yet.")
		return nil
	}
	for _, a := range acts {
		t := time.UnixMilli(a.T).Local().Format("Jan 2 15:04")
		line := fmt.Sprintf("%s  %-8s", t, sanitize.Line(a.What))
		if a.Who != "" {
			line += " " + sanitize.Line(a.Who)
		}
		if a.Where != "" {
			line += " in " + sanitize.Line(a.Where)
		}
		if a.Detail != "" {
			line += ": " + sanitize.Line(a.Detail)
		}
		if a.USD > 0 {
			line += fmt.Sprintf(" ($%.2f)", a.USD)
		}
		fmt.Fprintln(out, line)
	}
	return nil
}
