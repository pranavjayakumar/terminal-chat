package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/trust"
)

// A server can force a cache reset (and a re-login wipes the cache), but
// neither may touch e2e.db: forgetting pins, heads or flags is how a
// malicious server would roll back trust.
func TestCacheResetLeavesTrustStore(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	home := login(t, fs)
	c := open(t, home)
	if st, err := os.Stat(filepath.Join(home, trust.FileName)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("e2e.db after Open: %v %v", st, err)
	}

	sid := e2ecrypto.ServerID(serverID)
	ts := c.Trust()
	keys, err := ts.DeviceKeys(sid, alice)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := keys.MarshalBinary()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	pk, _ := ssh.NewPublicKey(pub)
	fp := ssh.FingerprintSHA256(pk)
	if err := ts.PinKey(sid, bob, fp, pk.Marshal(), trust.TOFU); err != nil {
		t.Fatal(err)
	}
	h := trust.Head{Epoch: 3, Hash: sha256.Sum256([]byte("b3"))}
	copy(h.EH[:], h.Hash[:])
	if err := ts.AdvanceChain(sid, dm, h); err != nil {
		t.Fatal(err)
	}
	if err := ts.MarkChannelE2E(sid, dm); err != nil {
		t.Fatal(err)
	}

	run(t, c)
	fc := fs.accept()
	fc.bootstrap() // Reset: true → resetCache
	waitFor(t, "online", online(c))
	if got := lastSeq(t, c); got != 20 {
		t.Fatalf("last_seq = %d", got)
	}
	if got := bodies(c.Messages(general, 50)); got != "first,mine" {
		t.Fatalf("cache not reset: %q", got)
	}
	check := func(ts *trust.Store, when string) {
		t.Helper()
		k, err := ts.DeviceKeys(sid, alice)
		if b, _ := k.MarshalBinary(); err != nil || !bytes.Equal(b, want) {
			t.Fatalf("%s: device keys changed (%v)", when, err)
		}
		if ks, err := ts.PinnedKeys(sid, bob); err != nil || len(ks) != 1 || ks[0].Fingerprint != fp {
			t.Fatalf("%s: pinned = %+v %v", when, ks, err)
		}
		if got, ok, err := ts.ChainHead(sid, dm); err != nil || !ok || got != h {
			t.Fatalf("%s: head = %+v %v %v", when, got, ok, err)
		}
		if ok, err := ts.ChannelE2E(sid, dm); err != nil || !ok {
			t.Fatalf("%s: e2e flag = %v %v", when, ok, err)
		}
		if ok, err := ts.ServerUsesE2E(sid); err != nil || !ok {
			t.Fatalf("%s: server flag = %v %v", when, ok, err)
		}
	}
	check(c.Trust(), "after reset snapshot")

	// The reset helper directly, on top of the live store.
	if err := resetCache(c.db, serverID); err != nil {
		t.Fatal(err)
	}
	check(c.Trust(), "after resetCache")

	// Re-login wipes the whole cache, outbox included.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Login(ctx, home, proto.FormatInvite(fs.addr(), "tok", fs.pin), "alice", writeKey(t)); err != nil {
		t.Fatal(err)
	}
	ts2, err := trust.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer ts2.Close()
	check(ts2, "after re-login wipe")
}
