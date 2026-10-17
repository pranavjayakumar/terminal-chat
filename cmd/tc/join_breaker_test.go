package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/transport"
)

func joinBreakServer(t *testing.T) *server.Server {
	t.Helper()
	t.Setenv("TC_TRANSPORT", "tcp")
	srv, err := server.New(server.Config{DataDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MemberInvites: true})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.Listen("127.0.0.1:0", srv.Cert())
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

func writePrivKey(t *testing.T, path string, priv crypto.PrivateKey) ssh.PublicKey {
	t.Helper()
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s.PublicKey()
}

// The point of `--key github:<login>` invites is one-step onboarding: the
// invitee pastes the install command and it joins. But the join wizard
// picks exactly one key (agent ed25519, else ~/.ssh/id_ed25519, else a
// freshly generated tc key) without looking at what the invite is bound
// to, so an invitee whose GitHub key is RSA or ECDSA (accepted by the
// spec, §4.1) or isn't the first ed25519 in their agent is refused with
// invite_invalid, and is left with a stray generated key in TC_HOME.
func TestJoinKeyBoundInvite_BreakPicksWrongKey(t *testing.T) {
	rsaPriv, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Fatal(err)
	}
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, env joinEnv) ssh.PublicKey // returns the key on GitHub
	}{
		{"rsa id_rsa only", func(t *testing.T, env joinEnv) ssh.PublicKey {
			return writePrivKey(t, filepath.Join(env.sshDir, "id_rsa"), rsaPriv)
		}},
		{"ecdsa id_ecdsa next to unrelated id_ed25519", func(t *testing.T, env joinEnv) ssh.PublicKey {
			writeKey(t, filepath.Join(env.sshDir, "id_ed25519")) // e.g. a work key not on GitHub
			return writePrivKey(t, filepath.Join(env.sshDir, "id_ecdsa"), ecPriv)
		}},
		{"second ed25519 in agent", func(t *testing.T, env joinEnv) ssh.PublicKey {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			_, gh, _ := ed25519.GenerateKey(rand.Reader)
			serveAgent(t, other, gh)
			pk, _ := ssh.NewPublicKey(gh.Public())
			return pk
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := joinBreakServer(t)
			env, _, out := testJoinEnv(t, noRead{t}, false)
			env.login = client.Login
			pk := tc.setup(t, env)
			inv, err := srv.CreateInviteOpts(context.Background(), server.InviteOpts{Keys: [][]byte{pk.Marshal()}, KeySrc: "github:ada"})
			if err != nil {
				t.Fatal(err)
			}
			if err := runJoin(context.Background(), env, inv.Code, "ada", ""); err != nil {
				t.Fatalf("joining with a key-bound invite whose key the invitee holds: %v\n%s", err, out)
			}
		})
	}
}
