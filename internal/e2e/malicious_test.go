//go:build tctamper

package e2e

// Malicious-server suite (docs/E2E.md step 11). Run with
//
//	GOTOOLCHAIN=local go test -race -tags tctamper ./internal/e2e -run Malicious
//
// Each test turns the real server into an attacker (internal/server
// tamper_on.go: rewrite/drop/inject/reorder frames per session, rewrite
// WELCOME, edit the database) against real clients over loopback, and
// checks (a) that no key or plaintext reaches anything the attacker
// controls and (b) the signal the honest client shows. Behaviour the
// threat model accepts (docs/E2E.md "Threat model") is pinned down by
// tests named ..._AcceptedLimit instead of being skipped.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// ---- devices and keys ----

// The server makes a new SSH key count as alice's (a row in its keys table,
// or an add-key invite it mints itself) and registers a device under it.
// That device's cert verifies, but under a key no one pinned: bob shows a
// key change and refuses to send to alice until it is accepted, and no
// epoch box or key.share ever goes to the attacker's device.
func TestMaliciousInjectedDeviceKey(t *testing.T) {
	for _, how := range []string{"server-added key", "add-key invite"} {
		t.Run(how, func(t *testing.T) {
			env := malTeam(t, "alice", "bob")
			alice, bob := env.us[0], env.us[1]
			ctx := ctxT(t)
			dm := env.encDM(t, alice, bob)
			a := alice.Me().ID

			keyPath := writeKey(t, t.TempDir())
			var mal *user
			if how == "server-added key" {
				signer, err := identity.Load(keyPath)
				if err != nil {
					t.Fatal(err)
				}
				pk := signer.PublicKey()
				if _, err := env.db().Exec(`INSERT INTO keys (fp, user, pubkey, created) VALUES (?, ?, ?, 0)`,
					identity.Fingerprint(pk), a, pk.Marshal()); err != nil {
					t.Fatal(err)
				}
				mal = openWith(t, alice, keyPath)
			} else {
				code, err := env.s.CreateInvite(ctx, false, a)
				if err != nil {
					t.Fatal(err)
				}
				home := t.TempDir()
				if _, err := client.Login(ctx, home, code, "alice", keyPath); err != nil {
					t.Fatal(err)
				}
				c, err := client.Open(home)
				if err != nil {
					t.Fatal(err)
				}
				rctx, cancel := context.WithCancel(context.Background())
				go c.Run(rctx)
				t.Cleanup(func() { cancel(); c.Close() })
				mal = &user{Client: c, cancel: cancel, home: home}
			}
			eventually(t, "attacker device registered", func() bool {
				var n int
				env.db().QueryRow(`SELECT COUNT(*) FROM devices WHERE user = ?`, a).Scan(&n)
				return n == 2
			})
			malDev := mal.dev(t, env.sid).ID

			eventually(t, "bob sees a key change for alice", func() bool {
				return slices.Contains(bob.KeyChangedMembers(dm), a)
			})
			if err := bob.Send(dm, "to alice", 0); !errors.Is(err, client.ErrKeyChange) {
				t.Fatalf("bob's send: %v, want ErrKeyChange", err)
			}
			// alice still talks to bob, and rotates: neither seals to the
			// attacker's device (it isn't verified by anyone).
			alice.send(t, ctx, dm, "alice to bob")
			eventually(t, "bob reads alice", bob.reads(dm, "alice to bob"))
			rotate(t, ctx, alice, dm)
			eventually(t, "bob at epoch 2", bob.e2eAt(dm, 2))
			time.Sleep(time.Second) // the attacker's key.need gets every chance to be answered
			if env.sealedTo(t, malDev) {
				t.Fatal("a key was sealed to the attacker's device")
			}
			if k := mal.keysHeld(env.sid, dm); len(k) != 0 {
				t.Fatalf("attacker holds keys for epochs %v", k)
			}
			for _, b := range env.bundles(t, dm) {
				if slices.ContainsFunc(b.Boxes, func(x identity.EpochBox) bool { return x.Dev == malDev }) {
					t.Fatal("bundle boxes the attacker")
				}
			}
		})
	}
}

// The server adds mallory (a real user it colludes with) to an encrypted
// channel with an unsigned chan.join; then also with a next-epoch bundle
// mallory signed herself. Members alarm (unconfirmed membership / rotator
// not in the previous roster), refuse to send, and nothing is sealed to
// mallory.
func TestMaliciousFakeJoin(t *testing.T) {
	for _, withBundle := range []bool{false, true} {
		name := "plain chan.join"
		if withBundle {
			name = "with mallory's bundle"
		}
		t.Run(name, func(t *testing.T) {
			env := malTeam(t, "alice", "bob", "mallory")
			alice, bob, mallory := env.us[0], env.us[1], env.us[2]
			ctx := ctxT(t)
			ch := env.encChan(t, alice, "sec", bob)
			m := mallory.Me().ID
			before := len(rawEncs(t, env.dir, ch))

			_, err := env.s.TamperWrite(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
				ev, err := env.s.TamperAddMember(ctx, tx, ch, m)
				if err != nil || !withBundle {
					return []proto.Event{ev}, err
				}
				h := alice.head(t, env.sid, ch) // the attacker has the bundle bytes, so the hash
				k, _ := e2ecrypto.NewEpochKey(rand.Reader)
				junk, _ := e2ecrypto.NewEpochKey(rand.Reader) // mallory can't know K2
				b, err := identity.NewEpochBundle(rand.Reader, mallory.dev(t, env.sid), identity.EpochParams{
					ServerID: env.sid, Chan: ch, Epoch: h.epoch + 1, Prev: h.hash,
					Roster: []uint64{alice.Me().ID, bob.Me().ID, m}, Time: time.Now().UnixMilli(), K: k, PrevK: &junk,
					Recipients: mallory.recipients(t, env.sid, alice.Me().ID, bob.Me().ID, m),
				})
				if err != nil {
					return nil, err
				}
				kev, err := env.s.TamperAppendEvent(ctx, tx, proto.KKeyEpoch, ch, m, 0, proto.KeyEpoch{Bundle: proto.Signed(b)})
				return []proto.Event{ev, kev}, err
			})
			if err != nil {
				t.Fatal(err)
			}
			want := "membership not confirmed"
			if withBundle {
				want = "rotated by someone not in epoch 2"
			}
			for _, u := range []*user{alice, bob} {
				eventually(t, "alarm", func() bool {
					st := u.status(ch)
					return st.State == client.E2EAlarm && strings.Contains(st.Detail, want)
				})
			}
			if err := alice.Send(ch, "secret", 0); err == nil {
				if err := alice.Flush(ctx); err == nil || !strings.Contains(alice.failed(ch), "alarm") {
					t.Fatalf("alice's send went through (%v, failed %q)", err, alice.failed(ch))
				}
			} else if !errors.Is(err, client.ErrE2EAlarm) {
				t.Fatalf("alice's send: %v", err)
			}
			if err := bob.RotateKey(ctx, ch); err == nil {
				t.Fatal("bob rotated in alarm")
			}
			if n := len(rawEncs(t, env.dir, ch)); n != before {
				t.Fatalf("%d new encrypted messages", n-before)
			}
			for _, b := range env.bundles(t, ch) {
				if slices.Contains(b.Roster, m) && b.By != mallory.dev(t, env.sid).ID {
					t.Fatal("a member sealed to mallory")
				}
			}
			if k := mallory.keysHeld(env.sid, ch); len(k) != 0 && !withBundle {
				t.Fatalf("mallory holds keys for epochs %v", k)
			}
		})
	}
}

