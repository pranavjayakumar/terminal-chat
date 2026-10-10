package client

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/trust"
)

const secChan = 300

// alice's encrypted channel at epoch 1 (made by her client on its own).
func secSetup(t *testing.T, devs []proto.Device, prep func(c *Client)) (*e2eFake, *Client) {
	t.Helper()
	fs, c := e2eClient(t)
	if prep != nil {
		prep(c)
	}
	run(t, c)
	sec := proto.Channel{ID: secChan, Name: "sec", Kind: proto.ChanPrivate, Members: []uint64{alice}, E2E: true, Creator: alice}
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{sec}, Devs: devs}, proto.DevicesListRes{})
	f.myKeys(c)
	waitFor(t, "epoch 1", func() bool { st := c.E2EStatus(secChan); return st.State == E2EOK && st.Epoch == 1 })
	return f, c
}

// Adding someone to an encrypted channel whose devices this device can't
// verify is refused before anything is sent, naming them.
func TestAddMemberRefusesUnverified(t *testing.T) {
	old := newSSH(t)
	cp := newPeer(t, carol, nil) // under a key other than the one pinned
	var adds atomic.Int32
	f, c := secSetup(t, []proto.Device{{Cert: cp.cert}}, func(c *Client) {
		if err := c.Trust().PinKey(testSID(), carol, ssh.FingerprintSHA256(old.PublicKey()), old.PublicKey().Marshal(), trust.TOFU); err != nil {
			t.Fatal(err)
		}
	})
	f.mu.Lock()
	f.onOp = func(f *e2eFake, r proto.Req) bool {
		if r.Op == proto.OpChanAdd {
			adds.Add(1)
		}
		return false
	}
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.AddMember(ctx, secChan, carol)
	var ue *UnverifiedMembersError
	if !errors.As(err, &ue) || !slices.Equal(ue.Users, []uint64{carol}) || ue.Error() != "can't verify any device of: @carol" {
		t.Fatalf("AddMember = %v", err)
	}
	if n := adds.Load(); n != 0 {
		t.Fatalf("sent %d chan.add anyway", n)
	}
}

// AddMember in an encrypted channel sends chan.add with the next epoch
// (roster + the new member, boxed to their verified device, backlinked);
// RemoveMember sends chan.remove with an epoch without them.
func TestAddRemoveMemberCarryBundles(t *testing.T) {
	bp := newPeer(t, bob, nil)
	f, c := secSetup(t, []proto.Device{{Cert: bp.cert}}, nil)
	reqs := make(chan proto.ChanAddArgs, 4)
	f.mu.Lock()
	f.onOp = func(f *e2eFake, r proto.Req) bool {
		if r.Op != proto.OpChanAdd && r.Op != proto.OpChanRemove {
			return false
		}
		var a proto.ChanAddArgs
		proto.Unmarshal(r.Args, &a)
		reqs <- a
		ch, _ := c.Channel(secChan)
		members := slices.DeleteFunc(slices.Clone(ch.Members), func(u uint64) bool { return u == a.User })
		if r.Op == proto.OpChanAdd {
			members = append(members, a.User)
			ch.Channel.Members = members
			f.event(proto.KChanJoin, secChan, alice, proto.ChanJoin{User: a.User, Chan: ch.Channel})
		} else {
			f.event(proto.KChanLeave, secChan, alice, proto.UserRef{User: a.User})
		}
		if a.Bundle != nil {
			f.keyEpoch(secChan, alice, *a.Bundle)
		}
		f.ok(r.ID, proto.Empty{})
		return true
	}
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.AddMember(ctx, secChan, bob); err != nil {
		t.Fatal(err)
	}
	a := <-reqs
	if a.User != bob || a.Bundle == nil {
		t.Fatalf("chan.add %+v", a)
	}
	ep, ok, _ := c.Trust().Epoch(testSID(), secChan, 2)
	if !ok || !slices.Equal(ep.Roster, []uint64{alice, bob}) || ep.Key == nil || len(ep.Back) == 0 {
		t.Fatalf("epoch 2 %+v", ep)
	}
	if err := c.RemoveMember(ctx, secChan, bob); err != nil {
		t.Fatal(err)
	}
	if a := <-reqs; a.User != bob || a.Bundle == nil {
		t.Fatalf("chan.remove %+v", a)
	}
	if ep, _, _ := c.Trust().Epoch(testSID(), secChan, 3); !slices.Equal(ep.Roster, []uint64{alice}) {
		t.Fatalf("epoch 3 roster %v", ep.Roster)
	}
}

// Removed by someone else, a private channel stays with what we had and
// says so; a leave of our own forgets it; Leave closes a removed one.
func TestRemovedChannelKeepsHistory(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	team := proto.Channel{ID: secChan, Name: "team", Kind: proto.ChanPrivate, Members: []uint64{alice, bob}, Creator: bob}
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{team}}, proto.DevicesListRes{})
	waitFor(t, "online", online(c))
	f.post(proto.Message{Chan: secChan, Author: bob, Body: "hello"})
	waitFor(t, "message", func() bool { return len(c.Messages(secChan, 10)) == 1 })
	f.event(proto.KChanLeave, secChan, bob, proto.UserRef{User: alice})
	waitFor(t, "removed", func() bool { v, ok := c.Channel(secChan); return ok && v.Removed && !v.Member })
	if ms := c.Messages(secChan, 10); len(ms) != 1 || ms[0].Body != "hello" {
		t.Fatalf("history after removal: %+v", ms)
	}
	// Re-added: no longer removed.
	team.Members = []uint64{alice, bob}
	f.event(proto.KChanJoin, secChan, bob, proto.ChanJoin{User: alice, Chan: team})
	waitFor(t, "re-added", func() bool { v, _ := c.Channel(secChan); return v.Member && !v.Removed })
	f.event(proto.KChanLeave, secChan, bob, proto.UserRef{User: alice})
	waitFor(t, "removed again", func() bool { v, _ := c.Channel(secChan); return v.Removed })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Leave(ctx, secChan); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Channel(secChan); ok || len(c.Messages(secChan, 10)) != 0 {
		t.Fatal("closing a removed channel kept it")
	}
}
