package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
)

// msgsUpdated drains u's updates until one says ch's messages changed, or
// the timeout passes.
func msgsUpdated(u *user, ch uint64, d time.Duration) bool {
	t := time.After(d)
	for {
		select {
		case up := <-u.Updates():
			if up.Msgs[ch] || up.Reset {
				return true
			}
		case <-t:
			return false
		}
	}
}

// drainUpdates discards u's updates until none came for quiet.
func drainUpdates(u *user, quiet time.Duration) {
	for {
		select {
		case <-u.Updates():
		case <-time.After(quiet):
			return
		}
	}
}

// /encrypt marks the earlier plaintext legacy on every member's device; an
// open channel view must hear about it (Update.Msgs), or the UI keeps
// showing the old messages without the "unauthenticated (legacy)" marker
// until the user switches channels.
func TestQAEncryptChannelUpdatesOpenView(t *testing.T) {
	_, _, us := team(t, "alice", "bob")
	alice, bob := us[0], us[1]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := alice.CreateChannel(ctx, "team", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.AddMember(ctx, ch.ID, bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	alice.send(t, ctx, ch.ID, "plain old")
	eventually(t, "bob has it", func() bool { sec, ok := bob.sec(ch.ID, "plain old"); return ok && sec == client.SecPlain })
	eventually(t, "alice has it", func() bool { sec, ok := alice.sec(ch.ID, "plain old"); return ok && sec == client.SecPlain })
	// Let both clients settle, then forget what they said so far.
	drainUpdates(alice, 500*time.Millisecond)
	drainUpdates(bob, 500*time.Millisecond)
	if err := alice.EncryptChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	for _, u := range []*user{alice, bob} {
		if !msgsUpdated(u, ch.ID, 5*time.Second) {
			sec, _ := u.sec(ch.ID, "plain old")
			t.Errorf("%s: no message update for the channel after /encrypt (old message is %s)", u.Me().Name, sec)
		} else if sec, _ := u.sec(ch.ID, "plain old"); sec != client.SecLegacy {
			t.Errorf("%s: after the update the old message is %s, want legacy", u.Me().Name, sec)
		}
	}
}
