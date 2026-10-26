package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
)

func testSigner(t *testing.T) ssh.Signer {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// An agent endorsement verifies only as one: never as a KeyEndorse (which
// would give the agent key an owner key's powers), and the reverse.
func TestAgentEndorseIsNotAKeyEndorse(t *testing.T) {
	owner, agent := testSigner(t), testSigner(t)
	var sid e2ecrypto.ServerID
	ae, err := NewAgentEndorse(owner, sid, 7, agent.PublicKey(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e, err := ae.VerifyAgentEndorse(sid, owner.PublicKey().Marshal())
	if err != nil || e.User != 7 || string(e.AgentSSH) != string(agent.PublicKey().Marshal()) {
		t.Fatalf("verify: %+v %v", e, err)
	}
	if _, err := ae.VerifyKeyEndorse(sid, owner.PublicKey().Marshal()); err == nil {
		t.Fatal("an agent endorsement verified as a key endorsement")
	}
	if _, err := PeekKeyEndorse(ae); err == nil {
		t.Fatal("an agent endorsement decoded as a key endorsement")
	}
	ke, err := NewKeyEndorse(owner, sid, 7, agent.PublicKey(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ke.VerifyAgentEndorse(sid, owner.PublicKey().Marshal()); err == nil {
		t.Fatal("a key endorsement verified as an agent endorsement")
	}
	if _, err := ae.VerifyAgentEndorse(sid, agent.PublicKey().Marshal()); err == nil {
		t.Fatal("verified under the wrong key")
	}
}
