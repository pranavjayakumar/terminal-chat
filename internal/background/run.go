package background

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/skailama/tc/internal/alert"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/sanitize"
)

// Config configures Run.
type Config struct {
	Home    string
	Version string
	// Out shows notifications; nil: the OS notifier (osascript,
	// notify-send). Tests pass a fake.
	Out alert.Sender
	Env alert.Env
	Log *slog.Logger // nil: discard
	// Exe is the binary to watch: when it is replaced (tc update, brew
	// upgrade) Run returns ErrBinaryChanged so the service manager starts
	// the new one. "": don't watch.
	Exe string

	// Poll is how often to re-check config.json while not logged in or
	// stopped, a held run lock and Exe (default 30 s).
	Poll time.Duration
	// UIPoll is how often to look for a chat window opening or closing
	// (default 1 s; one non-blocking flock each time).
	UIPoll time.Duration
	Now    func() time.Time
}

// ErrBinaryChanged: the tc binary was replaced; exit so the new one starts.
var ErrBinaryChanged = errors.New("the tc binary changed")

// OSSender is the notifier the background process uses: the OS backend
// (Notification Center, notify-send), never herdr, the terminal or the
// bell, since there is no window. TC_NOTIFY=off still silences it.
func OSSender() (*notify.Notifier, error) {
	e := notify.OSEnv()
	get := e.Getenv
	e.Getenv = func(k string) string {
		switch k {
		case "TC_NOTIFY":
			if v := get(k); v == "off" || v == "none" || v == "0" {
				return v
			}
			return "os"
		case "HERDR_ENV", "SSH_CONNECTION", "SSH_TTY", "TMUX", "TC_NOTIFY_OSC":
			return ""
		}
		return get(k)
	}
	n := notify.New(e, nil)
	switch n.Backend() {
	case notify.OSAScript, notify.NotifySnd, notify.Off:
		return n, nil
	}
	return nil, errors.New("no desktop notifier here: install notify-send (libnotify-bin on Debian/Ubuntu, libnotify on Fedora/Arch)")
}

// Run is `tc background run`: it holds home's run lock (waiting while
// another notifier holds it), connects passively and notifies until ctx
// ends or the binary changes. Not logged in, or shut out by the server
// (revoked, key no longer accepted), it waits for config.json to change.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Poll <= 0 {
		cfg.Poll = 30 * time.Second
	}
	if cfg.UIPoll <= 0 {
		cfg.UIPoll = time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Out == nil {
		n, err := OSSender()
		if err != nil {
			return err
		}
		cfg.Out = n
	}
	// Mostly idle: give memory back rather than hold a big heap.
	debug.SetGCPercent(50)

	lock, err := acquireRun(cfg.Home, ctx.Done(), cfg.Poll)
	if errors.Is(err, errStopped) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	probe, err := NewUIProbe(cfg.Home)
	if err != nil {
		return err
	}
	defer probe.Close()

	r := &runner{cfg: cfg, probe: probe, exe: statOf(cfg.Exe)}
	now := cfg.Now().UnixMilli()
	r.st = Status{PID: os.Getpid(), Version: cfg.Version, Started: now, State: "starting", Since: now}
	r.save()
	defer func() {
		r.mu.Lock()
		r.st.State, r.st.Transport, r.st.Since = "stopped", "", cfg.Now().UnixMilli()
		r.mu.Unlock()
		r.save()
	}()
	cfg.Log.Info("started", "version", cfg.Version, "pid", os.Getpid())
	for {
		err := r.waitUIClosed(ctx)
		if err == nil {
			err = r.session(ctx)
		}
		if ctx.Err() != nil {
			cfg.Log.Info("stopping")
			return nil
		}
		if errors.Is(err, ErrBinaryChanged) {
			cfg.Log.Info("binary changed: exiting so the new one starts")
			return err
		}
		if errors.Is(err, errUIOpened) || errors.Is(err, errLoginChanged) {
			continue
		}
		r.setErr("waiting", err)
		cfg.Log.Warn("waiting for a login change", "err", sanitize.Line(err.Error()))
		if err := r.waitConfig(ctx); err != nil {
			if errors.Is(err, ErrBinaryChanged) {
				return err
			}
			return nil
		}
	}
}

// errUIOpened: a chat window opened, so the session was closed.
var errUIOpened = errors.New("tc is open")

// errLoginChanged: config.json changed under a live session.
var errLoginChanged = errors.New("login changed")

// waitUIClosed returns once no chat window is open. While one is, the
// notifier holds no connection at all: the window does the notifying, and
// two processes on one cache would race to open waiting encrypted messages
// (whoever opens one first is the one that reports it as new).
func (r *runner) waitUIClosed(ctx context.Context) error {
	if !r.probe.Open() {
		return nil
	}
	r.setErr("paused", nil)
	r.cfg.Log.Info("tc is open: paused")
	t := time.NewTicker(r.cfg.UIPoll)
	defer t.Stop()
	check := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if !r.probe.Open() {
				r.cfg.Log.Info("tc closed: resuming")
				return nil
			}
			if time.Since(check) >= r.cfg.Poll {
				check = time.Now()
				if r.binaryChanged() {
					return ErrBinaryChanged
				}
			}
		}
	}
}

