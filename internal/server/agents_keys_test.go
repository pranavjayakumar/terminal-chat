package server

import (
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

var agentE2ECaps = []string{proto.CapAgent, proto.CapE2E}

// An agent session gets channel key material (key.fetch, key.share,
// key.need) only for channels it may read, like message content; and one
// active device, so it can't fill its owner's device slots.
func TestAgentKeyMaterialFollowsAccess(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", true, agentE2ECaps)
	alice.syncFresh()
	sid := ts.sid()
	alice.ok(proto.OpAgentRegister, proto.AgentRegisterArgs{Handle: "rocky"}, nil)
	key := newKey(t)
	alice.ok(proto.OpAgentKey, agentKeyArgs(ts, alice, key), nil)
	agent, bye := ts.connectCaps(t, key, "", "", agentE2ECaps)
	if bye != "" {
		t.Fatal(bye)
	}
	agent.syncFresh()

	agentDev, _ := agent.registerDevice(ts)
	cert, _ := certFor(t, key, sid, alice.user.ID)
	agent.fail(proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: cert}, proto.ErrConflict)

	a1, _ := alice.registerDevice(ts)
	ch := alice.createE2E("sec")
	since := alice.rotate(ch.ID, bundle(t, a1, sid, ch.ID, 1, nil, []uint64{alice.user.ID}, a1, agentDev))

	fetch := proto.KeyFetchArgs{Chan: ch.ID, From: 1, To: 1}
	agent.fail(proto.OpKeyFetch, fetch, proto.ErrForbidden)
	share := proto.KeyShareArgs{Chan: ch.ID, Epoch: 1, Dev: agentDev.ID[:], Eph: make([]byte, 32), CT: make([]byte, 48)}
	agent.fail(proto.OpKeyShare, share, proto.ErrForbidden)
	alice.ok(proto.OpKeyShare, share, nil)
	agent.noKind(proto.KKeyShare, 200*time.Millisecond)
	for _, ev := range agent.replayCaps(t, ts, since, agentE2ECaps) {
		if ev.Kind == proto.KKeyShare {
			t.Fatal("key.share replayed to an agent that can't read the channel")
		}
	}

	alice.sendEph(proto.Ephemeral{Kind: proto.EphKeyNeed, Chan: ch.ID, Epoch: 1, Dev: a1.ID[:]})
	if e, ok := agent.keyNeedFrom(300 * time.Millisecond); ok {
		t.Fatalf("agent got key.need for a channel it can't read: %+v", e)
	}
	agent.sendEph(proto.Ephemeral{Kind: proto.EphKeyNeed, Chan: ch.ID, Epoch: 1, Dev: agentDev.ID[:]})
	if e, ok := alice.keyNeedFrom(300 * time.Millisecond); ok {
		t.Fatalf("agent's key.need for a channel it can't read was relayed: %+v", e)
	}

	// Granted, all of it works.
	alice.ok(proto.OpAgentAccess, proto.AgentAccessArgs{Add: []uint64{ch.ID}}, nil)
	agent.ok(proto.OpKeyFetch, fetch, nil)
	alice.ok(proto.OpKeyShare, share, nil)
	agent.event(proto.KKeyShare)
}