// Rollback: the server re-sends an old bundle (harmless: it is the one
// verified, nothing changes and sends stay on the new epoch), claims the
// channel is back at epoch 1 (ignored: cached epochs only grow), then
// forces a cache reset whose snapshot says epoch 1 (alarm, sends refused;
// the trust store survives the reset).
func TestMaliciousRollback(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	dm := env.encDM(t, alice, bob)
	rotate(t, ctx, alice, dm)
	eventually(t, "bob at epoch 2", bob.e2eAt(dm, 2))

	old := env.rawBundles(t, dm)[0]
	env.inject(t, proto.KKeyEpoch, dm, alice.Me().ID, proto.KeyEpoch{Bundle: old})
	bob.send(t, ctx, dm, "after replay")
	eventually(t, "alice reads", alice.reads(dm, "after replay"))
	if st := bob.status(dm); st.State != client.E2EOK || st.Epoch != 2 {
		t.Fatalf("bob after the replayed bundle: %+v", st)
	}
	id := bob.msgID(dm, "after replay")
	if h, err := e2ecrypto.ParseEnc(env.rawMsg(t, id).Enc); err != nil || h.Epoch != 2 {
		t.Fatalf("sealed under epoch %d (%v)", h.Epoch, err)
	}

	// A live claim of an older epoch is ignored: cached epochs only grow.
	cv, _ := bob.Channel(dm)
	rolled := cv.Channel
	rolled.Epoch = 1
	env.inject(t, proto.KChanUpdate, dm, alice.Me().ID, rolled)
	bob.send(t, ctx, dm, "after the claim")
	id = bob.msgID(dm, "after the claim")
	if h, err := e2ecrypto.ParseEnc(env.rawMsg(t, id).Enc); err != nil || h.Epoch != 2 {
		t.Fatalf("sealed under epoch %d (%v)", h.Epoch, err)
	}

	// A forced reset: on bob's next connection the server slips in a
	// snapshot (the reset signal) where the DM is at epoch 1 with its old
	// bundle. The cache is rebuilt from it, the trust store isn't: alarm.
	b := bob.Me().ID
	sn := proto.Snapshot{Users: bob.Users(), Keys: []proto.EpochKey{{Bundle: old, Seq: 1}}}
	for _, c := range bob.Channels() {
		if c.ID == dm {
			c.Channel.Epoch = 1
		}
		sn.Chans = append(sn.Chans, c.Channel)
	}
	armed := false
	env.tp.setWelcome(func(user uint64, _ *proto.Welcome) { armed = armed || user == b })
	env.tp.setOut(func(user uint64, f frame) []frame {
		if user != b || !armed || (f.Type != proto.TEvent && f.Type != proto.TRes) {
			return []frame{f}
		}
		armed = false
		return []frame{{Type: proto.TSnapshot, Payload: proto.MustRaw(sn)}, f}
	})
	env.s.TamperKick(b)
	eventually(t, "rollback alarm", func() bool {
		st := bob.status(dm)
		return st.State == client.E2EAlarm && strings.Contains(st.Detail, "rollback")
	})
	if err := bob.Send(dm, "after rollback", 0); !errors.Is(err, client.ErrE2EAlarm) {
		t.Fatalf("send after rollback: %v", err)
	}
}

// Fork: bob is served a different, validly signed epoch-4 bundle than
// alice (carol's device signed two, as in a rotation race the server
// resolved differently per recipient). alice's messages under the real
// epoch are invalid to bob; once either sees the other bundle: alarm.
func TestMaliciousFork(t *testing.T) {
	env := malTeam(t, "alice", "bob", "carol")
	alice, bob, carol := env.us[0], env.us[1], env.us[2]
	ctx := ctxT(t)
	ch := env.encChan(t, alice, "sec", bob, carol)
	b := bob.Me().ID

	h := carol.head(t, env.sid, ch)
	k, _ := e2ecrypto.NewEpochKey(rand.Reader)
	alt, err := identity.NewEpochBundle(rand.Reader, carol.dev(t, env.sid), identity.EpochParams{
		ServerID: env.sid, Chan: ch, Epoch: h.epoch + 1, Prev: h.hash, Roster: h.roster,
		Time: time.Now().UnixMilli(), K: k, PrevK: h.key,
		Recipients: carol.recipients(t, env.sid, h.roster...),
	})
	if err != nil {
		t.Fatal(err)
	}
	env.tp.setOut(func(user uint64, f frame) []frame {
		if ev, ok := asEvent(f); ok && user == b && ev.Kind == proto.KKeyEpoch && ev.Chan == ch {
			ev.Data = proto.MustRaw(proto.KeyEpoch{Bundle: proto.Signed(alt)})
			return []frame{evFrame(ev)}
		}
		return []frame{f}
	})
	rotate(t, ctx, carol, ch)
	eventually(t, "alice and bob at epoch 4", func() bool { return alice.e2eAt(ch, 4)() && bob.e2eAt(ch, 4)() })
	env.tp.setOut(nil)
	if *bob.epochKey(env.sid, ch, 4) == *alice.epochKey(env.sid, ch, 4) {
		t.Fatal("no fork happened")
	}
	alice.send(t, ctx, ch, "on the real branch")
	id := alice.msgID(ch, "on the real branch")
	eventually(t, "invalid to bob", func() bool { m, ok := bob.msgAny(ch, id); return ok && m.Sec == client.SecInvalid })

	// The other branch reaches each of them: fork alarm on both sides.
	real := env.rawBundles(t, ch)[3]
	env.inject(t, proto.KKeyEpoch, ch, carol.Me().ID, proto.KeyEpoch{Bundle: real})
	env.inject(t, proto.KKeyEpoch, ch, carol.Me().ID, proto.KeyEpoch{Bundle: proto.Signed(alt)})
	for _, u := range []*user{alice, bob} {
		eventually(t, "fork alarm", func() bool {
			st := u.status(ch)
			return st.State == client.E2EAlarm && strings.Contains(st.Detail, "fork")
		})
	}
}

