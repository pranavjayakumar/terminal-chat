package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/transport"
)

func startTestServer(t *testing.T) *server.Server {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	srv, err := server.New(server.Config{DataDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.ListenWith("127.0.0.1:0", srv.Cert(), transport.ListenOpts{HTTP: true, HTTPCert: srv.HTTPCert()})
	if err != nil {
		t.Fatal(err)
	}
	srv.SetPublicAddr(ln.Addr().String())
	sctx, scancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Serve(sctx, ln); close(done) }()
	t.Cleanup(func() { scancel(); <-done; srv.Close() })
	return srv
}

// B4: a message the server rejected for good stays in the outbox (the TUI
// shows it as failed), but later tc send / tc notify runs report only on
// their own message. Colored argv text is stripped like piped text instead
// of being rejected.
func TestSendIgnoresOlderRejectedRow(t *testing.T) {
	srv := startTestServer(t)
	home := loginHome(t, srv, "alice")
	t.Setenv("TC_HOME", home)

	// Poison the outbox the way an old tc send did: control characters
	// into a plaintext channel, rejected with bad_request.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := connected(ctx)
	if err != nil {
		t.Fatal(err)
	}
	general, err := resolve(ctx, c, "#general")
	if err != nil {
		t.Fatal(err)
	}
	if err := sendWait(ctx, c, general, "\x1b[31mRED\x1b[0m"); err == nil || !strings.Contains(err.Error(), proto.ErrBadRequest) {
		t.Fatalf("raw control characters = %v, want bad_request", err)
	}
	c.Close()

	if err := cmdSend([]string{"#general", "\x1b[32mgreen\x1b[0m", "build", "ok"}); err != nil {
		t.Fatalf("tc send after an old rejection: %v", err)
	}
	if err := postTo(ctx, "#general", "✓ `make` · 1s"); err != nil {
		t.Fatalf("tc notify after an old rejection: %v", err)
	}

	c, err = connected(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var got []string
	failed := 0
	for _, m := range c.Messages(general, 50) {
		if m.Failed != "" {
			failed++
			continue
		}
		got = append(got, m.Body)
	}
	if failed != 1 {
		t.Errorf("failed rows = %d, want the old rejected one kept", failed)
	}
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "green build ok") || strings.Contains(joined, "\x1b") || !strings.Contains(joined, "✓ `make` · 1s") {
		t.Errorf("delivered = %q", joined)
	}
}

func TestNotifyBodySanitizesArgv(t *testing.T) {
	long := strings.Repeat("é", 150) // 300 bytes: the cut lands mid-rune
	for _, argv := range [][]string{{"echo", "\x1b[31mred\x1b[0m\x07"}, {"echo", long}} {
		b := notifyBody(argv, 0, time.Second, "")
		if !sanitize.Valid(b) {
			t.Errorf("notifyBody(%q) = %q: not valid message text", argv, b)
		}
	}
}
