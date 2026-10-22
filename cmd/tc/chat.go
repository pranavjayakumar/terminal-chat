package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/skailama/tc/internal/background"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/tui"
)

func cmdChat() error {
	c, err := client.Open(client.DefaultHome())
	if errors.Is(err, client.ErrNotLoggedIn) {
		fmt.Fprint(os.Stderr, "Not logged in. Ask an admin for an invite, then:\n\n  tc join <invite>\n\n"+
			"Already in the team on another computer? Run tc login there to see its server line, then here:\n\n  tc login 'tc://host:port#pin'\n\n")
		return nil
	}
	if err != nil {
		return err
	}
	defer c.Close()
	// While this window is open tc background stays quiet (no double
	// popups); the OS drops the lock when we exit, crash or not.
	release, err := background.HoldUI(c.Home())
	if err != nil {
		fmt.Fprintln(os.Stderr, "tc: warning:", err)
	}
	defer release()
	ctx, cancel := signalContext()
	defer cancel()
	go c.Run(ctx)
	return tui.Run(ctx, c, version)
}