// The server writes its own epoch bundle with a key K it chose: signed by
// a device no one verified (can't be judged: "unverified", and nothing is
// ever sealed with K), or claiming to be bob's device (bad signature:
// alarm).
func TestMaliciousForgedBundle(t *testing.T) {
	for _, claim := range []string{"unknown device", "bob's device"} {
		t.Run(claim, func(t *testing.T) {
			env := malTeam(t, "alice", "bob")
			alice, bob := env.us[0], env.us[1]
			ctx := ctxT(t)
			dm := env.encDM(t, alice, bob)
			fake, _ := e2ecrypto.Generate(rand.Reader)
			if claim == "bob's device" {
				fake.ID = bob.dev(t, env.sid).ID
			}
			h := alice.head(t, env.sid, dm)
			k, _ := e2ecrypto.NewEpochKey(rand.Reader)
			junk, _ := e2ecrypto.NewEpochKey(rand.Reader)
			forged, err := identity.NewEpochBundle(rand.Reader, fake, identity.EpochParams{
				ServerID: env.sid, Chan: dm, Epoch: 2, Prev: h.hash, Roster: h.roster,
				Time: time.Now().UnixMilli(), K: k, PrevK: &junk,
				Recipients: alice.recipients(t, env.sid, h.roster...),
			})
			if err != nil {
				t.Fatal(err)
			}
			env.inject(t, proto.KKeyEpoch, dm, bob.Me().ID, proto.KeyEpoch{Bundle: proto.Signed(forged)})
			want, detail := client.E2EUnverified, "can't verify the rotating device"
			if claim == "bob's device" {
				want, detail = client.E2EAlarm, "signature doesn't verify"
			}
			eventually(t, "alice judges the bundle", func() bool {
				st := alice.status(dm)
				return st.State == want && strings.Contains(st.Detail, detail)
			})
			// Sending may go on under the verified epoch 1 (or be refused in
			// alarm); it must never use the server's K.
			if err := alice.Send(dm, "secret", 0); err == nil {
				if err := alice.Flush(ctx); err == nil {
					eventually(t, "bob reads under epoch 1", bob.reads(dm, "secret"))
				}
			} else if !errors.Is(err, client.ErrE2EAlarm) {
				t.Fatal(err)
			}
			for id, enc := range rawEncs(t, env.dir, dm) {
				if h, _ := e2ecrypto.ParseEnc(enc); h.Epoch != 1 {
					t.Fatalf("message %d sealed under epoch %d", id, h.Epoch)
				}
			}
			for _, u := range []*user{alice, bob} {
				if u.epochKey(env.sid, dm, 2) != nil {
					t.Fatal("the server's K was accepted")
				}
			}
		})
	}
}

// epochB mirrors identity's EpochBundle.b encoding, to edit a bundle's
// bytes the way an attacker would.
type epochB struct {
	V        uint32   `msgpack:"v"`
	ServerID []byte   `msgpack:"server_id"`
	Chan     uint64   `msgpack:"chan"`
	Epoch    uint32   `msgpack:"epoch"`
	Prev     []byte   `msgpack:"prev"`
	Roster   []uint64 `msgpack:"roster"`
	By       []byte   `msgpack:"by"`
	Time     int64    `msgpack:"time"`
	KC       []byte   `msgpack:"kc"`
	Eph      []byte   `msgpack:"eph"`
	Boxes    []boxB   `msgpack:"boxes"`
	Back     []byte   `msgpack:"back,omitempty"`
}

type boxB struct {
	Dev []byte `msgpack:"dev"`
	CT  []byte `msgpack:"ct"`
}

// Boxes can't be swapped or moved: bob's box replaced by alice's breaks
// the bundle's signature (alarm); a whole bundle of another channel
// delivered as this channel's is ignored (state and sends unchanged).
func TestMaliciousSwappedOrMovedBox(t *testing.T) {
	t.Run("swapped", func(t *testing.T) {
		env := malTeam(t, "alice", "bob")
		alice, bob := env.us[0], env.us[1]
		ctx := ctxT(t)
		dm := env.encDM(t, alice, bob)
		b := bob.Me().ID
		env.tp.setOut(func(user uint64, f frame) []frame {
			ev, ok := asEvent(f)
			if !ok || user != b || ev.Kind != proto.KKeyEpoch || ev.Chan != dm {
				return []frame{f}
			}
			var d proto.KeyEpoch
			var w epochB
			if proto.Unmarshal(ev.Data, &d) != nil || proto.Unmarshal(d.Bundle.B, &w) != nil || len(w.Boxes) != 2 {
				return []frame{f}
			}
			w.Boxes[0].CT, w.Boxes[1].CT = w.Boxes[1].CT, w.Boxes[0].CT
			d.Bundle.B = proto.MustRaw(w)
			ev.Data = proto.MustRaw(d)
			return []frame{evFrame(ev)}
		})
		rotate(t, ctx, alice, dm)
		eventually(t, "bob's alarm", func() bool {
			st := bob.status(dm)
			return st.State == client.E2EAlarm && strings.Contains(st.Detail, "signature")
		})
		if bob.epochKey(env.sid, dm, 2) != nil {
			t.Fatal("bob took a key from the tampered bundle")
		}
	})
	t.Run("moved from another channel", func(t *testing.T) {
		env := malTeam(t, "alice", "bob")
		alice, bob := env.us[0], env.us[1]
		ctx := ctxT(t)
		dm := env.encDM(t, alice, bob)
		other := env.encChan(t, alice, "other", bob) // epoch 2
		moved := env.rawBundles(t, other)[1]
		env.inject(t, proto.KKeyEpoch, dm, alice.Me().ID, proto.KeyEpoch{Bundle: moved})
		bob.send(t, ctx, dm, "still fine")
		eventually(t, "alice reads", alice.reads(dm, "still fine"))
		for _, u := range []*user{alice, bob} {
			if st := u.status(dm); st.State != client.E2EOK || st.Epoch != 1 {
				t.Fatalf("after the moved bundle: %+v", st)
			}
		}
	})
}

