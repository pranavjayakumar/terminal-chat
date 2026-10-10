package e2e

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// An admin's /invite 3 1h through the real client: three people join with
// it, the fourth is refused, and non-admins can't mint one.
func TestTeamInviteViaClient(t *testing.T) {
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	admin := login(t, s, addr, "admin", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return admin.online() && bob.online() })
	ctx := context.Background()

	res, err := admin.CreateTeamInvite(ctx, 3, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Uses != 3 || time.Until(time.UnixMilli(res.Expires)) > time.Hour {
		t.Fatalf("res %+v", res)
	}
	if _, err := proto.InstallCommand(res.Code, "dev"); err != nil {
		t.Fatalf("server's code isn't shell-safe: %v", err)
	}
	for i := range 4 {
		home := t.TempDir()
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := client.Login(lctx, home, res.Code, fmt.Sprintf("member%d", i), writeKey(t, home))
		cancel()
		var bye *client.ByeError
		switch {
		case i < 3 && err != nil:
			t.Fatalf("member%d: %v", i, err)
		case i == 3 && (!errors.As(err, &bye) || bye.Code != proto.ByeInviteInvalid):
			t.Fatalf("4th join: %v", err)
		}
	}
	var pe *proto.Error
	if _, err := bob.CreateTeamInvite(ctx, 2, 0); !errors.As(err, &pe) || pe.Code != proto.ErrForbidden {
		t.Fatalf("non-admin: %v", err)
	}
	if _, err := admin.CreateTeamInvite(ctx, 101, 0); !errors.As(err, &pe) || pe.Code != proto.ErrBadRequest {
		t.Fatalf("101 uses: %v", err)
	}
}

// /invites and /invite-revoke through the real client: a revoked invite
// stops working at once.
func TestInviteListRevokeViaClient(t *testing.T) {
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	admin := login(t, s, addr, "admin", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return admin.online() && bob.online() })
	ctx := context.Background()

	res, err := admin.CreateTeamInvite(ctx, 5, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	invs, err := admin.ListInvites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, inv := range invs {
		if inv.Kind == proto.InviteKindUser && inv.UsesLeft == 5 && inv.CreatedBy == admin.Me().ID {
			id = inv.ID
		}
	}
	if id == "" {
		t.Fatalf("team invite not listed: %+v", invs)
	}
	var pe *proto.Error
	if _, err := bob.ListInvites(ctx); !errors.As(err, &pe) || pe.Code != proto.ErrForbidden {
		t.Fatalf("non-admin list: %v", err)
	}
	if err := bob.RevokeInvite(ctx, id); !errors.As(err, &pe) || pe.Code != proto.ErrForbidden {
		t.Fatalf("non-admin revoke: %v", err)
	}
	if err := admin.RevokeInvite(ctx, id); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var bye *client.ByeError
	if _, err := client.Login(lctx, home, res.Code, "late", writeKey(t, home)); !errors.As(err, &bye) || bye.Code != proto.ByeInviteInvalid {
		t.Fatalf("join with revoked invite: %v", err)
	}
}
