package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/transport"
)

// tc join (and the curl | sh install command, which runs it) redeems the
// invite in the handshake: the server creates the user, registers the key
// and spends the use in one transaction, then sends WELCOME. If the joiner
// never gets to write its config after that (WELCOME lost to a network
// drop, the 60 s join timeout, Ctrl-C, a crash), re-running the same
// command is refused: invite_invalid ("used or expired") for a single-use
// invite, key_exists ("log in without an invite") if uses are left. There
// is no way to log in without an invite on a machine with no config: tc
// join/login both need one. The account exists and can't be used.
// A single-use key-bound invite (--key github:…) makes this a dead end:
// the invitee must ask for a new invite and use a different key.
func TestJoinRetryAfterLostWelcome_Break(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	s.Server.SetPublicAddr(addr)
	home := t.TempDir()
	keyPath := writeKey(t, home)
	signer, err := identity.Load(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := s.CreateInviteOpts(context.Background(), server.InviteOpts{
		Keys: [][]byte{signer.PublicKey().Marshal()}, KeySrc: "github:carol"})
	if err != nil {
		t.Fatal(err)
	}
	pinv, err := proto.ParseInvite(inv.Code)
	if err != nil {
		t.Fatal(err)
	}

	// First attempt: the server redeems, the WELCOME arrives, and then the
	// joining process dies before it writes TC_HOME (same as a lost WELCOME).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := transport.DialTCP(ctx, pinv.Addr, pinv.Pin)
	if err != nil {
		t.Fatal(err)
	}
	codec := proto.NewCodec(conn)
	exp, _ := conn.Exporter()
	sig, err := identity.Sign(signer, exp)
	if err != nil {
		t.Fatal(err)
	}
	codec.Write(proto.THello, proto.Hello{Client: "tc", Auth: proto.Auth{Pubkey: signer.PublicKey().Marshal(), Sig: sig,
		Invite: pinv.Token, Name: "carol"}})
	f, err := codec.Read()
	if err != nil || f.Type != proto.TWelcome {
		t.Fatalf("first attempt: %v %v", f.Type, err)
	}
	conn.Close()

	// The user re-runs the same install command / tc join.
	if _, err := client.Login(ctx, home, inv.Code, "carol", keyPath); err != nil {
		t.Fatalf("re-running tc join after an interrupted first attempt: %v", err)
	}
}