// A withheld box: the server hides bob's new device from alice, so alice's
// rotation boxes only bob's first device. bob2 asks with key.need and gets
// K from bob's other device by key.share: no alarm.
func TestMaliciousWithheldBox(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	dm := env.encDM(t, alice, bob)
	a := alice.Me().ID
	env.tp.setOut(func(user uint64, f frame) []frame {
		if ev, ok := asEvent(f); ok && user == a && ev.Kind == proto.KDeviceNew {
			return nil
		}
		return []frame{f}
	})
	bob2 := openAs(t, bob)
	eventually(t, "bob2 registered", func() bool {
		var n int
		env.db().QueryRow(`SELECT COUNT(*) FROM devices WHERE user = ?`, bob.Me().ID).Scan(&n)
		return n == 2
	})
	b2 := bob2.dev(t, env.sid).ID
	eventually(t, "bob verified bob2", func() bool { return bob.verifiedDevices(env.sid, bob.Me().ID) == 2 })
	// bob2 goes offline (so its next key.need isn't held back by the
	// 30 s key.need limit from its first one) while alice rotates.
	home := bob2.Home()
	bob2.cancel()
	bob2.Close()
	rotate(t, ctx, alice, dm)
	bob2 = reopen(t, home)
	if alice.verifiedDevices(env.sid, bob.Me().ID) != 1 {
		t.Fatal("alice saw bob2")
	}
	last := env.bundles(t, dm)
	if slices.ContainsFunc(last[len(last)-1].Boxes, func(x identity.EpochBox) bool { return x.Dev == b2 }) {
		t.Fatal("bob2 got a box")
	}
	alice.send(t, ctx, dm, "at epoch 2")
	eventually(t, "bob2 at epoch 2 via key.share", bob2.e2eAt(dm, 2))
	eventually(t, "bob2 reads", bob2.reads(dm, "at epoch 2"))
	if *bob2.epochKey(env.sid, dm, 2) != *alice.epochKey(env.sid, dm, 2) {
		t.Fatal("bob2 holds another K")
	}
	for _, u := range []*user{alice, bob, bob2} {
		if _, ok := u.Alarm(dm); ok {
			t.Fatal("alarm for a withheld box")
		}
	}
}

// ---- membership ----

// alice (creator) removes carol; the server hides carol's chan.leave from
// bob. The new epoch alice signed leaves carol out, so bob sees a member no
// verified epoch includes: alarm, and he never seals to carol.
func TestMaliciousHiddenRemove(t *testing.T) {
	env := malTeam(t, "alice", "bob", "carol")
	alice, bob, carol := env.us[0], env.us[1], env.us[2]
	ctx := ctxT(t)
	ch := env.encChan(t, alice, "sec", bob, carol)
	b, c := bob.Me().ID, carol.Me().ID
	env.tp.setOut(func(user uint64, f frame) []frame {
		if ev, ok := asEvent(f); ok && user == b && ev.Kind == proto.KChanLeave && ev.Chan == ch {
			var r proto.UserRef
			if proto.Unmarshal(ev.Data, &r) == nil && r.User == c {
				return nil
			}
		}
		return []frame{f}
	})
	if err := alice.RemoveMember(ctx, ch, c); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob's alarm", func() bool {
		st := bob.status(ch)
		return st.State == client.E2EAlarm && strings.Contains(st.Detail, "not confirmed") && st.Epoch == 4
	})
	if err := bob.Send(ch, "to whom?", 0); err == nil {
		if err := bob.Flush(ctx); err == nil {
			t.Fatal("bob sent with carol unconfirmed")
		}
	}
	alice.send(t, ctx, ch, "after carol")
	if carol.epochKey(env.sid, ch, 4) != nil {
		t.Fatal("carol holds epoch 4")
	}
	for id, enc := range rawEncs(t, env.dir, ch) {
		if carol.canDecrypt(t, env.sid, ch, enc) {
			t.Fatalf("carol can read message %d", id)
		}
	}
}

// carol leaves on her own (a leave can't carry a bundle) and the server
// withholds it: it tells carol she left but keeps her a member for
// everyone else (it re-adds her and hides both events). alice still sees
// carol in the channel and the current epoch, so she keeps sealing to a
// key carol holds. Accepted (threat model: the server may withhold a
// removal); this pins it down.
func TestMaliciousHiddenLeave_AcceptedLimit(t *testing.T) {
	env := malTeam(t, "alice", "bob", "carol")
	alice, bob, carol := env.us[0], env.us[1], env.us[2]
	ctx := ctxT(t)
	ch := env.encChan(t, alice, "sec", bob, carol)
	c := carol.Me().ID
	env.tp.setOut(func(user uint64, f frame) []frame {
		if ev, ok := asEvent(f); ok && ev.Chan == ch &&
			(ev.Kind == proto.KChanJoin || ev.Kind == proto.KChanLeave && user != c) {
			return nil
		}
		return []frame{f}
	})
	if err := carol.Leave(ctx, ch); err != nil {
		t.Fatal(err)
	}
	if _, err := env.s.TamperWrite(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		ev, err := env.s.TamperAddMember(ctx, tx, ch, c)
		return []proto.Event{ev}, err
	}); err != nil {
		t.Fatal(err)
	}
	alice.send(t, ctx, ch, "after carol left")
	eventually(t, "bob reads, nothing looks wrong", bob.reads(ch, "after carol left"))
	id := alice.msgID(ch, "after carol left")
	if st := alice.status(ch); st.State != client.E2EOK || st.Epoch != 3 {
		t.Fatalf("alice: %+v", st)
	}
	if !carol.canDecrypt(t, env.sid, ch, env.rawMsg(t, id).Enc) {
		t.Fatal("expected the accepted limit: carol reads what was sent after her hidden leave")
	}
}

