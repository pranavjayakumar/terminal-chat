package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
)

// realJoinEnv is testJoinEnv against a real server.
func realJoinEnv(t *testing.T) (joinEnv, *bytes.Buffer) {
	env, _, out := testJoinEnv(t, noRead{t}, false)
	env.login = client.Login
	return env, out
}

// A second computer (B6): same SSH key, a fresh TC_HOME. `tc login
// tc://host:port#pin` signs in to the existing account without an invite,
// and so does re-running the original (now spent) install command.
func TestLoginSecondComputer(t *testing.T) {
	srv := joinBreakServer(t)
	inv, err := srv.CreateInviteOpts(context.Background(), server.InviteOpts{})
	if err != nil {
		t.Fatal(err)
	}
	first, out := realJoinEnv(t)
	writeKey(t, filepath.Join(first.sshDir, "id_ed25519"))
	if err := runJoin(context.Background(), first, inv.Code, "ada", ""); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	c, err := client.Open(first.home)
	if err != nil {
		t.Fatal(err)
	}
	cfg := c.Config()
	c.Close()

	// tc login with no argument prints the line to use elsewhere.
	var show bytes.Buffer
	if err := showLogin(&show, first.home); err != nil {
		t.Fatal(err)
	}
	line := proto.FormatServer(cfg.Addr, cfg.Pin)
	if !strings.Contains(show.String(), "tc login '"+line+"'") || !strings.Contains(show.String(), "as ada") {
		t.Fatalf("show:\n%s", show.String())
	}
	if err := showLogin(&show, filepath.Join(t.TempDir(), "none")); err == nil || !strings.Contains(err.Error(), "tc join") {
		t.Fatalf("showLogin without an account: %v", err)
	}

	for _, code := range []string{line, inv.Code} {
		second, out := realJoinEnv(t)
		second.sshDir = first.sshDir
		if err := runJoin(context.Background(), second, code, "", ""); err != nil {
			t.Fatalf("%s: %v\n%s", code, err, out)
		}
		c, err := client.Open(second.home)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Config(); got.UserID != cfg.UserID || got.Name != "ada" || got.Addr != cfg.Addr {
			t.Fatalf("%s: config %+v", code, got)
		}
		c.Close()
		if !strings.Contains(out.String(), "You're in as ada") {
			t.Fatalf("%s: output\n%s", code, out)
		}
	}

	// An unregistered key: a clear error, and no key is generated.
	other, _ := realJoinEnv(t)
	writeKey(t, filepath.Join(other.sshDir, "id_ed25519"))
	err = runJoin(context.Background(), other, line, "", "")
	if err == nil || !strings.Contains(err.Error(), "none of your SSH keys has an account") || !strings.Contains(err.Error(), "id_ed25519") {
		t.Fatalf("unknown key: %v", err)
	}
	empty, _ := realJoinEnv(t)
	if err := runJoin(context.Background(), empty, line, "", ""); err == nil || !strings.Contains(err.Error(), "no keys found") {
		t.Fatalf("no keys: %v", err)
	}
	if _, err := os.Stat(filepath.Join(empty.home, "id_ed25519")); !os.IsNotExist(err) {
		t.Fatal("tc login generated a key")
	}
}

