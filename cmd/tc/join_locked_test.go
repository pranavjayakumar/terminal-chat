package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// A passphrase-protected ~/.ssh key that isn't in ssh-agent is not "no SSH
// key": say which key tc couldn't use and how to use it.
func TestJoinNamesLockedKey(t *testing.T) {
	env, _, out := testJoinEnv(t, noRead{t}, false)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(env.sshDir, "id_ed25519")
	os.MkdirAll(env.sshDir, 0o700)
	if err := os.WriteFile(p, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	pk, _ := ssh.NewPublicKey(priv.Public())
	os.WriteFile(p+".pub", ssh.MarshalAuthorizedKey(pk), 0o644)
	if err := runJoin(context.Background(), env, testInvite, "ada", ""); err != nil {
		t.Fatal(err)
	}
	o := out.String()
	if strings.Contains(o, "No SSH key found") || !strings.Contains(o, "can't use your SSH key "+p) || !strings.Contains(o, "ssh-add "+p) {
		t.Fatalf("output:\n%s", o)
	}
}
