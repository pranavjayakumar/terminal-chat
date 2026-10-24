// Package mcp is `tc mcp`: a Model Context Protocol server over stdio that
// gives a Claude session tc's tools. It speaks the small part of MCP a tool
// server needs (initialize, ping, tools/list, tools/call, cancellation),
// JSON-RPC 2.0 with one message per line.
//
// Two modes: the agent's (`tc mcp --agent <sock>`), which forwards every
// call to the agent supervisor over a unix socket so the supervisor decides
// what the asker may do; and the visitor's (`tc mcp`), which serves an
// outside Claude session from the owner's own tc home.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
)

// ProtocolVersion is the newest MCP revision this server implements.
const ProtocolVersion = "2025-06-18"

var knownVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// Tool describes one tool for tools/list.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// CallFunc runs a tool. A returned error becomes a tool result with
// isError set (the model sees the message), not a protocol error.
type CallFunc func(ctx context.Context, name string, args json.RawMessage) (string, error)

// Server is one MCP server session.
type Server struct {
	Name, Version string
	Instructions  string
	Tools         []Tool
	Call          CallFunc
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	codeParse         = -32700
	codeNoMethod      = -32601
	codeInvalidParams = -32602
)

// maxLine bounds one incoming message.
const maxLine = 8 << 20

// Serve reads requests from r and writes responses to w until r ends or
// ctx is done. Tool calls run concurrently, so a call waiting on the owner
// doesn't hold up the others.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wmu sync.Mutex
	enc := json.NewEncoder(w)
	send := func(m rpcMsg) {
		m.JSONRPC = "2.0"
		wmu.Lock()
		defer wmu.Unlock()
		_ = enc.Encode(m)
	}
	var (
		mu       sync.Mutex
		inflight = map[string]context.CancelFunc{}
		wg       sync.WaitGroup
	)
	defer wg.Wait()
	defer cancel()

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m rpcMsg
		if err := json.Unmarshal(line, &m); err != nil {
			send(rpcMsg{ID: json.RawMessage("null"), Error: &rpcError{codeParse, "parse error"}})
			continue
		}
		if m.Method == "" {
			continue // a response to something we never ask
		}
		notify := len(m.ID) == 0 || string(m.ID) == "null"
		reply := func(res any, e *rpcError) {
			if notify {
				return
			}
			if e != nil {
				send(rpcMsg{ID: m.ID, Error: e})
				return
			}
			send(rpcMsg{ID: m.ID, Result: res})
		}
		switch m.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(m.Params, &p)
			v := ProtocolVersion
			if slices.Contains(knownVersions, p.ProtocolVersion) {
				v = p.ProtocolVersion
			}
			res := map[string]any{
				"protocolVersion": v,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
			}
			if s.Instructions != "" {
				res["instructions"] = s.Instructions
			}
			reply(res, nil)
		case "ping":
			reply(map[string]any{}, nil)
		case "tools/list":
			tools := s.Tools
			if tools == nil {
				tools = []Tool{}
			}
			reply(map[string]any{"tools": tools}, nil)
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(m.Params, &p); err != nil || p.Name == "" {
				reply(nil, &rpcError{codeInvalidParams, "tools/call needs a name"})
				continue
			}
			if !s.hasTool(p.Name) {
				reply(nil, &rpcError{codeInvalidParams, "unknown tool " + p.Name})
				continue
			}
			if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
				p.Arguments = json.RawMessage("{}")
			}
			cctx, ccancel := context.WithCancel(ctx)
			key := string(m.ID)
			mu.Lock()
			inflight[key] = ccancel
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(inflight, key)
					mu.Unlock()
					ccancel()
				}()
				text, err := s.call(cctx, p.Name, p.Arguments)
				if cctx.Err() != nil && ctx.Err() == nil {
					return // cancelled by the client: no response is expected
				}
				res := map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}
				if err != nil {
					res["content"] = []map[string]string{{"type": "text", "text": err.Error()}}
					res["isError"] = true
				}
				reply(res, nil)
			}()
		case "notifications/cancelled":
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(m.Params, &p) == nil {
				mu.Lock()
				if c := inflight[string(p.RequestID)]; c != nil {
					c()
				}
				mu.Unlock()
			}
		default:
			if !notify {
				reply(nil, &rpcError{codeNoMethod, "method not found: " + m.Method})
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading MCP input: %w", err)
	}
	return nil
}

func (s *Server) hasTool(name string) bool {
	return slices.ContainsFunc(s.Tools, func(t Tool) bool { return t.Name == name })
}

func (s *Server) call(ctx context.Context, name string, args json.RawMessage) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool %s failed", name)
		}
	}()
	if s.Call == nil {
		return "", errors.New("no tools here")
	}
	return s.Call(ctx, name, args)
}
