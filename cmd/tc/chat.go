package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/skailama/tc/internal/background"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/tui"
)

func cmdChat() error {
	c, err := client.Open(client.DefaultHome())
	if errors.Is(err, client.ErrNotLoggedIn) {
		fmt.Fprint(os.Stderr, "Not logged in. Ask an admin for an invite, then:\n\n  tc join <invite>\n\n"+
			"Already in the team on another computer? Run tc login there to see its server line, then here:\n\n  tc login 'tc://host:port#pin'\n\n"+
			"(That needs the same SSH key here. Without it, ask an admin for an add-key invite: tc invite --for @you.)\n\n")
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	release, err := holdUI(ctx, c.Home(), c.Run)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tc: warning:", err)
	}
	// Our connection is gone before the notifier may take over.
	defer func() {
		cancel()
		c.Close()
		release()
	}()
	return tui.Run(ctx, c, version)
}

// holdUI marks a chat window open, which makes tc background let go of its
// connection (no double popups; the OS drops the lock when we exit, crash
// or not), and starts run once it has. Until then the window shows the
// cache and queues sends: two clients on one cache would both apply the
// same events, and a message the paused notifier opened first would reach
// neither the window's notifications (not new to it) nor the notifier's
// (a window is open).
func holdUI(ctx context.Context, home string, run func(context.Context)) (release func(), err error) {
	release, err = background.HoldUI(home)
	if err != nil {
		// Without the lock the notifier never pauses: don't wait for it.
		go run(ctx)
		return release, err
	}
	go func() {
		if background.Running(home) {
			waitFor(5*time.Second, func() bool { return ctx.Err() != nil || background.Quiet(home) })
		}
		run(ctx)
	}()
	return release, nil
}
