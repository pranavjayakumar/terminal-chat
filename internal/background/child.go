package background

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Child is a process tc background keeps running beside the notifier (the
// personal agent's supervisor, `tc agent run`).
type Child struct {
	Name string
	Path string
	Args []string
	Env  []string // extra, KEY=value
	// Want reports whether the child should run now; it is asked again
	// every Poll, so setting the agent up (or moving it to another
	// machine) takes effect without a restart.
	Want   func() bool
	Stdout io.Writer // nil: discarded
	Log    *slog.Logger

	Poll       time.Duration // default 30 s
	MinBackoff time.Duration // default 1 s
	MaxBackoff time.Duration // default 5 min
	// Grace is how long the child gets to exit on SIGTERM (default 10 s).
	Grace time.Duration
}

// Supervise runs c while Want says so, restarting it with backoff when it
// exits, until ctx ends; then it stops the child and returns.
func Supervise(ctx context.Context, c Child) {
	if c.Poll <= 0 {
		c.Poll = 30 * time.Second
	}
	if c.MinBackoff <= 0 {
		c.MinBackoff = time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 5 * time.Minute
	}
	if c.Grace <= 0 {
		c.Grace = 10 * time.Second
	}
	if c.Log == nil {
		c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.Stdout == nil {
		c.Stdout = io.Discard
	}
	backoff := c.MinBackoff
	for ctx.Err() == nil {
		if c.Want != nil && !c.Want() {
			if !sleepCtx(ctx, c.Poll) {
				return
			}
			continue
		}
		start := time.Now()
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 2*c.MaxBackoff {
			backoff = c.MinBackoff // it ran fine for a while: a fresh failure
		}
		c.Log.Warn("child exited", "name", c.Name, "err", err, "restart_in", backoff)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, c.MaxBackoff)
	}
}

// runOnce runs the child until it exits, ctx ends, or Want turns false.
func (c Child) runOnce(ctx context.Context) error {
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stdout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	c.Log.Info("child started", "name", c.Name, "pid", cmd.Process.Pid)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t := time.NewTicker(c.Poll)
	defer t.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return c.stop(cmd, done)
		case <-t.C:
			if c.Want != nil && !c.Want() {
				c.Log.Info("child no longer wanted", "name", c.Name)
				return c.stop(cmd, done)
			}
		}
	}
}

func (c Child) stop(cmd *exec.Cmd, done chan error) error {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case err := <-done:
		return err
	case <-time.After(c.Grace):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return <-done
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
