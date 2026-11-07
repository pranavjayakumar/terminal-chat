//go:build tctamper

package e2e

import (
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// #195: the server stores an encrypted message but its RES is lost; on the
// next connection the outbox holds it for a key change (failed), and the
// user retries it by hand before accepting the change. The retry encrypts
// afresh (§13.4), so the server answers conflict for the nonce: the client
// must see that the message was delivered, not show it as both delivered
// and failed.
func TestEncryptedRetryAfterLostRes(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	dm := env.encDM(t, alice, bob)
	a, b := alice.Me().ID, bob.Me().ID

	// Drop the RES of alice's send; later, end her session on the next
	// frame (as if the connection broke) so she reconnects.
	var dropRes, bye atomic.Bool
	dropRes.Store(true)
	env.tp.setOut(func(user uint64, f frame) []frame {
		if user != a {
			return []frame{f}
		}
		if bye.Load() {
			bye.Store(false)
			return []frame{{Type: proto.TBye, Payload: proto.MustRaw(proto.Bye{Code: proto.ByeServerShutdown})}}
		}
		if f.Type == proto.TRes && dropRes.Load() {
			var r proto.Res
			var sr proto.SendRes
			if proto.Unmarshal(f.Payload, &r) == nil && r.OK && proto.Unmarshal(r.Data, &sr) == nil && sr.Msg != 0 {
				dropRes.Store(false)
				return nil
			}
		}
		return []frame{f}
	})
	const body = "stored, but alice never heard"
	if err := alice.Send(dm, body, 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob reads it", bob.reads(dm, body))
	eventually(t, "the RES dropped", func() bool { return !dropRes.Load() })

	// bob gets a key alice hasn't pinned (the server adds it; a real add-key
	// invite looks the same to alice): a key change she must accept.
	keyPath := writeKey(t, t.TempDir())
	signer, err := identity.Load(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	pk := signer.PublicKey()
	if _, err := env.db().Exec(`INSERT INTO keys (fp, user, pubkey, created) VALUES (?, ?, ?, 0)`,
		identity.Fingerprint(pk), b, pk.Marshal()); err != nil {
		t.Fatal(err)
	}
	openWith(t, bob, keyPath)
	eventually(t, "alice sees bob's key change", func() bool { return slices.Contains(alice.KeyChangedMembers(dm), b) })

	// The connection breaks: alice reconnects, syncs the stored message
	// and the outbox holds the entry for the key change.
	bye.Store(true)
	bob.send(t, ctx, env.us[1].channel("general"), "anything for alice")
	pending := func() (client.Message, bool) {
		for _, m := range alice.Messages(dm, 50) {
			if m.ID == 0 && m.Local != 0 {
				return m, true
			}
		}
		return client.Message{}, false
	}
	eventually(t, "held for the key change", func() bool { m, ok := pending(); return ok && m.Failed != "" })
	eventually(t, "the stored copy synced", alice.reads(dm, body))

	// Retry by hand, then accept the change: the entry goes out encrypted
	// afresh under the same nonce.
	m, _ := pending()
	if err := alice.RetryFailed(m.Local); err != nil {
		t.Fatal(err)
	}
	if err := alice.AcceptKeyChanges(b, alice.PendingKeys(b), false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the outbox settles", func() bool {
		m, ok := pending()
		return !ok || m.Failed != "" && !strings.Contains(m.Failed, "key changed")
	})
	if m, ok := pending(); ok {
		t.Fatalf("still pending or failed after the retry: %q", m.Failed)
	}
	n := 0
	for _, m := range alice.Messages(dm, 50) {
		if m.Body == body && m.ID != 0 {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("alice shows %d delivered copies", n)
	}
}
