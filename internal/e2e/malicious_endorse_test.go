//go:build tctamper

package e2e

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// #146, malicious server: it registers an SSH key it holds as alice's, runs
// a device under it, and then pushes every KeyEndorse it can make or
// replay. None of them makes alice's devices (or bob) trust that key: an
// endorsement counts only if a key the client already pinned for that same
// user signed it, for this server. Without one the key stays a pending
// change, /accept-key refuses alice's own keys, and nothing is sealed to
// the server's device.
func TestMaliciousOwnKeyEndorsement(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	dm := env.encDM(t, alice, bob)
	a, b := alice.Me().ID, bob.Me().ID

	// The server's own key, made alice's (a keys row), with a device.
	malPath := writeKey(t, t.TempDir())
	mal, err := identity.Load(malPath)
	if err != nil {
		t.Fatal(err)
	}
	malPK := mal.PublicKey()
	fpMal := identity.Fingerprint(malPK)
	if _, err := env.db().Exec(`INSERT INTO keys (fp, user, pubkey, created) VALUES (?, ?, ?, 0)`, fpMal, a, malPK.Marshal()); err != nil {
		t.Fatal(err)
	}
	malDevice := openWith(t, alice, malPath)
	eventually(t, "the server's device registered", func() bool {
		var n int
		env.db().QueryRow(`SELECT COUNT(*) FROM devices WHERE user = ?`, a).Scan(&n)
		return n == 2
	})
	isPending := func(u *user, of uint64, fp string) bool {
		ks, _ := u.Trust().PendingKeyChanges(env.sid)
		for _, k := range ks {
			if k.User == of && k.Fingerprint == fp {
				return true
			}
		}
		return false
	}
	isPinned := func(u *user, of uint64, fp string) bool {
		ks, _ := u.Trust().PinnedKeys(env.sid, of)
		for _, k := range ks {
			if k.Fingerprint == fp {
				return true
			}
		}
		return false
	}
	eventually(t, "the server's key is a pending change on alice and bob", func() bool {
		return isPending(alice, a, fpMal) && isPending(bob, a, fpMal)
	})

	// Forged and replayed endorsements, pushed as key.endorse events (the
	// server's checks are skipped: it is the attacker).
	aliceKey, err := identity.Load(filepath.Join(alice.home, "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	bobKey, err := identity.Load(filepath.Join(bob.home, "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := identity.Load(writeKey(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	endorse := func(by ssh.Signer, sid e2ecrypto.ServerID, user uint64, key ssh.PublicKey) proto.KeyEndorseArgs {
		e, err := identity.NewKeyEndorse(by, sid, user, key, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return proto.KeyEndorseArgs{Endorse: proto.Signed(e)}
	}
	otherServer := e2ecrypto.ServerID{0xee}
	badSig := endorse(aliceKey, env.sid, a, malPK)
	badSig.Endorse.Sig[len(badSig.Endorse.Sig)-1] ^= 1
	for _, e := range []proto.KeyEndorseArgs{
		endorse(mal, env.sid, a, malPK),              // self-endorsed
		endorse(stranger, env.sid, a, malPK),         // by a key no one pinned
		endorse(bobKey, env.sid, a, malPK),           // by bob's key, for alice
		endorse(aliceKey, otherServer, a, malPK),     // alice's, for another server
		badSig,                                       // alice's, signature broken
		endorse(mal, env.sid, b, bobKey.PublicKey()), // wrong direction
	} {
		env.inject(t, proto.KKeyEndorse, 0, a, e)
	}
	// A real endorsement alice made for bob's account (one she would never
	// sign, but the server may try any signature it has) doesn't count for
	// alice, and one for alice doesn't count for bob.
	env.inject(t, proto.KKeyEndorse, 0, a, endorse(aliceKey, env.sid, b, malPK))
	// The snapshot path too: a device that starts fresh gets them all.
	time.Sleep(time.Second)
	for _, u := range []*user{alice, bob} {
		if err := u.RefreshDevices(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	alice2 := openAs(t, alice)
	eventually(t, "alice's new device saw the server's key", func() bool { return isPending(alice2, a, fpMal) })
	time.Sleep(500 * time.Millisecond)

	for name, u := range map[string]*user{"alice": alice, "bob": bob, "alice2": alice2} {
		if isPinned(u, a, fpMal) || !isPending(u, a, fpMal) {
			t.Fatalf("%s trusts the server's key for alice", name)
		}
	}
	if isPinned(bob, b, fpMal) {
		t.Fatal("bob pins the server's key as his own")
	}
	if !slices.Contains(bob.KeyChangedMembers(dm), a) {
		t.Fatal("bob's key-change gate for alice opened")
	}
	if err := alice.AcceptKeyChanges(a, []string{fpMal}, true); !errors.Is(err, client.ErrOwnKeyAccept) {
		t.Fatal("alice accepted the server's key for herself on a prompt")
	}
	malDev := malDevice.dev(t, env.sid).ID
	for name, u := range map[string]*user{"alice": alice, "bob": bob, "alice2": alice2} {
		if _, ok, _ := u.Trust().Device(env.sid, malDev); ok {
			t.Fatalf("%s verified the server's device", name)
		}
	}
	// EndorseKey needs the exact fingerprint, which only the computer
	// holding the key shows; a look-alike is refused, not matched. (A key
	// she does name is her call.)
	if _, err := alice.EndorseKey(ctx, "SHA256:"+fpMal[len("SHA256:"):len(fpMal)-1]+"x"); !errors.Is(err, client.ErrKeyNotSeen) {
		t.Fatalf("near-miss fingerprint: %v", err)
	}
	rotate(t, ctx, alice, dm)
	time.Sleep(time.Second)
	if env.sealedTo(t, malDev) {
		t.Fatal("a key was sealed to the server's device")
	}
}