// Key-bound invites never generate a stray key, say which key would work
// when none does, and name a locked matching key so the user can ssh-add it.
func TestJoinKeyBoundErrors(t *testing.T) {
	srv := joinBreakServer(t)
	_, gh, _ := ed25519.GenerateKey(rand.Reader)
	ghPub, _ := ssh.NewPublicKey(gh.Public())
	mk := func() string {
		inv, err := srv.CreateInviteOpts(context.Background(), server.InviteOpts{Keys: [][]byte{ghPub.Marshal()}, KeySrc: "github:ada"})
		if err != nil {
			t.Fatal(err)
		}
		return inv.Code
	}

	// No keys at all: the generated key isn't bound, so it's removed again.
	env, _ := realJoinEnv(t)
	code := mk()
	err := runJoin(context.Background(), env, code, "ada", "")
	if err == nil || !strings.Contains(err.Error(), "particular SSH keys") || !strings.Contains(err.Error(), "SHA256:") {
		t.Fatalf("no keys: %v", err)
	}
	if ents, _ := os.ReadDir(env.home); len(ents) != 0 {
		t.Fatalf("left files in TC_HOME: %v", ents)
	}

	// The matching key is on disk but passphrase-protected.
	env, _ = realJoinEnv(t)
	writeKey(t, filepath.Join(env.sshDir, "id_ed25519"))
	blk, _ := ssh.MarshalPrivateKeyWithPassphrase(gh, "", []byte("pw"))
	os.WriteFile(filepath.Join(env.sshDir, "id_ecdsa"), pem.EncodeToMemory(blk), 0o600) // any id_* name
	os.WriteFile(filepath.Join(env.sshDir, "id_ecdsa.pub"), ssh.MarshalAuthorizedKey(ghPub), 0o644)
	err = runJoin(context.Background(), env, code, "ada", "")
	if err == nil || !strings.Contains(err.Error(), "ssh-add") || !strings.Contains(err.Error(), "id_ecdsa") {
		t.Fatalf("locked key: %v", err)
	}

	// --key path.pub with the private half in the agent works.
	env, out := realJoinEnv(t)
	serveAgent(t, gh)
	pubPath := filepath.Join(t.TempDir(), "github.pub")
	os.WriteFile(pubPath, ssh.MarshalAuthorizedKey(ghPub), 0o644)
	if err := runJoin(context.Background(), env, code, "ada", pubPath); err != nil {
		t.Fatalf("--key .pub: %v\n%s", err, out)
	}
	c, err := client.Open(env.home)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	// Re-running the same command (WELCOME lost, say) signs in again.
	env2, out := realJoinEnv(t)
	serveAgent(t, gh) // realJoinEnv cleared SSH_AUTH_SOCK
	if err := runJoin(context.Background(), env2, code, "ada", pubPath); err != nil {
		t.Fatalf("retry: %v\n%s", err, out)
	}
}

// The saved config pins an agent key by fingerprint, so the client keeps
// using the key it joined with even when it isn't the agent's first.
func TestJoinPinsAgentKey(t *testing.T) {
	srv := joinBreakServer(t)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	_, gh, _ := ed25519.GenerateKey(rand.Reader)
	ghPub, _ := ssh.NewPublicKey(gh.Public())
	inv, err := srv.CreateInviteOpts(context.Background(), server.InviteOpts{Keys: [][]byte{ghPub.Marshal()}, KeySrc: "github:ada"})
	if err != nil {
		t.Fatal(err)
	}
	env, out := realJoinEnv(t)
	serveAgent(t, other, gh)
	if err := runJoin(context.Background(), env, inv.Code, "ada", ""); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	c, err := client.Open(env.home)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go c.Run(ctx)
	for c.Status().State != client.Online {
		if ctx.Err() != nil {
			t.Fatalf("never online: %+v (config %+v)", c.Status(), c.Config())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Without a key hint, tc login offers at most maxBlindKeys keys: a hostile
// server answering auth_failed to everything can't collect every key.
func TestLoginBlindKeyCap(t *testing.T) {
	env, f, _ := testJoinEnv(t, noRead{t}, false)
	var keys []any
	for range 6 {
		_, k, _ := ed25519.GenerateKey(rand.Reader)
		keys = append(keys, k)
	}
	serveAgent(t, keys...)
	for range 6 {
		f.errs = append(f.errs, &client.ByeError{Code: proto.ByeAuthFailed})
	}
	err := runJoin(context.Background(), env, "tc://127.0.0.1:7443#pin", "", "")
	if err == nil || len(f.calls) != maxBlindKeys {
		t.Fatalf("err %v, %d attempts", err, len(f.calls))
	}
}

// An invite used with a key that already has an account logs in to that
// account and grants nothing (an admin invite doesn't make it admin), so
// join says the invite wasn't used; a retry minutes after a lost WELCOME
// doesn't.
func TestJoinSaysInviteUnusedForExistingKey(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		note bool
	}{{time.Hour, true}, {time.Minute, false}} {
		env, _, out := testJoinEnv(t, noRead{t}, false)
		writeKey(t, filepath.Join(env.sshDir, "id_ed25519"))
		env.login = func(context.Context, string, string, string, string) (proto.User, error) {
			return proto.User{ID: 7, Name: "ada", Created: time.Now().Add(-tc.age).UnixMilli()}, nil
		}
		if err := runJoin(context.Background(), env, testInvite, "ada", ""); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(out.String(), "the invite was not used"); got != tc.note {
			t.Fatalf("age %v: note %v\n%s", tc.age, got, out)
		}
	}
}
