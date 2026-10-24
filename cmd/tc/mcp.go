package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"sync"

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
	var mu sync.Mutex
	var allowed []uint64
	var known bool
	v := &mcp.Visitor{Chat: c, Allowed: func(ctx context.Context) ([]uint64, error) {
		ids, err := c.AgentAccess(ctx)
		mu.Lock()
		defer mu.Unlock()
		if err == nil {
			allowed, known = ids, true
			return ids, nil
		}
		if known {
			return allowed, nil // offline: the last list we saw
		}
		return nil, err
	}}
	s := &mcp.Server{Name: mcp.ServerName, Version: version, Tools: mcp.VisitorTools(), Instructions: mcp.VisitorInstructions, Call: v.Call}
	return s.Serve(cctx, os.Stdin, os.Stdout)
}