type runner struct {
	cfg   Config
	probe *UIProbe
	exe   os.FileInfo

	core      *Core       // kept across sessions of one login
	coreLogin os.FileInfo // config.json when core was made

	mu sync.Mutex
	st Status
}

func (r *runner) save() {
	r.mu.Lock()
	st := r.st
	r.mu.Unlock()
	if err := writeStatus(r.cfg.Home, st); err != nil {
		r.cfg.Log.Warn("writing status", "err", err)
	}
}

func (r *runner) setErr(state string, err error) {
	r.mu.Lock()
	now := r.cfg.Now().UnixMilli()
	if state != r.st.State {
		r.st.Since = now
	}
	r.st.State, r.st.Transport = state, ""
	if err != nil {
		r.st.Err, r.st.ErrAt = sanitize.Line(err.Error()), now
	}
	r.mu.Unlock()
	r.save()
}

// errClientStopped: the server shut us out for good (Status.Stopped).
type errClientStopped string

func (e errClientStopped) Error() string { return string(e) }

func (r *runner) session(ctx context.Context) error {
	cfgPath := filepath.Join(r.cfg.Home, "config.json")
	login := statOf(cfgPath)
	c, err := client.Open(r.cfg.Home)
	if err != nil {
		return fmt.Errorf("opening %s: %w", r.cfg.Home, err)
	}
	defer c.Close()
	c.SetPassive(true)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.Run(cctx)

	// The engine outlives a pause for the chat window (ring cooldowns,
	// the cap), but not a change of login.
	if r.core == nil || changed(r.coreLogin, login) {
		r.core = NewCore(c, r.cfg.Out, r.cfg.Env, r.probe.Open, r.cfg.Now)
		r.coreLogin = login
	}
	core := r.core
	core.w.src = c
	core.Sent = func() {
		r.mu.Lock()
		r.st.Notified++
		r.st.LastNote = r.cfg.Now().UnixMilli()
		r.mu.Unlock()
		r.save()
	}
	fire := make(chan alert.Timer, 64)
	schedule := func(ts []alert.Timer) {
		for _, t := range ts {
			time.AfterFunc(t.After, func() {
				select {
				case fire <- t:
				case <-cctx.Done():
				}
			})
		}
	}
	tick := time.NewTicker(r.cfg.Poll)
	defer tick.Stop()
	uiTick := time.NewTicker(r.cfg.UIPoll)
	defer uiTick.Stop()
	var last client.Status
	for {
		select {
		case <-ctx.Done():
			return nil
		case u, ok := <-c.Updates():
			if !ok {
				return nil
			}
			if s := c.Status(); s != last {
				last = s
				r.connState(c, s)
				if s.Stopped {
					return errClientStopped(s.Err)
				}
			}
			schedule(core.Handle(u))
		case t := <-fire:
			schedule(core.Fire(t))
		case <-tick.C:
			if r.binaryChanged() {
				return ErrBinaryChanged
			}
			// tc join/login here wipes the cache for another account:
			// never keep feeding it the old one's events.
			if changed(login, statOf(cfgPath)) {
				r.cfg.Log.Info("login changed: reconnecting")
				return errLoginChanged
			}
		case <-uiTick.C:
			if r.probe.Open() {
				return errUIOpened
			}
		}
	}
}

// connState records a connection state change (never message text).
func (r *runner) connState(c *client.Client, s client.Status) {
	now := r.cfg.Now().UnixMilli()
	r.mu.Lock()
	state := s.State.String()
	if s.Stopped {
		state = "stopped"
	}
	if state != r.st.State {
		r.st.Since = now
	}
	r.st.State, r.st.Transport = state, s.Transport
	if s.Err != "" {
		r.st.Err, r.st.ErrAt = sanitize.Line(s.Err), now
	}
	if s.State == client.Online {
		r.st.Passive = c.PassiveOK()
	}
	r.mu.Unlock()
	r.save()
	if s.Err != "" {
		r.cfg.Log.Info("connection", "state", state, "err", sanitize.Line(s.Err))
	} else {
		r.cfg.Log.Info("connection", "state", state, "transport", s.Transport)
	}
}

// waitConfig returns once config.json changes (a new tc join/login), ctx
// ends (nil error) or the binary changes.
func (r *runner) waitConfig(ctx context.Context) error {
	path := filepath.Join(r.cfg.Home, "config.json")
	before := statOf(path)
	t := time.NewTicker(r.cfg.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if r.binaryChanged() {
				return ErrBinaryChanged
			}
			if changed(before, statOf(path)) {
				return nil
			}
		}
	}
}

func (r *runner) binaryChanged() bool {
	return r.exe != nil && changed(r.exe, statOf(r.cfg.Exe))
}

func statOf(path string) os.FileInfo {
	if path == "" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return fi
}

// changed: appeared, disappeared, or a different file or content.
func changed(a, b os.FileInfo) bool {
	switch {
	case a == nil && b == nil:
		return false
	case a == nil || b == nil:
		return true
	}
	return !os.SameFile(a, b) || !a.ModTime().Equal(b.ModTime()) || a.Size() != b.Size()
}