// ---- downgrades ----

// The server strips e2e1 from WELCOME: a sticky server-wide alarm, sends
// into encrypted channels refused, nothing reaches the server in the clear.
func TestMaliciousStrippedCap(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	dm := env.encDM(t, alice, bob)
	a := alice.Me().ID
	env.tp.setWelcome(func(user uint64, w *proto.Welcome) {
		if user == a {
			w.Caps = slices.DeleteFunc(w.Caps, func(c string) bool { return c == proto.CapE2E })
		}
	})
	env.s.TamperKick(a)
	eventually(t, "downgrade alarm", func() bool { _, ok := alice.TrustAlarm(); return ok })
	if st := alice.status(dm); st.State != client.E2EAlarm {
		t.Fatalf("DM status %+v", st)
	}
	if err := alice.Send(dm, "in the clear?", 0); !errors.Is(err, client.ErrE2EAlarm) {
		t.Fatalf("send: %v", err)
	}
	if n := env.plaintextRows(t, dm); n != 0 {
		t.Fatalf("%d plaintext rows", n)
	}
	// Still sticky with the cap back.
	env.tp.setWelcome(nil)
	env.s.TamperKick(a)
	eventually(t, "online again", alice.online)
	if _, ok := alice.TrustAlarm(); !ok {
		t.Fatal("alarm cleared itself")
	}
}

// The server says a known encrypted DM isn't encrypted any more: alarm,
// sends refused, no plaintext.
func TestMaliciousClearedE2EFlag(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	dm := env.encDM(t, alice, bob)
	cv, _ := alice.Channel(dm)
	plain := cv.Channel
	plain.E2E, plain.Epoch = false, 0
	env.inject(t, proto.KChanUpdate, dm, alice.Me().ID, plain)
	for _, u := range []*user{alice, bob} {
		eventually(t, "alarm", func() bool {
			st := u.status(dm)
			return st.State == client.E2EAlarm && strings.Contains(st.Detail, "no longer encrypted")
		})
		if err := u.Send(dm, "in the clear?", 0); !errors.Is(err, client.ErrE2EAlarm) {
			t.Fatalf("send: %v", err)
		}
	}
	if n := env.plaintextRows(t, dm); n != 0 {
		t.Fatalf("%d plaintext rows", n)
	}
}

// A channel bob has never seen is presented to him as plaintext when he
// is added: his client takes the server's word and sends in the clear
// (an honest server refuses it, but the text has reached the server).
// Accepted (threat model: "Whether a channel is encrypted comes from the
// server for a channel this device has never seen").
func TestMaliciousPlaintextFirstContact_AcceptedLimit(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	b := bob.Me().ID
	ch, err := alice.CreateEncryptedChannel(ctx, "sec", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "epoch 1", alice.e2eAt(ch.ID, 1))
	env.tp.setOut(func(user uint64, f frame) []frame {
		ev, ok := asEvent(f)
		if !ok || user != b || ev.Chan != ch.ID || ev.Kind != proto.KChanJoin {
			return []frame{f}
		}
		var j proto.ChanJoin
		if proto.Unmarshal(ev.Data, &j) == nil {
			j.Chan.E2E, j.Chan.Epoch = false, 0
			ev.Data = proto.MustRaw(j)
		}
		return []frame{evFrame(ev)}
	})
	if err := alice.AddMember(ctx, ch.ID, b); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob sees the channel", func() bool { _, ok := bob.Channel(ch.ID); return ok })
	if st := bob.status(ch.ID); st.State != client.E2EOff {
		t.Fatalf("bob's status %+v", st)
	}
	if err := bob.Send(ch.ID, "sent in the clear", 0); err != nil {
		t.Fatal(err)
	}
	bob.Flush(ctx)
	if f := bob.failed(ch.ID); !strings.Contains(f, "upgrade tc") {
		t.Fatalf("expected the accepted limit: bob sent plaintext (failed %q)", f)
	}
}

// ---- injected and replayed messages ----

// A plaintext msg.new injected into an encrypted DM shows as inserted by
// the server: not trusted, no mention.
func TestMaliciousInjectedPlaintext(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	dm := env.encDM(t, alice, bob)
	id := env.injectMsg(t, proto.Message{Chan: dm, Author: alice.Me().ID, Body: "@bob click here"})
	eventually(t, "bob has it", func() bool { _, ok := bob.msgAny(dm, id); return ok })
	m, _ := bob.msgAny(dm, id)
	if m.Sec != client.SecInserted || m.Sec.Trusted() {
		t.Fatalf("sec %s", m.Sec)
	}
	if bob.mention(dm) {
		t.Fatal("inserted message counts as a mention")
	}
}

// An enc alice sent, replayed by the server into another channel, into a
// thread, as a duplicate, or as an edit: invalid or rejected each time.
func TestMaliciousReplayedEnc(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	dm := env.encDM(t, alice, bob)
	other := env.encChan(t, alice, "other", bob)
	a := alice.Me().ID
	alice.send(t, ctx, dm, "root")
	alice.send(t, ctx, dm, "original")
	eventually(t, "bob reads", bob.reads(dm, "original"))
	root, orig := alice.msgID(dm, "root"), alice.msgID(dm, "original")
	enc := env.rawMsg(t, orig).Enc

	invalid := func(ch, id uint64) func() bool {
		return func() bool { m, ok := bob.msgAny(ch, id); return ok && m.Sec == client.SecInvalid && m.Body == "" }
	}
	x := env.injectMsg(t, proto.Message{Chan: other, Author: a, Enc: enc})
	eventually(t, "replay into another channel invalid", invalid(other, x))
	th := env.injectMsg(t, proto.Message{Chan: dm, Author: a, Enc: enc, Thread: root})
	eventually(t, "replay into a thread invalid", invalid(dm, th))
	dup := env.injectMsg(t, proto.Message{Chan: dm, Author: a, Enc: enc})
	eventually(t, "duplicate invalid", invalid(dm, dup))
	if n := bob.count(dm, "original"); n != 1 {
		t.Fatalf("bob shows the text %d times", n)
	}
	if m, _ := bob.msgAny(dm, orig); m.Sec != client.SecE2E {
		t.Fatalf("the original became %s", m.Sec)
	}

	env.inject(t, proto.KMsgEdit, dm, a, proto.MsgEdit{Msg: root, Enc: enc, Edited: time.Now().UnixMilli()})
	eventually(t, "edit rejected", func() bool {
		return slices.ContainsFunc(bob.E2ERejects(), func(s string) bool { return strings.Contains(s, "not an edit") })
	})
	if m, _ := bob.msgAny(dm, root); m.Body != "root" || m.Edited != 0 {
		t.Fatalf("root after the replayed edit: %q edited %d", m.Body, m.Edited)
	}
}

