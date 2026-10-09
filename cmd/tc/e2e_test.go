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
