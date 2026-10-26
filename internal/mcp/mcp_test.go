package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// rpc drives a Server over pipes like Claude Code does.
type rpc struct {
	t   *testing.T
	in  *io.PipeWriter
	out *bufio.Scanner
	mu  sync.Mutex
}

func startServer(t *testing.T, s *Server) *rpc {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), inR, outW); outW.Close() }()
	t.Cleanup(func() {
		inW.Close()
		go io.Copy(io.Discard, outR)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve didn't return after stdin closed")
		}
	})
	sc := bufio.NewScanner(outR)
	sc.Buffer(nil, 1<<20)
	return &rpc{t: t, in: inW, out: sc}
}

func (r *rpc) send(line string) {
	r.t.Helper()
	if _, err := io.WriteString(r.in, line+"\n"); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rpc) recv() map[string]any {
	r.t.Helper()
	if !r.out.Scan() {
		r.t.Fatalf("no response: %v", r.out.Err())
	}
	var m map[string]any
	if err := json.Unmarshal(r.out.Bytes(), &m); err != nil {
		r.t.Fatalf("bad response %s: %v", r.out.Bytes(), err)
	}
	if m["jsonrpc"] != "2.0" {
		r.t.Fatalf("not jsonrpc 2.0: %s", r.out.Bytes())
	}
	return m
}

