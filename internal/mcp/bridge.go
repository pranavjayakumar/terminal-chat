package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
)

// The bridge carries an agent's tool calls from `tc mcp --agent` to the
// supervisor: one unix-socket connection per call, one JSON line each way.
// The token names the run the call belongs to; the supervisor hands it to
// the engine in --mcp-config and forgets it when the run ends, so a stale or
// guessed token reaches nothing.

// BridgeReq is a tool call on the bridge.
type BridgeReq struct {
	Token string          `json:"token"`
	Tool  string          `json:"tool"`
	Args  json.RawMessage `json:"args"`
}

// BridgeRes is its answer.
type BridgeRes struct {
	Text  string `json:"text"`
	Error bool   `json:"error,omitempty"`
}

// BridgeCall sends one call to the supervisor at sock and waits for the
// answer. Closing the connection (ctx done) tells the supervisor to stop
// waiting on the owner.
func BridgeCall(ctx context.Context, sock string, req BridgeReq) (BridgeRes, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return BridgeRes{}, fmt.Errorf("the agent supervisor isn't reachable: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return BridgeRes{}, fmt.Errorf("calling the supervisor: %w", err)
	}
	var res BridgeRes
	if err := json.NewDecoder(bufio.NewReaderSize(conn, 64<<10)).Decode(&res); err != nil {
		if ctx.Err() != nil {
			return BridgeRes{}, ctx.Err()
		}
		return BridgeRes{}, fmt.Errorf("reading the supervisor's answer: %w", err)
	}
	return res, nil
}

// AgentCall is the CallFunc of `tc mcp --agent`: every call goes over the
// bridge with the run's token.
func AgentCall(sock, token string) CallFunc {
	return func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		res, err := BridgeCall(ctx, sock, BridgeReq{Token: token, Tool: name, Args: args})
		if err != nil {
			return "", err
		}
		if res.Error {
			return "", errors.New(res.Text)
		}
		return res.Text, nil
	}
}

// ServeBridge answers calls on ln until ctx ends. Each call's context is
// cancelled when its caller hangs up.
func ServeBridge(ctx context.Context, ln net.Listener, handle func(context.Context, BridgeReq) BridgeRes) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			serveBridgeConn(ctx, conn, handle)
		}()
	}
}

func serveBridgeConn(ctx context.Context, conn net.Conn, handle func(context.Context, BridgeReq) BridgeRes) {
	r := bufio.NewReaderSize(conn, 64<<10)
	var req BridgeReq
	dec := json.NewDecoder(&limitedReader{r: r, n: maxLine})
	if err := dec.Decode(&req); err != nil {
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		// The caller sends nothing more: a read returning means it hung up.
		var b [1]byte
		_, _ = r.Read(b[:])
		cancel()
	}()
	res := handle(cctx, req)
	_ = json.NewEncoder(conn).Encode(res)
}

type limitedReader struct {
	r interface{ Read([]byte) (int, error) }
	n int
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("request too large")
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= n
	return n, err
}
