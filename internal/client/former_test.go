package client

import (
	"crypto/rand"
	"testing"

	"github.com/skailama/tc/internal/e2ecrypto"
)

// A former id counts only with its own keys: a cert reusing one of our old
// (public) ids with other keys is a new device, not this one's past.
func TestIsFormerNeedsKeys(t *testing.T) {
	k, err := e2ecrypto.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, err := e2ecrypto.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	former := []formerDev{{k.ID, k.X25519Pub, k.Ed25519Pub}}
	if !isFormer(former, k.ID, k.X25519Pub, k.Ed25519Pub) {
		t.Fatal("own former keys not recognized")
	}
	if isFormer(former, k.ID, other.X25519Pub, other.Ed25519Pub) {
		t.Fatal("old id with other keys counted as former")
	}
	if isFormer(former, k.ID, k.X25519Pub, other.Ed25519Pub) || isFormer(former, other.ID, k.X25519Pub, k.Ed25519Pub) {
		t.Fatal("partial match counted as former")
	}
}
