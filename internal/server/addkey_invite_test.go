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
