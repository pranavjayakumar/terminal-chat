package server

import (
	"bytes"
	"context"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

func endorseArgs(t *testing.T, by ssh.Signer, sid e2ecrypto.ServerID, user uint64, key ssh.PublicKey) proto.KeyEndorseArgs {
	t.Helper()
	e, err := identity.NewKeyEndorse(by, sid, user, key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return proto.KeyEndorseArgs{Endorse: proto.Signed(e)}
}

// key.endorse (#146): one of the caller's login keys vouches for another;
// everything else is refused, the endorsement reaches every e2e1 session
// live, in devices.list and in the snapshot, and a retry is a no-op.
func TestKeyEndorse(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	alice.syncFresh()
	bob.syncFresh()
	if !containsCap(alice.welcome.Caps, proto.CapKeyEndorse) {
		t.Fatalf("caps %v lack %s", alice.welcome.Caps, proto.CapKeyEndorse)
	}
	sid := ts.sid()
	alice.registerDevice(ts)
	bob.event(proto.KDeviceNew)

	// alice's second key, added by an add-key invite, with a device of its own.
	laptopKey := newKey(t)
	code, _ := ts.CreateInvite(context.Background(), false, alice.user.ID)
	inv, _ := proto.ParseInvite(code)
	laptop, bye := ts.connectCaps(t, laptopKey, inv.Token, "", e2eCaps)
	if bye != "" {
		t.Fatal(bye)
	}
	laptop.syncFresh()
	lc, _ := certFor(t, laptopKey, sid, alice.user.ID)
	laptop.ok(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: lc}, nil)
	bob.event(proto.KDeviceNew)

	stranger := newKey(t)
	for _, c := range []struct {
		name string
		args proto.KeyEndorseArgs
		code string
	}{
		{"self", endorseArgs(t, laptopKey, sid, alice.user.ID, laptopKey.PublicKey()), proto.ErrForbidden},
		{"unregistered signer", endorseArgs(t, stranger, sid, alice.user.ID, laptopKey.PublicKey()), proto.ErrForbidden},
		{"bob's key signs", endorseArgs(t, bob.signer, sid, alice.user.ID, laptopKey.PublicKey()), proto.ErrForbidden},
		{"for bob", endorseArgs(t, alice.signer, sid, bob.user.ID, laptopKey.PublicKey()), proto.ErrForbidden},
		{"other server", endorseArgs(t, alice.signer, e2ecrypto.ServerID{0xee}, alice.user.ID, laptopKey.PublicKey()), proto.ErrBadRequest},
		{"not a login key", endorseArgs(t, alice.signer, sid, alice.user.ID, stranger.PublicKey()), proto.ErrBadRequest},
		{"bob's login key", endorseArgs(t, alice.signer, sid, alice.user.ID, bob.signer.PublicKey()), proto.ErrBadRequest},
		{"garbage", proto.KeyEndorseArgs{Endorse: proto.Signed{B: []byte{0xc1}, Sig: []byte{1}}}, proto.ErrBadRequest},
		{"too large", proto.KeyEndorseArgs{Endorse: proto.Signed{B: make([]byte, proto.MaxSigned+1)}}, proto.ErrTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) { alice.fail(proto.OpKeyEndorse, c.args, c.code) })
	}
	good := endorseArgs(t, alice.signer, sid, alice.user.ID, laptopKey.PublicKey())
	bad := proto.KeyEndorseArgs{Endorse: proto.Signed{B: good.Endorse.B, Sig: bytes.Clone(good.Endorse.Sig)}}
	bad.Endorse.Sig[len(bad.Endorse.Sig)-1] ^= 1
	alice.fail(proto.OpKeyEndorse, bad, proto.ErrForbidden)
	bob.quiet(100 * time.Millisecond) // nothing above produced an event

	var r proto.SeqRes
	alice.ok(proto.OpKeyEndorse, good, &r)
	if r.Seq == 0 {
		t.Fatal("seq 0")
	}
	var ev proto.KeyEndorseArgs
	if err := proto.Unmarshal(bob.event(proto.KKeyEndorse).Data, &ev); err != nil || !bytes.Equal(ev.Endorse.B, good.Endorse.B) {
		t.Fatalf("event: %+v %v", ev, err)
	}
	// Either session of alice's may send it; a repeat changes nothing.
	r = proto.SeqRes{}
	laptop.ok(proto.OpKeyEndorse, good, &r)
	if r.Seq != 0 {
		t.Fatalf("repeat: seq %d", r.Seq)
	}
	bob.quiet(100 * time.Millisecond)

	var list proto.DevicesListRes
	bob.ok(proto.OpDevicesList, proto.DevicesListArgs{Users: []uint64{alice.user.ID}}, &list)
	if len(list.Endorse) != 1 || !bytes.Equal(list.Endorse[0].B, good.Endorse.B) {
		t.Fatalf("devices.list endorse: %+v", list.Endorse)
	}
	bob.ok(proto.OpDevicesList, proto.DevicesListArgs{Users: []uint64{bob.user.ID}}, &list)
	if len(list.Endorse) != 0 {
		t.Fatalf("bob's list has alice's endorsement: %+v", list.Endorse)
	}
	carol := ts.registerCaps(t, "carol", false, e2eCaps)
	snap, _ := carol.syncFresh()
	if len(snap.Endorse) != 1 || !bytes.Equal(snap.Endorse[0].B, good.Endorse.B) {
		t.Fatalf("snapshot endorse: %+v", snap.Endorse)
	}
	// A session without e2e1 never sees it, live or replayed.
	dave := ts.register(t, "dave", false)
	snap, _ = dave.syncFresh()
	if len(snap.Endorse) != 0 {
		t.Fatal("snapshot without e2e1 has endorsements")
	}
	dave.fail(proto.OpKeyEndorse, good, proto.ErrUnknownOp)
}

// An agent session never endorses a key, not even with its owner's (it
// holds only its own; this checks the server's gate).
func TestKeyEndorseAgentRefused(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, agentE2ECaps)
	alice.syncFresh()
	alice.ok(proto.OpAgentRegister, proto.AgentRegisterArgs{Handle: "ally"}, nil)
	key := newKey(t)
	alice.ok(proto.OpAgentKey, agentKeyArgs(ts, alice, key), nil)
	agent, bye := ts.connectCaps(t, key, "", "", agentE2ECaps)
	if bye != "" {
		t.Fatal(bye)
	}
	agent.syncFresh()
	agent.fail(proto.OpKeyEndorse, endorseArgs(t, alice.signer, ts.sid(), alice.user.ID, key.PublicKey()), proto.ErrForbidden)
	// Nor can the owner make the agent's key one of the user's own.
	alice.fail(proto.OpKeyEndorse, endorseArgs(t, alice.signer, ts.sid(), alice.user.ID, key.PublicKey()), proto.ErrBadRequest)
}