// The server rewrites a message on its way to bob: another author or
// another thread makes it invalid.
func TestMaliciousRewrittenMessage(t *testing.T) {
	for _, field := range []string{"author", "thread"} {
		t.Run(field, func(t *testing.T) {
			env := malTeam(t, "alice", "bob")
			alice, bob := env.us[0], env.us[1]
			ctx := ctxT(t)
			dm := env.encDM(t, alice, bob)
			b := bob.Me().ID
			alice.send(t, ctx, dm, "root")
			root := alice.msgID(dm, "root")
			eventually(t, "bob has the root", bob.reads(dm, "root"))
			env.tp.setOut(func(user uint64, f frame) []frame {
				ev, ok := asEvent(f)
				if !ok || user != b || ev.Kind != proto.KMsgNew || ev.Chan != dm {
					return []frame{f}
				}
				var m proto.Message
				if proto.Unmarshal(ev.Data, &m) != nil || m.ID == root {
					return []frame{f}
				}
				if field == "author" {
					m.Author = b
				} else {
					m.Thread = root
				}
				ev.Data = proto.MustRaw(m)
				return []frame{evFrame(ev)}
			})
			alice.send(t, ctx, dm, "rewritten")
			id := alice.msgID(dm, "rewritten")
			eventually(t, "invalid to bob", func() bool { m, ok := bob.msgAny(dm, id); return ok && m.Sec == client.SecInvalid })
		})
	}
}

// The server rewrites a message's time: the shown time is the server's
// (Plain.ts is signed but not compared with it). Accepted: timestamps are
// metadata the server controls (docs/E2E.md "What the server still sees").
func TestMaliciousRewrittenTime_AcceptedLimit(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	dm := env.encDM(t, alice, bob)
	b := bob.Me().ID
	const fake = int64(946684800000) // 2000-01-01
	env.tp.setOut(func(user uint64, f frame) []frame {
		ev, ok := asEvent(f)
		if !ok || user != b || ev.Kind != proto.KMsgNew {
			return []frame{f}
		}
		var m proto.Message
		if proto.Unmarshal(ev.Data, &m) == nil {
			m.Time = fake
			ev.Data = proto.MustRaw(m)
		}
		return []frame{evFrame(ev)}
	})
	alice.send(t, ctx, dm, "when?")
	eventually(t, "bob reads", bob.reads(dm, "when?"))
	m, _ := bob.msgAny(dm, alice.msgID(dm, "when?"))
	if m.Time != fake {
		t.Fatalf("time %d", m.Time)
	}
}

// Forged deletes: the server deletes alice's message without her signed
// delete, or with her signed delete of another message. The text goes (the
// server can always delete), but it shows "removed (not by author)", never
// as alice's delete.
func TestMaliciousForgedDelete(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	dm := env.encDM(t, alice, bob)
	a := alice.Me().ID
	for _, s := range []string{"one", "two", "three"} {
		alice.send(t, ctx, dm, s)
	}
	eventually(t, "bob reads", bob.reads(dm, "three"))
	one, two, three := alice.msgID(dm, "one"), alice.msgID(dm, "two"), alice.msgID(dm, "three")
	if err := alice.Delete(ctx, three); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice's own delete is hers", func() bool { m, ok := bob.msgAny(dm, three); return ok && m.Deleted && m.Sec == client.SecE2E })
	var raw []byte
	if err := env.db().QueryRow(`SELECT data FROM events WHERE kind = ? AND chan = ? ORDER BY seq DESC LIMIT 1`, proto.KMsgDelete, dm).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var signed proto.MsgDelete
	if err := proto.Unmarshal(raw, &signed); err != nil || signed.Enc == nil {
		t.Fatalf("no signed delete: %v", err)
	}
	env.inject(t, proto.KMsgDelete, dm, a, proto.MsgDelete{Msg: one})
	env.inject(t, proto.KMsgDelete, dm, a, proto.MsgDelete{Msg: two, Enc: signed.Enc})
	for _, id := range []uint64{one, two} {
		eventually(t, "removed, not by author", func() bool {
			m, ok := bob.msgAny(dm, id)
			return ok && m.Deleted && m.Sec == client.SecRemoved && m.Body == ""
		})
	}
}

// Reordered events: bob gets alice's message before the key.epoch it is
// sealed under, and her second edit before her first, with the seqs
// swapped too (so the client applies both, in the server's order). The
// message waits and then opens; the newer edit wins.
func TestMaliciousReorderedEvents(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	dm := env.encDM(t, alice, bob)
	b := bob.Me().ID
	var held *proto.Event
	env.tp.setOut(func(user uint64, f frame) []frame {
		ev, ok := asEvent(f)
		if !ok || user != b || ev.Chan != dm || ev.Kind != proto.KKeyEpoch && ev.Kind != proto.KMsgEdit && ev.Kind != proto.KMsgNew {
			return []frame{f}
		}
		if held == nil {
			if ev.Kind == proto.KMsgNew {
				return []frame{f}
			}
			held = &ev
			return nil
		}
		first := *held
		held = nil
		first.Seq, ev.Seq = ev.Seq, first.Seq
		return []frame{evFrame(ev), evFrame(first)}
	})
	rotate(t, ctx, alice, dm)
	alice.send(t, ctx, dm, "under epoch 2")
	eventually(t, "bob reads after the late bundle", bob.reads(dm, "under epoch 2"))
	if st := bob.status(dm); st.Epoch != 2 {
		t.Fatalf("bob %+v", st)
	}
	id := alice.msgID(dm, "under epoch 2")
	if err := alice.Edit(ctx, id, "first edit"); err != nil {
		t.Fatal(err)
	}
	if err := alice.Edit(ctx, id, "second edit"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "newest edit wins", func() bool { m, _ := bob.msgAny(dm, id); return m.Body == "second edit" })
	time.Sleep(300 * time.Millisecond)
	if m, _ := bob.msgAny(dm, id); m.Body != "second edit" {
		t.Fatalf("bob ended on %q", m.Body)
	}
}

