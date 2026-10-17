package e2e

import (
	"testing"
	"time"
)

// Breaker 2 (devfix): since B5 a revoke of any device of a roster user
// makes the channel's epoch stale for every member, so every member's next
// send rotates first. A sender that doesn't hold the current epoch's key
// yet (a freshly registered device: the one standing in for lost machines
// is the typical revoker) can't build the next bundle (its backlink needs
// that key). Its message must wait for the key like any "no key yet"
// message and go out once a member shares it, not fail for good with
// rotateReason (which nothing requeues).
func TestRevokeStaleSenderWithoutKey_BreakDevfix2(t *testing.T) {
	_, _, alice, laptop, bob, dm, ctx := shareSetup(t)
	sid := alice.sid(t)
	a := alice.Me().ID
	lost, err := laptop.Trust().DeviceKeys(sid, a)
	if err != nil {
		t.Fatal(err)
	}
	// Everyone holding the DM's key goes offline.
	for _, u := range []*user{alice, laptop, bob} {
		u.cancel()
		u.Close()
	}

	phone := secondDevice(t, alice)
	eventually(t, "phone registered", func() bool {
		return phone.online() && phone.DeviceError() == nil && phone.verifiedDevices(sid, a) == 3
	})
	if k := phone.epochKey(sid, dm, 1); k != nil {
		t.Fatal("the phone got epoch 1's key with every holder offline")
	}
	// The laptop was lost: revoke it from the phone. The phone can't rotate
	// (no key yet); that's fine, the next sender does.
	if _, err := phone.RevokeDevice(ctx, lost.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := phone.Send(dm, "phone after revoke", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second) // let the outbox try
	failed := phone.brFailed(dm)
	t.Logf("phone's message while nobody can share the key: failed=%q", failed)

	// Bob comes back and shares epoch 1's key with the phone (key.need).
	bob2 := reopen(t, bob.home)
	eventually(t, "bob online", bob2.online)
	deadline := time.Now().Add(75 * time.Second)
	for time.Now().Before(deadline) {
		if bob2.reads(dm, "phone after revoke")() {
			if _, ok := laptop.sec(dm, "phone after revoke"); ok {
				t.Fatal("the revoked laptop decrypted the phone's message")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the phone's message never went out after it got the key (phone has epoch 1 key: %v, failed=%q, head=%+v)",
		phone.epochKey(sid, dm, 1) != nil, phone.brFailed(dm), phone.status(dm))
}
