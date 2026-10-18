package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/proto"
)

// bigSKKey is a wire-format sk-ssh-ed25519 public key whose application
// string makes it longer than proto.MaxInviteKeyLen.
func bigSKKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wire := ssh.Marshal(struct {
		Name string
		Key  []byte
		App  string
	}{ssh.KeyAlgoSKED25519, pub, "ssh:" + strings.Repeat("a", proto.MaxInviteKeyLen)})
	pk, err := ssh.ParsePublicKey(wire)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

// PROTOCOL.md §4.1: the client "keeps the keys that pass the rules above",
// and the rules include "each at most 4096 bytes". A GitHub account with one
// ordinary ed25519 key and one oversized security key must yield only the
// ed25519 key; otherwise the server rejects the whole invite.create with
// bad_request and `tc invite --key github:<login>` can never work for that
// account.
func TestFetchGitHubInviteKeys_BreakOversizedKeyKept(t *testing.T) {
	ed := newSSH(t)
	big := bigSKKey(t)
	srv := githubServer(t, map[string]string{
		"alice": authorized(ed) + string(ssh.MarshalAuthorizedKey(big)),
	})
	keys, err := FetchGitHubInviteKeys(context.Background(), srv.URL, "alice")
	if err != nil {
		t.Fatal(err)
	}
	for i, k := range keys {
		if len(k) > proto.MaxInviteKeyLen {
			t.Errorf("key %d is %d bytes (> %d): the server will refuse the whole invite", i, len(k), proto.MaxInviteKeyLen)
		}
	}
	if len(keys) != 1 || string(keys[0]) != string(ed.PublicKey().Marshal()) {
		t.Fatalf("got %d keys, want just the ed25519 one", len(keys))
	}
}