// The server drops the key.epoch of a rotation on its way to bob (or
// delivers it after a later event, so its seq is skipped: never replayed).
// bob must still get the epoch by key.fetch: when a message under it waits,
// and when his own send comes back stale_epoch. (Before the fix the
// channel stayed wedged for bob: the message waited forever and every
// send failed "couldn't start a new key".)
func TestMaliciousDroppedBundle(t *testing.T) {
	for _, then := range []string{"alice sends", "bob sends"} {
		t.Run(then, func(t *testing.T) {
			env := malTeam(t, "alice", "bob")
			alice, bob := env.us[0], env.us[1]
			ctx := ctxT(t)
			dm := env.encDM(t, alice, bob)
			b := bob.Me().ID
			env.tp.setOut(func(user uint64, f frame) []frame {
				if ev, ok := asEvent(f); ok && user == b && ev.Kind == proto.KKeyEpoch {
					return nil
				}
				return []frame{f}
			})
			rotate(t, ctx, alice, dm)
			if then == "alice sends" {
				alice.send(t, ctx, dm, "under epoch 2")
				eventually(t, "bob reads", bob.reads(dm, "under epoch 2"))
			} else {
				bob.send(t, ctx, dm, "bob under epoch 2")
				eventually(t, "alice reads", alice.reads(dm, "bob under epoch 2"))
				id := bob.msgID(dm, "bob under epoch 2")
				if h, _ := e2ecrypto.ParseEnc(env.rawMsg(t, id).Enc); h.Epoch != 2 {
					t.Fatalf("sealed under epoch %d", h.Epoch)
				}
			}
			eventually(t, "bob at epoch 2", bob.e2eAt(dm, 2))
		})
	}
}

// A different server_id in WELCOME (as if to force a reset onto a new
// identity): the client stops ("server identity changed") and its trust
// store is untouched.
func TestMaliciousServerIDChange(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	dm := env.encDM(t, alice, bob)
	b := bob.Me().ID
	k1 := *bob.epochKey(env.sid, dm, 1)
	env.tp.setWelcome(func(user uint64, w *proto.Welcome) {
		if user == b {
			w.ServerID = bytes.Repeat([]byte{0x42}, e2ecrypto.IDSize)
		}
	})
	env.s.TamperKick(b)
	eventually(t, "bob stopped", func() bool { return bob.Status().Stopped })
	if !strings.Contains(bob.Status().Err, "server identity changed") {
		t.Fatalf("err %q", bob.Status().Err)
	}
	if k := bob.epochKey(env.sid, dm, 1); k == nil || *k != k1 {
		t.Fatal("trust store changed")
	}
	if sid, _ := bob.ServerID(); sid != env.sid {
		t.Fatal("cache took the new server id")
	}
}

// key.share events to bob's new device are rewritten to carry a key the
// server chose: kc rejects it, bob2 keeps waiting and never holds it; the
// real K arrives with the next rotation (and older ones by backlink).
func TestMaliciousWrongKeyShare(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	dm := env.encDM(t, alice, bob)
	bob1 := bob.dev(t, env.sid).ID
	wrong, _ := e2ecrypto.NewEpochKey(rand.Reader)
	rewritten := 0
	// Every share to a device of bob's but his first, re-sealed with the
	// server's K to the X25519 key from that device's (public) cert.
	env.tp.setOut(func(user uint64, f frame) []frame {
		ev, ok := asEvent(f)
		if !ok || ev.Kind != proto.KKeyShare {
			return []frame{f}
		}
		var d proto.KeyShareArgs
		if proto.Unmarshal(ev.Data, &d) != nil || bytes.Equal(d.Dev, bob1[:]) {
			return []frame{f}
		}
		var x []byte
		if env.db().QueryRow(`SELECT x25519 FROM devices WHERE dev = ?`, d.Dev).Scan(&x) != nil || len(x) != 32 {
			return []frame{f}
		}
		eph, ct, err := e2ecrypto.SealShare(rand.Reader, [32]byte(x), e2ecrypto.ShareAD{
			ServerID: env.sid, Chan: d.Chan, Epoch: d.Epoch, Recipient: e2ecrypto.DevID(d.Dev)}, wrong)
		if err != nil {
			return []frame{f}
		}
		d.Eph, d.CT = eph[:], ct[:]
		ev.Data = proto.MustRaw(d)
		rewritten++
		return []frame{evFrame(ev)}
	})
	bob2 := openAs(t, bob)
	eventually(t, "a share reached bob2", func() bool {
		env.tp.mu.Lock()
		defer env.tp.mu.Unlock()
		return rewritten > 0
	})
	time.Sleep(500 * time.Millisecond)
	if bob2.epochKey(env.sid, dm, 1) != nil {
		t.Fatal("bob2 accepted a share")
	}
	if st := bob2.status(dm); st.State != client.E2EWaitingKey {
		t.Fatalf("bob2 %+v", st)
	}
	env.tp.setOut(nil)
	eventually(t, "alice verified bob2", func() bool { return alice.verifiedDevices(env.sid, bob.Me().ID) == 2 })
	rotate(t, ctx, alice, dm)
	eventually(t, "bob2 at epoch 2", bob2.e2eAt(dm, 2))
	eventually(t, "bob2 has K1 by backlink", func() bool { return bob2.epochKey(env.sid, dm, 1) != nil })
	if *bob2.epochKey(env.sid, dm, 1) != *alice.epochKey(env.sid, dm, 1) || *bob2.epochKey(env.sid, dm, 1) == wrong {
		t.Fatal("bob2 holds a wrong K1")
	}
}

