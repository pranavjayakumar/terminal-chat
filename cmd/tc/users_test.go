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

func TestParseUsers(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want usersArgs
		ok   bool
	}{
		{nil, usersArgs{}, true},
		{[]string{"list"}, usersArgs{}, true},
		{[]string{"disable", "@bob"}, usersArgs{disable: "bob"}, true},
		{[]string{"disable", "@bob", "--yes"}, usersArgs{disable: "bob", yes: true}, true},
		{[]string{"disable"}, usersArgs{}, false},
		{[]string{"disable", "bob"}, usersArgs{disable: "bob"}, true}, // the @ is optional
		{[]string{"disable", "bob", "--yes"}, usersArgs{disable: "bob", yes: true}, true},
		{[]string{"disable", "Bob!"}, usersArgs{}, false},
		{[]string{"disable", "@bob", "@carol"}, usersArgs{}, false},
		{[]string{"@bob"}, usersArgs{}, false},
		{[]string{"enable", "@bob"}, usersArgs{}, false},
	} {
		got, err := parseUsers(tc.args)
		if (err == nil) != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("parseUsers(%q) = %+v, %v", tc.args, got, err)
		}
	}
}

type fakeUsers struct {
	me       proto.User
	res      proto.UsersRes
	disabled []uint64
}

func (f *fakeUsers) Me() proto.User                                    { return f.me }
func (f *fakeUsers) ListUsers(context.Context) (proto.UsersRes, error) { return f.res, nil }
func (f *fakeUsers) DisableUser(_ context.Context, u uint64) error {
	f.disabled = append(f.disabled, u)
	return nil
}

func TestUsersFake(t *testing.T) {
	res := proto.UsersRes{
		Users: []proto.User{{ID: 1, Name: "alice", Admin: true}, {ID: 2, Name: "bob"}, {ID: 3, Name: "carol"},
			{ID: 4, Name: "eve\x1b[31m"}, {ID: 5, Name: "gone", Disabled: true}},
		InvitedBy: map[uint64]uint64{1: 0, 2: 1, 3: 2, 4: 2},
	}
	admin := &fakeUsers{me: res.Users[0], res: res}
	var out bytes.Buffer
	if err := users(context.Background(), admin, nil, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"INVITED BY", "server host", "@bob", "disabled"} {
		if !strings.Contains(s, want) {
			t.Fatalf("list lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "\x1b") {
		t.Fatalf("unsanitized name:\n%q", s)
	}

	// Disabling asks first; anything but y keeps the user.
	out.Reset()
	if err := users(context.Background(), admin, []string{"disable", "@bob"}, strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	if len(admin.disabled) != 0 || !strings.Contains(out.String(), "not disabled") {
		t.Fatalf("disabled without a yes: %v\n%s", admin.disabled, out.String())
	}
	out.Reset()
	if err := users(context.Background(), admin, []string{"disable", "@bob"}, strings.NewReader("y\n"), &out); err != nil {
		t.Fatal(err)
	}
	if len(admin.disabled) != 1 || admin.disabled[0] != 2 {
		t.Fatalf("disabled %v", admin.disabled)
	}
	if s := out.String(); !strings.Contains(s, "✓ disabled @bob") || !strings.Contains(s, "They invited 2 accounts: @carol, @eve") {
		t.Fatalf("disable output:\n%s", s)
	}
	for _, args := range [][]string{{"disable", "@alice"}, {"disable", "@nobody"}} {
		if err := users(context.Background(), admin, args, strings.NewReader("y\n"), io.Discard); err == nil {
			t.Fatalf("%v: no error", args)
		}
	}
	out.Reset()
	if err := users(context.Background(), admin, []string{"disable", "@gone", "--yes"}, nil, &out); err != nil ||
		!strings.Contains(out.String(), "already disabled") {
		t.Fatalf("already disabled: %v %s", err, out.String())
	}

	// A member sees no inviters and can't disable.
	member := &fakeUsers{me: res.Users[1], res: proto.UsersRes{Users: res.Users}}
	out.Reset()
	if err := users(context.Background(), member, nil, nil, &out); err != nil || strings.Contains(out.String(), "INVITED BY") {
		t.Fatalf("member list: %v\n%s", err, out.String())
	}
	if err := users(context.Background(), member, []string{"disable", "@carol", "--yes"}, nil, io.Discard); err == nil ||
		len(member.disabled) != 0 {
		t.Fatal("a member disabled someone")
	}
}

// loginAs logs name in with code and returns its TC_HOME and user.
func loginAs(t *testing.T, code, name string) (string, proto.User) {
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
	if err != nil {
		t.Fatal(err)
	}
	return home, u
}

// tc users against the real server: the admin sees that carol came in on
// bob's member invite, disables bob, and is pointed at carol.
func TestUsersRealServer(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	srv, err := server.New(server.Config{DataDir: t.TempDir(), MemberInvites: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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

	bg := context.Background()
	code, err := srv.CreateInvite(bg, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	adminHome, _ := loginAs(t, code, "alice")
	code, _ = srv.CreateInvite(bg, false, 0)
	bobHome, bob := loginAs(t, code, "bob")
	inv, err := srv.CreateInviteOpts(bg, server.InviteOpts{CreatedBy: bob.ID, Member: true})
	if err != nil {
		t.Fatal(err)
	}
	loginAs(t, inv.Code, "carol")

	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	t.Setenv("TC_HOME", bobHome)
	bc, err := connected(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := users(ctx, bc, nil, nil, &out); err != nil || strings.Contains(out.String(), "INVITED BY") ||
		!strings.Contains(out.String(), "@carol") {
		t.Fatalf("bob's list: %v\n%s", err, out.String())
	}
	bc.Close()

	t.Setenv("TC_HOME", adminHome)
	c, err := connected(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out.Reset()
	if err := users(ctx, c, nil, nil, &out); err != nil {
		t.Fatal(err)
	}
	carolLine := ""
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "@carol ") {
			carolLine = l
		}
	}
	if !strings.HasSuffix(carolLine, "@bob") {
		t.Fatalf("carol's inviter not shown:\n%s", out.String())
	}
	out.Reset()
	if err := users(ctx, c, []string{"disable", "@bob", "--yes"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "They invited 1 account: @carol") {
		t.Fatalf("disable output:\n%s", out.String())
	}
	res, err := c.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range res.Users {
		if u.Name == "bob" && !u.Disabled {
			t.Fatal("bob not disabled")
		}
	}
}

// Without a terminal (in == nil) disable never prompts: it says to pass --yes
// and fails, instead of printing a [y/N] nobody can answer and exiting 0.
func TestUsersDisableNoTerminal(t *testing.T) {
	res := proto.UsersRes{Users: []proto.User{{ID: 1, Name: "alice", Admin: true}, {ID: 2, Name: "bob"}}}
	admin := &fakeUsers{me: res.Users[0], res: res}
	var out bytes.Buffer
	err := users(context.Background(), admin, []string{"disable", "bob"}, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "--yes") || strings.Contains(out.String(), "[y/N]") {
		t.Fatalf("err %v, out %q", err, out.String())
	}
}

// /dev/null is a character device but not a terminal.
func TestDevNullIsNotATerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Fatal("/dev/null counted as a terminal")
	}
}
