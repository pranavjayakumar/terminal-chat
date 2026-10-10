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
	if _, err := proto.InstallCommand(res.Code); err != nil {
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