// ---- malicious member (with or without the server) ----

// memberEnv: alice made #sec and added bob and mallory (epoch 3);
// mallory also has a bare connection to send what tc never would.
func memberEnv(t *testing.T) (*malEnv, uint64, *rawSession) {
	env := malTeam(t, "alice", "bob", "mallory")
	ch := env.encChan(t, env.us[0], "sec", env.us[1], env.us[2])
	m := env.us[2]
	return env, ch, dialRaw(t, env.addr, m.Config().Pin, m.Config().KeyPath)
}

// mallory rotates with a bundle whose box for bob holds a different K: the
// kc check rejects it, bob asks (key.need) and gets the real K by key.share
// from alice, no alarm (§13.3: an unusable box is recovered by key.share).
// kc guarantees bob can only ever end up with the K everyone else has.
func TestMaliciousMemberInconsistentBoxes(t *testing.T) {
	env, ch, raw := memberEnv(t)
	alice, bob, mallory := env.us[0], env.us[1], env.us[2]
	h := mallory.head(t, env.sid, ch)
	md := mallory.dev(t, env.sid)
	k, _ := e2ecrypto.NewEpochKey(rand.Reader)
	other, _ := e2ecrypto.NewEpochKey(rand.Reader)
	eph, err := e2ecrypto.NewEphemeral(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	epoch := h.epoch + 1
	kc := e2ecrypto.KeyCommitment(k, env.sid, ch, epoch)
	back, err := e2ecrypto.SealBack(rand.Reader, k, *h.key, env.sid, ch, epoch)
	if err != nil {
		t.Fatal(err)
	}
	w := epochB{V: 1, ServerID: env.sid[:], Chan: ch, Epoch: epoch, Prev: h.hash[:], Roster: h.roster,
		By: md.ID[:], Time: time.Now().UnixMilli(), KC: kc[:], Eph: eph.PublicKey().Bytes(), Back: back[:]}
	bobDev := bob.dev(t, env.sid).ID
	for _, r := range mallory.recipients(t, env.sid, h.roster...) {
		key := k
		if r.Dev == bobDev {
			key = other
		}
		ct, err := e2ecrypto.SealKey(eph, r.X25519, e2ecrypto.BoxAD{ServerID: env.sid, Chan: ch, Epoch: epoch, Recipient: r.Dev, By: md.ID}, key)
		if err != nil {
			t.Fatal(err)
		}
		w.Boxes = append(w.Boxes, boxB{Dev: r.Dev[:], CT: ct[:]})
	}
	b := proto.MustRaw(w)
	sig, err := e2ecrypto.SignEd25519(md.Ed25519, identity.CtxEpoch, b)
	if err != nil {
		t.Fatal(err)
	}
	rotateCap()
	if res := raw.call(proto.OpKeyRotate, proto.KeyRotateArgs{Chan: ch, Bundle: proto.Signed{B: b, Sig: sig}}); !res.OK {
		t.Fatalf("rotate: %v", res.Err)
	}
	eventually(t, "alice at the new epoch", alice.e2eAt(ch, epoch))
	eventually(t, "bob recovers K by key.share", bob.e2eAt(ch, epoch))
	if *bob.epochKey(env.sid, ch, epoch) != k {
		t.Fatal("bob holds another K")
	}
	if _, ok := bob.Alarm(ch); ok {
		t.Fatal("alarm")
	}
}

// mallory spams rotations: the server's cap lets one through, the rest are
// rate_limited, and everyone ends on the same key.
func TestMaliciousMemberRotationSpam(t *testing.T) {
	env, ch, _ := memberEnv(t)
	alice, bob, mallory := env.us[0], env.us[1], env.us[2]
	ctx := ctxT(t)
	rotateCap()
	ok, limited := 0, 0
	for i := 0; i < 6; i++ {
		err := mallory.RotateKey(ctx, ch)
		var pe *proto.Error
		switch {
		case err == nil:
			ok++
		case errors.As(err, &pe) && pe.Code == proto.ErrRateLimited:
			limited++
		default:
			t.Fatalf("rotate %d: %v", i, err)
		}
	}
	if ok != 1 || limited != 5 {
		t.Fatalf("%d rotations went through, %d limited", ok, limited)
	}
	for _, u := range []*user{alice, bob} {
		eventually(t, "epoch 4", u.e2eAt(ch, 4))
		if *u.epochKey(env.sid, ch, 4) != *mallory.epochKey(env.sid, ch, 4) {
			t.Fatal("keys differ")
		}
	}
}

// mallory sends well-signed encs whose contents are garbage, or text with
// terminal control characters: shown as invalid, attributed to mallory.
func TestMaliciousMemberGarbageEnc(t *testing.T) {
	env, ch, raw := memberEnv(t)
	alice, bob, mallory := env.us[0], env.us[1], env.us[2]
	type plain struct {
		T    string `msgpack:"t"`
		TS   int64  `msgpack:"ts"`
		Body string `msgpack:"body"`
	}
	for name, p := range map[string][]byte{
		"garbage":       {0xc1, 0xff, 0x00, 0x13},
		"control chars": proto.MustRaw(plain{T: "msg", TS: time.Now().UnixMilli(), Body: "hi\x1b]52;c;cm0gLXJmIH4=\x07\x1b[2J"}),
		"wrong type":    proto.MustRaw(plain{T: "edit", TS: time.Now().UnixMilli(), Body: "not a message"}),
	} {
		enc := mallory.encFor(t, env.sid, ch, p)
		nonce := make([]byte, 16)
		rand.Read(nonce)
		res := raw.call(proto.OpSend, proto.SendArgs{Chan: ch, Nonce: nonce, Enc: enc})
		if !res.OK {
			t.Fatalf("%s: server refused: %v", name, res.Err)
		}
		var sr proto.SendRes
		proto.Unmarshal(res.Data, &sr)
		for _, u := range []*user{alice, bob} {
			eventually(t, name+" invalid", func() bool {
				m, ok := u.msgAny(ch, sr.Msg)
				return ok && m.Sec == client.SecInvalid && m.Body == "" && m.Author == mallory.Me().ID
			})
		}
	}
}
