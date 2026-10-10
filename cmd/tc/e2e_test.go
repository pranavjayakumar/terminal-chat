package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/transport"
)

// syncBuf is a goroutine-safe output buffer.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// loginHome logs name in on s and returns its TC_HOME.
func loginHome(t *testing.T, s *server.Server, name string) string {
	t.Helper()
	code, err := s.CreateInvite(context.Background(), false, 0)
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := client.Login(ctx, home, code, name, key); err != nil {
		t.Fatal(err)
	}
	return home
}

// tc send and tc notify into a DM (encrypted) and tc tail on the other
// side all go through the client's encryption: the server only ever sees
// enc, and tail prints the decrypted text.
func TestSendNotifyTailEncrypted(t *testing.T) {
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

	aliceHome := loginHome(t, srv, "alice")
	bobHome := loginHome(t, srv, "bob")
	bob, err := client.Open(bobHome)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go bob.Run(ctx)
	t.Cleanup(func() { cancel(); bob.Close() })
	until(t, "bob's device registered", func() bool {
		sid, ok := bob.ServerID()
		if !ok {
			return false
		}
		k, err := bob.Trust().DeviceKeys(sid, bob.Me().ID)
		if err != nil {
			return false
		}
		_, ok, _ = bob.Trust().Device(sid, k.ID)
		return ok
	})

	t.Setenv("TC_HOME", aliceHome)
	if err := cmdSend([]string{"@bob", "the", "secret", "plan"}); err != nil {
		t.Fatalf("tc send: %v", err)
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer pcancel()
	if err := postTo(pctx, "@bob", "✓ `make` · 1s"); err != nil {
		t.Fatalf("tc notify: %v", err)
	}

	var dm uint64
	until(t, "the DM at bob", func() bool {
		for _, v := range bob.Channels() {
			if v.Kind == "dm" && len(v.Members) == 2 {
				dm = v.ID
				return v.E2E
			}
		}
		return false
	})
	out := &syncBuf{}
	tctx, tcancel := context.WithCancel(context.Background())
	tdone := make(chan error, 1)
	go func() { tdone <- tail(tctx, bob, dm, out) }()
	until(t, "tail prints both messages decrypted", func() bool {
		s := out.String()
		return strings.Contains(s, "alice: the secret plan") && strings.Contains(s, "alice: ✓ `make` · 1s")
	})
	tcancel()
	if err := <-tdone; err != nil {
		t.Fatal(err)
	}
	if s := out.String(); strings.Count(s, "the secret plan") != 1 || strings.Contains(s, "waiting for key") {
		t.Fatalf("tail output:\n%s", s)
	}
	for _, m := range bob.Messages(dm, 10) {
		if m.Sec != client.SecE2E {
			t.Fatalf("message %+v not end-to-end encrypted", m)
		}
	}
}

// Two real clients compute the same safety number for each other, and
// tc verify shows it and, confirmed, marks the other user's key verified;
// declining changes nothing.
func TestVerifySafetyNumberSymmetric(t *testing.T) {
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

	homes := map[string]string{"alice": loginHome(t, srv, "alice"), "bob": loginHome(t, srv, "bob")}
	clients := map[string]*client.Client{}
	for name, home := range homes {
		c, err := client.Open(home)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		stopped := make(chan struct{})
		go func() { c.Run(ctx); close(stopped) }()
		clients[name] = c
		t.Cleanup(func() { cancel(); <-stopped; c.Close() })
	}
	id := func(c *client.Client, name string) uint64 {
		for _, u := range c.Users() {
			if u.Name == name {
				return u.ID
			}
		}
		return 0
	}
	var na, nb client.SafetyNumber
	until(t, "both sides see each other's key", func() bool {
		var ea, eb error
		na, ea = clients["alice"].SafetyNumber(id(clients["alice"], "bob"))
		nb, eb = clients["bob"].SafetyNumber(id(clients["bob"], "alice"))
		return ea == nil && eb == nil
	})
	if na.Digits != nb.Digits || len(na.Digits) != 60 {
		t.Fatalf("alice sees %s, bob sees %s", na.Digits, nb.Digits)
	}

	t.Setenv("TC_HOME", homes["alice"])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := verify(ctx, []string{"@bob"}, strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), strings.Join(na.Groups()[:4], " ")) || !strings.Contains(out.String(), "not verified") {
		t.Fatalf("output:\n%s", out.String())
	}
	sid, _ := clients["alice"].ServerID()
	bobID := id(clients["alice"], "bob")
	verified := func() bool {
		ks, _ := clients["alice"].Trust().PinnedKeys(sid, bobID)
		return len(ks) == 1 && ks[0].How == "verified"
	}
	if verified() {
		t.Fatal("declined but verified")
	}
	out.Reset()
	if err := verify(ctx, []string{"@bob"}, strings.NewReader("y\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "✓ @bob verified") || !verified() {
		t.Fatalf("not verified; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "pinned different key sets") {
		t.Errorf("no key-set hint:\n%s", out.String())
	}

	// --github: the comparison, then y to verify because the account is bob's.
	ks, _ := clients["alice"].Trust().PinnedKeys(sid, bobID)
	pk, err := ssh.ParsePublicKey(ks[0].Wire)
	if err != nil {
		t.Fatal(err)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bob-gh.keys" {
			w.Write(ssh.MarshalAuthorizedKey(pk))
			return
		}
		http.NotFound(w, r)
	}))
	defer gh.Close()
	testGitHubBase = gh.URL
	defer func() { testGitHubBase = "" }()
	ghAudits := func() int {
		n := 0
		a, _ := clients["alice"].Trust().Audit(sid)
		for _, e := range a {
			if e.Detail == "github:bob-gh" {
				n++
			}
		}
		return n
	}
	out.Reset()
	if err := verify(ctx, []string{"@bob", "--github", "bob-gh"}, strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	if ghAudits() != 0 || !strings.Contains(out.String(), "Verify @bob because you confirmed github.com/bob-gh is theirs?") {
		t.Fatalf("declined GitHub verified, or no question:\n%s", out.String())
	}
	out.Reset()
	if err := verify(ctx, []string{"@bob", "--github", "bob-gh"}, strings.NewReader("y\n"), &out); err != nil {
		t.Fatal(err)
	}
	if ghAudits() != 1 {
		t.Fatalf("GitHub confirmation not recorded:\n%s", out.String())
	}
	out.Reset()
	if err := verify(ctx, []string{"@bob", "--github", "nobody"}, strings.NewReader("y\n"), &out); err == nil {
		t.Fatal("404 account verified")
	}
	for _, bad := range [][]string{{}, {"bob"}, {"@bob", "--github"}, {"@bob", "@carol"}} {
		if err := verify(ctx, bad, strings.NewReader(""), &out); !errors.Is(err, errUsage) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