func TestProtocolRoundTrip(t *testing.T) {
	release := make(chan struct{})
	s := &Server{Name: "tc", Version: "test", Tools: AgentTools(), Call: func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		switch name {
		case ToolWhoami:
			return "rocky", nil
		case ToolAskOwner:
			select {
			case <-release:
				return "yes", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		case ToolRecall:
			return "", errors.New("memory is empty")
		}
		panic("boom")
	}}
	r := startServer(t, s)

	r.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"claude-code","version":"2"}}}`)
	m := r.recv()
	res := m["result"].(map[string]any)
	if m["id"].(float64) != 1 || res["protocolVersion"] != "2025-03-26" || res["serverInfo"].(map[string]any)["name"] != "tc" {
		t.Fatalf("initialize: %v", m)
	}
	r.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	r.send(`{"jsonrpc":"2.0","id":"a","method":"tools/list"}`)
	m = r.recv()
	tools := m["result"].(map[string]any)["tools"].([]any)
	if len(tools) != len(AgentTools()) || m["id"] != "a" {
		t.Fatalf("tools/list: %v", m)
	}
	for _, x := range tools {
		tool := x.(map[string]any)
		if tool["name"] == "" || tool["inputSchema"].(map[string]any)["type"] != "object" {
			t.Fatalf("bad tool %v", tool)
		}
	}

	// A slow call doesn't block the next one.
	r.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ask_owner","arguments":{"action":"push"}}}`)
	r.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
	m = r.recv()
	if m["id"].(float64) != 3 || text(m) != "rocky" {
		t.Fatalf("whoami: %v", m)
	}
	close(release)
	m = r.recv()
	if m["id"].(float64) != 2 || text(m) != "yes" {
		t.Fatalf("ask_owner: %v", m)
	}

	r.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"recall","arguments":{"query":"x"}}}`)
	m = r.recv()
	if text(m) != "memory is empty" || m["result"].(map[string]any)["isError"] != true {
		t.Fatalf("tool error: %v", m)
	}
	r.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"stream","arguments":{"text":"x"}}}`)
	if m = r.recv(); m["result"].(map[string]any)["isError"] != true || strings.Contains(text(m), "boom") {
		t.Fatalf("panicking tool: %v", m)
	}
	r.send(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"rm_rf","arguments":{}}}`)
	if m = r.recv(); m["error"].(map[string]any)["code"].(float64) != codeInvalidParams {
		t.Fatalf("unknown tool: %v", m)
	}
	r.send(`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`)
	if m = r.recv(); m["error"].(map[string]any)["code"].(float64) != codeNoMethod {
		t.Fatalf("unknown method: %v", m)
	}
	r.send(`{not json`)
	if m = r.recv(); m["error"].(map[string]any)["code"].(float64) != codeParse {
		t.Fatalf("parse error: %v", m)
	}
	r.send(`{"jsonrpc":"2.0","id":8,"method":"ping"}`)
	if m = r.recv(); m["id"].(float64) != 8 || m["result"] == nil {
		t.Fatalf("ping: %v", m)
	}
}

func TestProtocolCancel(t *testing.T) {
	cancelled := make(chan struct{})
	s := &Server{Name: "tc", Tools: AgentTools(), Call: func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		if name == ToolWhoami {
			return "me", nil
		}
		<-ctx.Done()
		close(cancelled)
		return "", ctx.Err()
	}}
	r := startServer(t, s)
	r.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask_owner","arguments":{"action":"x"}}}`)
	r.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("call not cancelled")
	}
	// No response for the cancelled call: the next one is the first out.
	r.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"whoami"}}`)
	if m := r.recv(); m["id"].(float64) != 2 {
		t.Fatalf("got %v", m)
	}
}

func text(m map[string]any) string {
	res, _ := m["result"].(map[string]any)
	c, _ := res["content"].([]any)
	if len(c) == 0 {
		return ""
	}
	return c[0].(map[string]any)["text"].(string)
}

func TestBridge(t *testing.T) {
	sock := filepath.Join(shortTemp(t), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hungUp := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ServeBridge(ctx, ln, func(ctx context.Context, r BridgeReq) BridgeRes {
			switch r.Tool {
			case "wait":
				<-ctx.Done()
				close(hungUp)
				return BridgeRes{}
			case "bad":
				return BridgeRes{Text: "no", Error: true}
			}
			return BridgeRes{Text: r.Token + ":" + r.Tool + ":" + string(r.Args)}
		})
	}()
	call := AgentCall(sock, "tok")
	if got, err := call(context.Background(), "whoami", json.RawMessage(`{"a":1}`)); err != nil || got != `tok:whoami:{"a":1}` {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := call(context.Background(), "bad", nil); err == nil || err.Error() != "no" {
		t.Fatalf("err %v", err)
	}
	cctx, ccancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer ccancel()
	if _, err := call(cctx, "wait", nil); err == nil {
		t.Fatal("wait returned")
	}
	select {
	case <-hungUp:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor side never saw the hang-up")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := AgentCall(sock, "tok")(context.Background(), "whoami", nil); err == nil {
		t.Fatal("call after close worked")
	}
}

// shortTemp is a temp dir short enough for a unix socket path.
func shortTemp(t *testing.T) string {
	d, err := os.MkdirTemp("/tmp", "tcmcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// ---- visitor ----

type fakeChat struct {
	me    proto.User
	users map[uint64]proto.User
	chans []client.ChannelView
	msgs  []client.Message
	sent  []string
}

func (f *fakeChat) Me() proto.User                 { return f.me }
func (f *fakeChat) User(id uint64) proto.User      { return f.users[id] }
func (f *fakeChat) Channels() []client.ChannelView { return f.chans }
func (f *fakeChat) Channel(id uint64) (client.ChannelView, bool) {
	for _, c := range f.chans {
		if c.ID == id {
			return c, true
		}
	}
	return client.ChannelView{}, false
}
func (f *fakeChat) Messages(ch uint64, limit int) []client.Message {
	var out []client.Message
	for _, m := range f.msgs {
		if m.Chan == ch && m.Thread == 0 {
			out = append(out, m)
		}
	}
	return LastN(out, limit)
}
func (f *fakeChat) Thread(root uint64) []client.Message {
	var out []client.Message
	for _, m := range f.msgs {
		if m.ID == root || m.Thread == root {
			out = append(out, m)
		}
	}
	return out
}
func (f *fakeChat) Search(_ context.Context, q string, ch uint64) ([]proto.Message, error) {
	var out []proto.Message
	for _, m := range f.msgs {
		if strings.Contains(m.Body, q) && (ch == 0 || m.Chan == ch) {
			out = append(out, m.Message)
		}
	}
	return out, nil
}
func (f *fakeChat) AgentOf(user uint64) (proto.Agent, bool) {
	if user == 2 {
		return proto.Agent{Handle: "bobbot", Owner: 2}, true
	}
	return proto.Agent{}, false
}
func (f *fakeChat) SendVia(ch uint64, body string, thread uint64, via string) error {
	f.sent = append(f.sent, via+":"+body)
	return nil
}
func (f *fakeChat) Flush(context.Context) error { return nil }

func msg(id, ch, author, thread uint64, body string) client.Message {
	return client.Message{Message: proto.Message{ID: id, Chan: ch, Author: author, Thread: thread, Body: body, Time: 1}}
}

func newFake() *fakeChat {
	return &fakeChat{
		me:    proto.User{ID: 1, Name: "fizan"},
		users: map[uint64]proto.User{1: {ID: 1, Name: "fizan"}, 2: {ID: 2, Name: "bob"}},
		chans: []client.ChannelView{
			{Channel: proto.Channel{ID: 10, Name: "general", Kind: proto.ChanPublic}},
			{Channel: proto.Channel{ID: 11, Name: "secret", Kind: proto.ChanPrivate}},
		},
		msgs: []client.Message{
			msg(100, 10, 2, 0, "deploy is green"),
			msg(101, 10, 1, 100, "nice"),
			msg(102, 11, 2, 0, "deploy password is hunter2"),
		},
	}
}

func TestVisitor(t *testing.T) {
	f := newFake()
	f.msgs[1].Agent = ""
	v := &Visitor{Chat: f, Allowed: func(context.Context) ([]uint64, error) { return []uint64{10}, nil }}
	call := func(name, args string) (string, error) {
		return v.Call(context.Background(), name, json.RawMessage(args))
	}
	out, err := call(ToolListChannels, `{}`)
	if err != nil || !strings.Contains(out, "#general") || strings.Contains(out, "secret") {
		t.Fatalf("list: %q %v", out, err)
	}
	if out, err = call(ToolReadChannel, `{"chan":"#general"}`); err != nil || !strings.Contains(out, "bob: deploy is green") {
		t.Fatalf("read: %q %v", out, err)
	}
	if out, err = call(ToolReadThread, `{"chan":"general","thread":100}`); err != nil || !strings.Contains(out, "fizan: nice") {
		t.Fatalf("thread: %q %v", out, err)
	}
	if _, err = call(ToolReadChannel, `{"chan":"#secret"}`); err == nil {
		t.Fatal("read a channel the owner didn't allow")
	}
	if _, err = call(ToolReadThread, `{"chan":"general","thread":102}`); err == nil {
		t.Fatal("read another channel's thread through an allowed one")
	}
	if out, err = call(ToolSearch, `{"q":"deploy"}`); err != nil || strings.Contains(out, "hunter2") || !strings.Contains(out, "green") {
		t.Fatalf("search leaks: %q %v", out, err)
	}
	if _, err = call(ToolPostReply, `{"chan":"#secret","text":"hi"}`); err == nil {
		t.Fatal("posted outside the allowed channels")
	}
	if _, err = call(ToolPostReply, `{"chan":"#general","thread":100,"text":"  shipped \u001b[31m "}`); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0] != "mcp:shipped" {
		t.Fatalf("sent %q", f.sent)
	}
	if _, err = call(ToolRemember, `{}`); err == nil {
		t.Fatal("visitor has memory")
	}
}

func TestFormatAgentAuthor(t *testing.T) {
	f := newFake()
	m := msg(5, 10, 2, 0, "on it")
	m.Agent = "bobbot"
	if out := FormatMessages(f, []client.Message{m}); !strings.Contains(out, "bobbot (bob's agent): on it") {
		t.Fatal(out)
	}
	m.Sec = client.SecInserted
	if out := FormatMessages(f, []client.Message{m}); strings.Contains(out, "on it") {
		t.Fatal("unverified text shown:", out)
	}
}
