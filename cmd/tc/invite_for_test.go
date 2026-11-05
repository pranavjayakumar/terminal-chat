package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/transport"
)

func TestParseInviteFor(t *testing.T) {
	for _, c := range []struct {
		args []string
		want inviteCmd
	}{
		{[]string{"--for", "@alice"}, inviteCmd{forUser: "alice"}},
		{[]string{"--for", "Bob"}, inviteCmd{forUser: "bob"}},
		{[]string{"--for", "@me", "--raw"}, inviteCmd{forUser: "me", raw: true}},
	} {
		got, err := parseInviteCmd(c.args)
		if err != nil || got != c.want {
			t.Errorf("%v: %+v %v", c.args, got, err)
		}
	}
	for _, args := range [][]string{
		{"--for"}, {"--for", ""}, {"--for", "@"}, {"--for", "a b"}, {"--for", "@x;rm"},
		{"--for", "@alice", "--uses", "2"}, {"--for", "@alice", "--ttl", "1h"}, {"--for", "@alice", "--key", "github:alice"},
		{"--for", "@alice", "--list"}, {"--for", "@alice", "--revoke", "0123456789ab"}, {"--for", "@alice", "extra"},
	} {
		if _, err := parseInviteCmd(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestPrintAddKeyInvite(t *testing.T) {
	code := proto.FormatInvite("203.0.113.7:7443", "abcdefghijklmnopqrstuvwxyz", "Pin-_x")
	var b bytes.Buffer
	printAddKeyInvite(&b, code, "alice", "v0.2.0")
	out := b.String()
	for _, want := range []string{"Add-key invite for @alice", "own SSH key", "| sh -s -- '" + code + "'",
		"tc join '" + code + "' --name alice", "tc devices endorse"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	b.Reset()
	printAddKeyInvite(&b, "tc://h:7443/tok#pin'; id; '", "alice", "dev")
	if strings.Contains(b.String(), "tc join '") || strings.Contains(b.String(), "| sh") {
		t.Fatalf("unsafe code printed as a command:\n%s", b.String())
	}
}

// keyHome logs a fresh key into a new TC_HOME with code (name for new users).
func keyHome(t *testing.T, code, name string) (string, proto.User, error) {
	t.Helper()
	home := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(home, "id_ed25519")
	if err := os.WriteFile(key, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u, err := client.Login(ctx, home, code, name, key)
	return home, u, err
}

// tc invite --for @bob against a real server (#59): an admin's add-key
// invite signs a second computer, with a key of its own, in as bob; a
// member is refused with a hint, and unknown users never reach the server.
func TestRunInviteFor(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	srv, err := server.New(server.Config{DataDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MemberInvites: true})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.ListenWith("127.0.0.1:0", srv.Cert(), transport.ListenOpts{})
	if err != nil {
		t.Fatal(err)
	}
	srv.SetPublicAddr(ln.Addr().String())
	sctx, scancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Serve(sctx, ln); close(done) }()
	t.Cleanup(func() { scancel(); <-done; srv.Close() })

	adminCode, err := srv.CreateInvite(context.Background(), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	adminHome, _, err := keyHome(t, adminCode, "ada")
	if err != nil {
		t.Fatal(err)
	}
	bobHome := loginHome(t, srv, "bob")
	open := func(home string) *client.Client {
		c, err := client.Open(home)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		stopped := make(chan struct{})
		go func() { c.Run(ctx); close(stopped) }()
		t.Cleanup(func() { cancel(); <-stopped; c.Close() })
		until(t, "online", func() bool { return c.Status().State == client.Online })
		return c
	}
	admin, bob := open(adminHome), open(bobHome)
	until(t, "bob known", func() bool {
		for _, u := range admin.Users() {
			if u.Name == "bob" {
				return true
			}
		}
		return false
	})
	ctx := context.Background()

	var out bytes.Buffer
	if err := runInvite(ctx, admin, inviteCmd{forUser: "bob"}, &out); err != nil || !strings.Contains(out.String(), "Add-key invite for @bob") {
		t.Fatalf("add-key invite: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := runInvite(ctx, admin, inviteCmd{forUser: "bob", raw: true}, &out); err != nil {
		t.Fatal(err)
	}
	code := strings.TrimSpace(out.String())
	// The second computer's own key joins bob's account; the name is ignored.
	_, u, err := keyHome(t, code, "whatever")
	if err != nil || u.ID != bob.Me().ID || u.Name != "bob" {
		t.Fatalf("redeemed as %+v (bob is %d): %v", u, bob.Me().ID, err)
	}
	// Single use.
	if _, _, err := keyHome(t, code, ""); err == nil {
		t.Fatal("add-key invite redeemed twice")
	}
	out.Reset()
	if err := runInvite(ctx, admin, inviteCmd{list: true}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "key for @bob") {
		t.Fatalf("list after first invite (one still open):\n%s", out.String())
	}

	if err := runInvite(ctx, admin, inviteCmd{forUser: "nobody"}, &out); err == nil || !strings.Contains(err.Error(), "no user @nobody") {
		t.Fatalf("unknown user: %v", err)
	}
	// Members can't, and are told who can.
	err = runInvite(ctx, bob, inviteCmd{forUser: "me"}, &out)
	if err == nil || !strings.Contains(err.Error(), "only admins") || !strings.Contains(err.Error(), "tc invite --for @bob") {
		t.Fatalf("member: %v", err)
	}
}
