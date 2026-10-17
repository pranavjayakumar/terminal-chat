package identity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"math/big"
	"testing"

	"golang.org/x/crypto/ssh"
)

func sshPub(t testing.TB, k any) []byte {
	t.Helper()
	pk, err := ssh.NewPublicKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pk.Marshal()
}

func TestInviteKey(t *testing.T) {
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	ec, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	r2048, _ := rsa.GenerateKey(rand.Reader, 2048)
	r1024, _ := rsa.GenerateKey(rand.Reader, 1024)
	for name, b := range map[string][]byte{
		"ed25519":  sshPub(t, edPub),
		"ecdsa":    sshPub(t, &ec.PublicKey),
		"rsa 2048": sshPub(t, &r2048.PublicKey),
	} {
		pk, err := InviteKey(b)
		if err != nil || !bytes.Equal(pk.Marshal(), b) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// ssh-dss: a well-formed key of a type tc refuses.
	dss := ssh.Marshal(struct {
		Name       string
		P, Q, G, Y *big.Int
	}{ssh.KeyAlgoDSA, big.NewInt(23), big.NewInt(11), big.NewInt(4), big.NewInt(8)})
	ed := sshPub(t, edPub)
	for name, b := range map[string][]byte{
		"rsa 1024":      sshPub(t, &r1024.PublicKey),
		"dsa":           dss,
		"trailing byte": append(append([]byte{}, ed...), 0),
		"truncated":     ed[:len(ed)-1],
		"empty":         nil,
		"garbage":       []byte("ssh-ed25519 AAAA"),
	} {
		if _, err := InviteKey(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Whatever InviteKey accepts is canonical and of an accepted type.
func FuzzInviteKey(f *testing.F) {
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.Add(sshPub(f, edPub))
	f.Add(sshPub(f, &ec.PublicKey))
	f.Add([]byte{0, 0, 0, 7, 's', 's', 'h', '-', 'r', 's', 'a'})
	f.Fuzz(func(t *testing.T, b []byte) {
		pk, err := InviteKey(b)
		if err != nil {
			return
		}
		if !bytes.Equal(pk.Marshal(), b) || !allowedKey[pk.Type()] {
			t.Fatalf("accepted %s %x", pk.Type(), b)
		}
	})
}
