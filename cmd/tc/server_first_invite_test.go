package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
)

// The first-run admin invite used to be printed to stdout on every start
// (the journal, under systemd), a new live one each time. It goes to a
// 0600 file instead, to stdout only on a terminal, and a restart while it
// is open mints none.
func TestFirstRunInviteStaysOutOfLogs(t *testing.T) {
	data := t.TempDir()
	srv, err := server.New(server.Config{DataDir: data, PublicAddr: "chat.example.com:7443",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx := context.Background()

	var out bytes.Buffer
	if err := firstRunInvite(ctx, srv, data, &out, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, firstInviteFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("invite file mode %v", fi.Mode().Perm())
	}
	invs, _ := srv.ListInvites(ctx)
	if len(invs) != 1 {
		t.Fatalf("%d invites", len(invs))
	}
	code := inviteCodeIn(t, string(b))
	if strings.Contains(out.String(), code) || !strings.Contains(out.String(), path) {
		t.Fatalf("non-terminal output %q: has the invite, or lacks the path", out.String())
	}

	// A restart: no new invite; a terminal sees the same one.
	out.Reset()
	if err := firstRunInvite(ctx, srv, data, &out, true); err != nil {
		t.Fatal(err)
	}
	if invs, _ := srv.ListInvites(ctx); len(invs) != 1 {
		t.Fatalf("restart minted another invite: %d", len(invs))
	}
	if !strings.Contains(out.String(), code) {
		t.Fatalf("terminal output lacks the invite: %q", out.String())
	}
}

// inviteCodeIn finds the invite token (the tc:// code's path) in s.
func inviteCodeIn(t *testing.T, s string) string {
	t.Helper()
	for _, f := range strings.Fields(s) {
		f = strings.Trim(f, `'"`)
		if i := strings.Index(f, "tc://"); i >= 0 {
			if inv, err := proto.ParseInvite(f[i:]); err == nil && inv.Token != "" {
				return inv.Token
			}
		}
	}
	t.Fatalf("no invite in %q", s)
	return ""
}
