package server

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

// users.list tells admins who invited each account (B3), so the accounts a
// disabled member brought in stay traceable after their invites are gone.
func TestUsersListInvitedBy(t *testing.T) {
	for _, kind := range transports {
		t.Run(kind, func(t *testing.T) {
			ts := memberServer(t, kind)
			admin := ts.register(t, "admin", true)
			admin.syncFresh()
			bob := ts.register(t, "bob", false)
			bob.syncFresh()
			_, inv := createInvite(t, bob, proto.InviteCreateArgs{Uses: 2})
			carol, bye := ts.connect(t, newKey(t), inv.Token, "carol")
			if bye != "" {
				t.Fatal(bye)
			}
			_, ainv := createInvite(t, admin, proto.InviteCreateArgs{})
			dave, bye := ts.connect(t, newKey(t), ainv.Token, "dave")
			if bye != "" {
				t.Fatal(bye)
			}
			// An add-key redemption creates no account and records nothing.
			_, addKey := createInvite(t, admin, proto.InviteCreateArgs{User: carol.user.ID})
			if _, bye := ts.connect(t, newKey(t), addKey.Token, ""); bye != "" {
				t.Fatal(bye)
			}

			var res proto.UsersRes
			bob.ok(proto.OpUsersList, nil, &res)
			if len(res.InvitedBy) != 0 || len(res.Users) != 4 {
				t.Fatalf("member's users.list %+v", res)
			}
			admin.ok(proto.OpUserDisable, proto.UserArgs{User: bob.user.ID}, nil)
			res = proto.UsersRes{}
			admin.ok(proto.OpUsersList, nil, &res)
			want := map[uint64]uint64{admin.user.ID: 0, bob.user.ID: 0, carol.user.ID: bob.user.ID, dave.user.ID: admin.user.ID}
			if fmt.Sprint(res.InvitedBy) != fmt.Sprint(want) {
				t.Fatalf("invited_by %v, want %v", res.InvitedBy, want)
			}
		})
	}
}

// A database from before users.invited_by keeps its users, with no inviter
// on record.
func TestInvitedByMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "tc.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
		display TEXT NOT NULL DEFAULT '', admin INTEGER NOT NULL DEFAULT 0, disabled INTEGER NOT NULL DEFAULT 0,
		created INTEGER NOT NULL);
		INSERT INTO users (name, admin, created) VALUES ('old', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := openStore(filepath.Join(dir, "tc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	by, err := invitedBy(context.Background(), st.db)
	if err != nil || len(by) != 0 {
		t.Fatalf("invited_by %v, %v", by, err)
	}
	if us, err := listUsers(context.Background(), st.db); err != nil || len(us) != 1 || us[0].Name != "old" {
		t.Fatalf("users %+v, %v", us, err)
	}
	if st2, err := openStore(filepath.Join(dir, "tc.db")); err == nil { // idempotent
		st2.close()
	} else {
		t.Fatal(err)
	}
}
