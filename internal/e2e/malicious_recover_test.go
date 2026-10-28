//go:build tctamper

package e2e

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// serverEpoch is a row of the server's epochs table.
type serverEpoch struct {
	epoch        uint32
	bundle, hash []byte
	by, tm, seq  int64
	state        []byte
}

func (env *malEnv) serverEpochs(t *testing.T, ch uint64) []serverEpoch {
	t.Helper()
	rows, err := env.db().Query(`SELECT epoch, bundle, hash, by_user, time, seq, state FROM epochs WHERE chan = ? ORDER BY epoch`, ch)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []serverEpoch
	for rows.Next() {
		var e serverEpoch
		if err := rows.Scan(&e.epoch, &e.bundle, &e.hash, &e.by, &e.tm, &e.seq, &e.state); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// setServerChain replaces ch's stored bundles (what key.fetch serves) with
// es, as an attacker holding the database could.
func (env *malEnv) setServerChain(t *testing.T, ch uint64, es []serverEpoch) {
	t.Helper()
	if _, err := env.db().Exec(`DELETE FROM epochs WHERE chan = ?`, ch); err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if _, err := env.db().Exec(`INSERT INTO epochs (chan, epoch, bundle, hash, by_user, time, seq, state) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			ch, e.epoch, e.bundle, e.hash, e.by, e.tm, e.seq, e.state); err != nil {
			t.Fatal(err)
		}
	}
}

// forged is a server row for a bundle by `by` (any keys, any claimed id).
func forged(t *testing.T, by *e2ecrypto.DeviceKeys, p identity.EpochParams, seq int64) (serverEpoch, [32]byte) {
	t.Helper()
	k, _ := e2ecrypto.NewEpochKey(rand.Reader)
	p.K, p.Time = k, time.Now().UnixMilli()
	if p.Epoch > 1 {
		junk, _ := e2ecrypto.NewEpochKey(rand.Reader)
		p.PrevK = &junk
	}
	b, err := identity.NewEpochBundle(rand.Reader, by, p)
	if err != nil {
		t.Fatal(err)
	}
	h := e2ecrypto.ChainHash(b.B)
	return serverEpoch{epoch: p.Epoch, bundle: proto.MustRaw(proto.Signed(b)), hash: h[:], seq: seq}, h
}

// Accepting the server's chain after a restore (issue #84) is refused for
// anything but the verified chain cut short, or extended by valid epochs
// from a member: a forged or unverifiable branch, a chain sharing no epoch
// with the verified one, no chain at all, or a chain that still holds the
// head (the alarm has another cause). A refusal changes nothing.
func TestMaliciousRecoverRefusals(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	dm := env.encDM(t, alice, bob)
	rotate(t, ctx, alice, dm)
	eventually(t, "bob at epoch 2", bob.e2eAt(dm, 2))
	if _, err := bob.ChainRecoveryPlan(ctx, dm); !errors.Is(err, client.ErrNoAlarm) {
		t.Fatalf("plan without an alarm: %v", err)
	}
	orig := env.serverEpochs(t, dm)
	if len(orig) != 2 {
		t.Fatalf("%d server epochs", len(orig))
	}
	e1 := bob.head(t, env.sid, dm)
	ep1, _, _ := bob.Trust().Epoch(env.sid, dm, 1)
	roster := e1.roster
	recips := bob.recipients(t, env.sid, roster...)

	// A second, forged epoch 2 "by alice's device" (keys the server made):
	// bob's fork alarm.
	impostor, _ := e2ecrypto.Generate(rand.Reader)
	impostor.ID = alice.dev(t, env.sid).ID
	bad2, _ := forged(t, impostor, identity.EpochParams{ServerID: env.sid, Chan: dm, Epoch: 2, Prev: ep1.Hash,
		Roster: roster, Recipients: recips}, orig[1].seq)
	var sb proto.Signed
	proto.Unmarshal(bad2.bundle, &sb)
	env.inject(t, proto.KKeyEpoch, dm, alice.Me().ID, proto.KeyEpoch{Bundle: sb})
	eventually(t, "bob's alarm", inAlarm(bob, dm))
	head, _, _ := bob.Trust().ChainHead(env.sid, dm)

	refused := func(name, want string, chain []serverEpoch) {
		t.Helper()
		env.setServerChain(t, dm, chain)
		_, err := bob.ChainRecoveryPlan(ctx, dm)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: plan %v, want %q", name, err, want)
		}
		// Not even a stale plan for the real chain gets through.
		if err := bob.RecoverChain(ctx, client.ChainRecovery{Chan: dm, Head: 2, Ancestor: 1, Top: 1}); err == nil {
			t.Fatalf("%s: recovered", name)
		}
		if h, _, _ := bob.Trust().ChainHead(env.sid, dm); h != head || !inAlarm(bob, dm)() {
			t.Fatalf("%s changed bob's chain", name)
		}
		if bob.audited(env.sid, "rewind_chain") != 0 {
			t.Fatalf("%s audited a rewind", name)
		}
	}
	refused("forged epoch 2", "doesn't verify", []serverEpoch{orig[0], bad2})

	stranger, _ := e2ecrypto.Generate(rand.Reader)
	unknown2, _ := forged(t, stranger, identity.EpochParams{ServerID: env.sid, Chan: dm, Epoch: 2, Prev: ep1.Hash,
		Roster: roster, Recipients: recips}, orig[1].seq)
	refused("epoch 2 by an unknown device", "can't judge", []serverEpoch{orig[0], unknown2})

	// A branch whose first epoch is validly signed by alice's real device
	// but whose next isn't: the whole branch must verify.
	good2, h2 := forged(t, alice.dev(t, env.sid), identity.EpochParams{ServerID: env.sid, Chan: dm, Epoch: 2,
		Prev: ep1.Hash, Roster: roster, Recipients: recips}, orig[1].seq)
	bad3, _ := forged(t, impostor, identity.EpochParams{ServerID: env.sid, Chan: dm, Epoch: 3, Prev: h2,
		Roster: roster, Recipients: recips}, orig[1].seq+1)
	refused("valid epoch 2, forged epoch 3", "epoch 3 doesn't verify", []serverEpoch{orig[0], good2, bad3})

	refused("no chain", "no key epoch", nil)

	f1, fh1 := forged(t, impostor, identity.EpochParams{ServerID: env.sid, Chan: dm, Epoch: 1, Roster: roster, Recipients: recips}, orig[0].seq)
	f2, _ := forged(t, impostor, identity.EpochParams{ServerID: env.sid, Chan: dm, Epoch: 2, Prev: fh1,
		Roster: roster, Recipients: recips}, orig[1].seq)
	refused("another chain from epoch 1", "shares no epoch", []serverEpoch{f1, f2})

	refused("the verified chain itself", "isn't a restore", orig)

	// AcceptedLimit: the server alone can drop epochs and present the
	// verified chain cut short — indistinguishable from a restore. It is
	// offered, never applied without the user's confirmation, and the
	// preview names what it discards.
	env.setServerChain(t, dm, orig[:1])
	plan, err := bob.ChainRecoveryPlan(ctx, dm)
	if err != nil || plan.Fork || plan.Head != 2 || plan.Ancestor != 1 || plan.Top != 1 {
		t.Fatalf("prefix plan %+v %v", plan, err)
	}
	if h, _, _ := bob.Trust().ChainHead(env.sid, dm); h != head || !inAlarm(bob, dm)() {
		t.Fatal("a preview changed bob's chain")
	}
}
