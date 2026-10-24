package main

import (
	"context"
	"errors"
	"flag"
	"os"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/mcp"
)

// cmdMCP is `tc mcp`: an MCP server on stdin/stdout. With --agent it is the
// personal agent's tool server, talking to its supervisor; without, it
// serves an outside Claude session from your own tc home.
func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	sock := fs.String("agent", "", "the agent supervisor's socket (set by tc agent run)")
	fs.Parse(args)
	ctx, cancel := signalContext()
	defer cancel()
	if *sock != "" {
		token := os.Getenv("TC_AGENT_TOKEN")
		if token == "" {
			return errors.New("tc mcp --agent runs only under the agent supervisor")
		}
		s := &mcp.Server{Name: mcp.ServerName, Version: version, Tools: mcp.AgentTools(), Call: mcp.AgentCall(*sock, token)}
		return s.Serve(ctx, os.Stdin, os.Stdout)
	}
	return visitorMCP(ctx)
}

// visitorMCP answers from the cache at once and connects in the background:
// Claude Code waits for initialize, not for the network.
func visitorMCP(ctx context.Context) error {
	c, err := client.Open(client.DefaultHome())
	if err != nil {
		return err
	}
	defer c.Close()
	if c.IsAgentSession() {
		return errors.New("this tc home is an agent's: run tc mcp from your own")
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.Run(cctx)
	go func() {
		for range c.Updates() {
		}
	}()
	// The channels the owner listed in the agent panel; not the server's
	// access list, which also holds channels granted because someone
	// called the agent there.
	dir := home.Dir(client.DefaultHome())
	v := &mcp.Visitor{Chat: c, Allowed: func(context.Context) ([]uint64, error) {
		cfg, err := home.LoadConfig(dir)
		if err != nil {
			return nil, err
		}
		return cfg.Access.Chans, nil
	}}
	s := &mcp.Server{Name: mcp.ServerName, Version: version, Tools: mcp.VisitorTools(), Instructions: mcp.VisitorInstructions, Call: v.Call}
	return s.Serve(cctx, os.Stdin, os.Stdout)
}
