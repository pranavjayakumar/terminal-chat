package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
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

// A member's /invite and /invite --key github:carol through the real
// client and server: the member sees only their own invites, and the
// key-bound one admits carol's key and nobody else's.
func TestMemberAndKeyInvitesViaClient(t *testing.T) {
	s, addr := startServerConfig(t, "127.0.0.1:0", server.Config{DataDir: t.TempDir(), MemberInvites: true})
	admin := login(t, s, addr, "admin", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return admin.online() && bob.online() })
	ctx := context.Background()

	if _, err := admin.CreateTeamInvite(ctx, 20, 0); err != nil {
		t.Fatal(err)
	}
	team, err := bob.CreateTeamInvite(ctx, 3, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var pe *proto.Error
	if _, err := bob.CreateTeamInvite(ctx, 11, 0); !errors.As(err, &pe) || pe.Code != proto.ErrBadRequest {
		t.Fatalf("member, 11 uses: %v", err)
	}

	carolHome := t.TempDir()
	carolKey := writeKey(t, carolHome)
	pub, err := identity.Load(carolKey)
	if err != nil {
		t.Fatal(err)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/carol.keys" {
			http.NotFound(w, r)
			return
		}
		w.Write(ssh.MarshalAuthorizedKey(pub.PublicKey()))
	}))
	defer gh.Close()
	bob.SetGitHubBase(gh.URL)
	bound, err := bob.CreateGitHubInvite(ctx, "carol", 0)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Keys != 1 || bound.Uses != 1 {
		t.Fatalf("bound %+v", bound)
	}

	mine, err := bob.ListInvites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 {
		t.Fatalf("bob's list %+v", mine)
	}
	for _, inv := range mine {
		if inv.CreatedBy != bob.Me().ID {
			t.Fatalf("bob sees someone else's invite %+v", inv)
		}
		if inv.Keys == 1 && inv.KeySrc != "github:carol" {
			t.Fatalf("bound invite listed as %+v", inv)
		}
	}
	if all, _ := admin.ListInvites(ctx); len(all) != 3 {
		t.Fatalf("admin's list %+v", all)
	}

	join := func(code, name, home, key string) error {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := client.Login(lctx, home, code, name, key)
		return err
	}
	malHome := t.TempDir()
	var bye *client.ByeError
	if err := join(bound.Code, "mallory", malHome, writeKey(t, malHome)); !errors.As(err, &bye) || bye.Code != proto.ByeInviteInvalid {
		t.Fatalf("leaked bound invite with another key: %v", err)
	}
	if err := join(bound.Code, "carol", carolHome, carolKey); err != nil {
		t.Fatalf("carol: %v", err)
	}
	dHome := t.TempDir()
	if err := join(team.Code, "dave", dHome, writeKey(t, dHome)); err != nil {
		t.Fatalf("dave via bob's team invite: %v", err)
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
