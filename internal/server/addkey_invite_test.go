package server

import (
	"testing"

	"github.com/skailama/tc/internal/proto"
)

// An add-key invite can't target a disabled account (it couldn't log in
// anyway) or an unknown one.
func TestAddKeyInviteTargets(t *testing.T) {
	ts := memberServer(t, "tcp")
	admin := ts.register(t, "admin", true)
	admin.syncFresh()
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	createInvite(t, admin, proto.InviteCreateArgs{User: bob.user.ID})
	admin.fail(proto.OpInviteCreate, proto.InviteCreateArgs{User: 9999}, proto.ErrNotFound)
	admin.ok(proto.OpUserDisable, proto.UserArgs{User: bob.user.ID}, nil)
	admin.fail(proto.OpInviteCreate, proto.InviteCreateArgs{User: bob.user.ID}, proto.ErrBadRequest)
}

// An add-key invite made before its target was disabled doesn't redeem
// (invite_invalid) and spends nothing: the account gets no key.
func TestAddKeyInviteTargetDisabledLater(t *testing.T) {
	for _, kind := range transports {
		t.Run(kind, func(t *testing.T) {
			ts := memberServer(t, kind)
			admin := ts.register(t, "admin", true)
			admin.syncFresh()
			bob := ts.register(t, "bob", false)
			bob.syncFresh()
			_, inv := createInvite(t, admin, proto.InviteCreateArgs{User: bob.user.ID})
			admin.ok(proto.OpUserDisable, proto.UserArgs{User: bob.user.ID}, nil)
			if _, bye := ts.connect(t, newKey(t), inv.Token, ""); bye != proto.ByeInviteInvalid {
				t.Fatalf("add-key invite for a disabled user: %q", bye)
			}
			var keys, uses int
			ts.st.db.QueryRow(`SELECT COUNT(*) FROM keys WHERE user = ?`, bob.user.ID).Scan(&keys)
			if keys != 1 {
				t.Fatalf("bob has %d keys", keys)
			}
			if err := ts.st.db.QueryRow(`SELECT uses FROM invites WHERE created_by = ? AND user = ?`,
				admin.user.ID, bob.user.ID).Scan(&uses); err != nil || uses != 1 {
				t.Fatalf("invite uses %d, %v", uses, err)
			}
		})
	}
}
