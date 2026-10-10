package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/tui"
)

func cmdChat() error {
	c, err := client.Open(client.DefaultHome())
	if errors.Is(err, client.ErrNotLoggedIn) {
		fmt.Fprint(os.Stderr, "Not logged in. Ask an admin for an invite, then:\n\n  tc join <invite>\n\n")
		return nil
	}
	if err != nil {
		return err
	}
	defer c.Close()
	ctx, cancel := signalContext()
	defer cancel()
	go c.Run(ctx)
	return tui.Run(ctx, c, version)
}
