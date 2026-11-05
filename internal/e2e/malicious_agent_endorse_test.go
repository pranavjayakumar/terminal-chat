//go:build tctamper

package e2e

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/trust"
)

// isAgentEndorse reports whether s is an AgentEndorse.
func isAgentEndorse(s *proto.Signed) bool {
	if s == nil {
		return false
	}
	_, err := identity.PeekAgentEndorse(identity.Signed(*s))
	return err == nil
}

// stripAgentEndorse removes every AgentEndorse from f: device.new events,
// devices.list results and snapshots.
func stripAgentEndorse(f frame) frame {
	switch f.Type {
	case proto.TEvent:
		ev, ok := asEvent(f)
		if !ok || ev.Kind != proto.KDeviceNew {
			return f
		}
		var d proto.Device
		if proto.Unmarshal(ev.Data, &d) != nil || !isAgentEndorse(d.Endorse) {
			return f
		}
		d.Endorse = nil
		ev.Data = proto.MustRaw(d)
		return evFrame(ev)
	case proto.TRes:
		var r proto.Res
		var dl proto.DevicesListRes
		if proto.Unmarshal(f.Payload, &r) != nil || len(r.Data) == 0 || proto.Unmarshal(r.Data, &dl) != nil {
			return f
		}
		kept := dl.Endorse[:0:0]
		for _, e := range dl.Endorse {
			if !isAgentEndorse(&e) {
				kept = append(kept, e)
			}
		}
		if len(kept) == len(dl.Endorse) {
			return f
		}
		dl.Endorse = kept
		r.Data = proto.MustRaw(dl)
		return frame{Type: f.Type, Payload: proto.MustRaw(r)}
	case proto.TSnapshot:
		var sn proto.Snapshot
		if proto.Unmarshal(f.Payload, &sn) != nil {
			return f
		}
		stripped := false
		for i := range sn.Devs {
			if isAgentEndorse(sn.Devs[i].Endorse) {
				sn.Devs[i].Endorse, stripped = nil, true
			}
		}
		if !stripped {
			return f
		}
		return frame{Type: f.Type, Payload: proto.MustRaw(sn)}
	}
	return f
}

// The server withholds the owner's AgentEndorse for her agent's key, so
// the key looks like one more SSH key of hers, and then serves a
// KeyEndorse of that key by her (she was talked into endorsing a
// fingerprint on another computer). The KeyEndorse used to pin the agent's
// key as trust.Endorsed, owner-level: it could then sign revokes and
// endorse more keys. Now the device that set the agent up keeps the
// AgentEndorse itself and refuses to endorse the key; a client that sees
// both never lets the KeyEndorse count, whatever their order; and one that
// pinned it before the AgentEndorse reached it demotes it to the agent's.
func TestMaliciousAgentKeyEndorsedAsOwner(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	a := alice.Me().ID

	var withhold atomic.Bool
	withhold.Store(true)
	env.tp.setOut(func(_ uint64, f frame) []frame {
		if withhold.Load() {
			f = stripAgentEndorse(f)
		}
		return []frame{f}
	})

	if _, err := alice.RegisterAgent(ctx, "rocky", ""); err != nil {
		t.Fatal(err)
	}
	agent := startAgent(t, alice, "rocky")
	eventually(t, "agent online", agent.online)
	agentKey, err := identity.Load(agent.Config().KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	fpK := identity.Fingerprint(agentKey.PublicKey())
	howOf := func(u *user) trust.How {
		ks, _ := u.Trust().PinnedKeys(env.sid, a)
		for _, k := range ks {
			if k.Fingerprint == fpK {
				return k.How
			}
		}
		return ""
	}
	pendingFor := func(u *user) bool {
		ks, _ := u.Trust().PendingKeyChanges(env.sid)
		for _, k := range ks {
			if k.User == a && k.Fingerprint == fpK {
				return true
			}
		}
		return false
	}
	eventually(t, "bob sees the agent's key as a key change of alice's", func() bool { return pendingFor(bob) })
	eventually(t, "alice's own device knows it is her agent's", func() bool { return howOf(alice) == trust.Agent })

	if _, err := alice.EndorseKey(ctx, fpK); err == nil || !strings.Contains(err.Error(), "agent's key") {
		t.Fatalf("alice endorsed her agent's key as her own: %v", err)
	}

	// The KeyEndorse she signed elsewhere, served to everyone.
	aliceKey, err := identity.Load(alice.Config().KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	ke, err := identity.NewKeyEndorse(aliceKey, env.sid, a, agentKey.PublicKey(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	env.inject(t, proto.KKeyEndorse, 0, a, proto.KeyEndorseArgs{Endorse: proto.Signed(ke)})
	// Bob can't know better while the AgentEndorse is withheld.
	eventually(t, "bob pins the key on alice's endorsement", func() bool { return howOf(bob) == trust.Endorsed })
	time.Sleep(300 * time.Millisecond)
	if h := howOf(alice); h != trust.Agent {
		t.Fatalf("alice's device took her agent's key as hers on the KeyEndorse: %q", h)
	}

	// Once the AgentEndorse reaches him, the key is the agent's.
	withhold.Store(false)
	eventually(t, "bob demotes the key to the agent's", func() bool {
		if err := bob.RefreshDevices(ctx, a); err != nil {
			t.Fatal(err)
		}
		return howOf(bob) == trust.Agent
	})
	// A fresh device of bob's sees both at once.
	bob2 := openAs(t, bob)
	eventually(t, "bob2 online", bob2.online)
	eventually(t, "bob2 pins the key as the agent's", func() bool { return howOf(bob2) == trust.Agent })
	if h := howOf(alice); h != trust.Agent {
		t.Fatalf("alice: %q", h)
	}
}
