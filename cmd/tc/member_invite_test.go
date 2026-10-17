package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/transport"
)

func TestParseInviteCmd(t *testing.T) {
	for _, c := range []struct {
		args []string
		want inviteCmd
	}{
		{nil, inviteCmd{}},
		{[]string{"--uses", "5", "--ttl", "72h"}, inviteCmd{uses: 5, ttl: 72 * time.Hour}},
		{[]string{"--key", "github:alice"}, inviteCmd{key: "alice"}},
		{[]string{"--key", "github:alice", "--uses", "1", "--ttl", "1h", "--raw"}, inviteCmd{key: "alice", uses: 1, ttl: time.Hour, raw: true}},
		{[]string{"--list"}, inviteCmd{list: true}},
		{[]string{"--revoke", "0123456789AB"}, inviteCmd{revoke: "0123456789ab"}},
	} {
		got, err := parseInviteCmd(c.args)
		if err != nil || got != c.want {
			t.Errorf("%v: %+v %v", c.args, got, err)
		}
	}
	for _, args := range [][]string{
		{"--uses", "0"}, {"--uses", "101"}, {"--ttl", "10s"}, {"--ttl", "721h"},
		{"--key", "alice"}, {"--key", "github:"}, {"--key", "github:a b"}, {"--key", "gitlab:x"},
		{"--key", "github:alice", "--uses", "2"}, {"--list", "--key", "github:x"},
		{"--list", "--revoke", "0123456789ab"}, {"--revoke", "xyz"}, {"extra"},
	} {
		if _, err := parseInviteCmd(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestParseServerInviteKey(t *testing.T) {
	c, err := parseServerInvite([]string{"--key", "github:Alice-1", "--admin"})
	if err != nil || c.key != "Alice-1" || !c.opts.Admin {
		t.Fatalf("%+v %v", c, err)
	}
	for _, args := range [][]string{
		{"--key", "alice"}, {"--key", "github:-x"}, {"--key", "github:alice", "--uses", "3"}, {"--list", "--key", "github:x"},
	} {
		if _, err := parseServerInvite(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestReadMemberInvites(t *testing.T) {
	dir := t.TempDir()
	if !readMemberInvites(dir) {
		t.Fatal("default is on")
	}
	os.WriteFile(filepath.Join(dir, memberInvitesFile), []byte("false"), 0o600)
	if readMemberInvites(dir) {
		t.Fatal("false not read")
	}
	os.WriteFile(filepath.Join(dir, memberInvitesFile), []byte("true\n"), 0o600)
	if !readMemberInvites(dir) {
		t.Fatal("true not read")
	}
}

// tc invite as a member against a real server: a team invite, a key-bound
// one via a fake github.com, the member's own list, and a revoke.
func TestRunInviteMember(t *testing.T) {
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

	home := loginHome(t, srv, "bob")
	c, err := client.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { c.Run(ctx); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped; c.Close() })
	until(t, "online", func() bool { return c.Status().State == client.Online })

	carol, err := identity.GenerateKey(filepath.Join(t.TempDir(), "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(ssh.MarshalAuthorizedKey(carol.PublicKey()))
	}))
	defer gh.Close()
	testGitHubBase = gh.URL
	t.Cleanup(func() { testGitHubBase = "" })

	var out bytes.Buffer
	if err := runInvite(ctx, c, inviteCmd{uses: 3}, &out); err != nil || !strings.Contains(out.String(), "3 uses") {
		t.Fatalf("team invite: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := runInvite(ctx, c, inviteCmd{key: "carol", raw: true}, &out); err != nil || !strings.HasPrefix(out.String(), "tc://") {
		t.Fatalf("key invite: %v\n%s", err, out.String())
	}
	code := strings.TrimSpace(out.String())
	out.Reset()
	if err := runInvite(ctx, c, inviteCmd{list: true}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "github:carol, 1 key") || !strings.Contains(s, "@bob") || !strings.Contains(s, "tc invite --revoke") {
		t.Fatalf("list:\n%s", s)
	}
	inv, _ := proto.ParseInvite(code)
	invs, _ := srv.ListInvites(ctx)
	var id string
	for _, i := range invs {
		if i.Keys == 1 {
			id = i.ID
		}
	}
	if id == "" || strings.Contains(out.String(), inv.Token) {
		t.Fatalf("listed %+v", invs)
	}
	out.Reset()
	if err := runInvite(ctx, c, inviteCmd{revoke: id}, &out); err != nil || !strings.Contains(out.String(), "Revoked") {
		t.Fatalf("revoke: %v %s", err, out.String())
	}
	if err := runInvite(ctx, c, inviteCmd{uses: 11}, &out); err == nil {
		t.Fatal("member made an 11-use invite")
	}
}
