package e2e

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
)

func (u *user) msgByID(ch, id uint64) (client.Message, bool) {
	for _, m := range u.Messages(ch, 200) {
		if m.ID == id {
			return m, true
		}
	}
	return client.Message{}, false
}

// Encrypted edit, reactions and delete between two real clients over the
// real server (step 8): each side applies only what verifies, the server
// stores tags and encs it can't read, and an admin's delete shows as not
// by the author.
func TestEncryptedEditReactDelete(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true) // admin
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	sid := alice.sid(t)
	a, b := alice.Me().ID, bob.Me().ID
	eventually(t, "devices verified both ways", func() bool {
		return alice.verifiedDevices(sid, b) == 1 && bob.verifiedDevices(sid, a) == 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dm, err := alice.OpenDM(ctx, []uint64{b})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob has the DM", func() bool { _, ok := bob.Channel(dm.ID); return ok })
	for _, body := range []string{"teh zebra plan", "second"} {
		if err := bob.Send(dm.ID, body, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := bob.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice reads bob", func() bool { sec, ok := alice.sec(dm.ID, "second"); return ok && sec == client.SecE2E })
	var id, id2 uint64
	for _, m := range alice.Messages(dm.ID, 10) {
		switch m.Body {
		case "teh zebra plan":
			id = m.ID
		case "second":
			id2 = m.ID
		}
	}

	// Edit.
	if err := bob.Edit(ctx, id, "the zebra plan"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice sees the edit", func() bool {
		m, ok := alice.msgByID(dm.ID, id)
		return ok && m.Body == "the zebra plan" && m.Edited != 0 && m.Sec == client.SecE2E
	})
	if res, err := alice.Search(ctx, "teh", dm.ID); err != nil || len(res) != 0 {
		t.Fatalf("search finds the old text: %+v %v", res, err)
	}
	if res, err := alice.Search(ctx, "zebra", dm.ID); err != nil || len(res) != 1 || res[0].Body != "the zebra plan" {
		t.Fatalf("search: %+v %v", res, err)
	}

	// Reactions: one emoji from both is one entry with two users.
	if err := alice.React(ctx, id, "👍"); err != nil {
		t.Fatal(err)
	}
	if err := bob.React(ctx, id, "👍"); err != nil {
		t.Fatal(err)
	}
	both := func(u *user) bool {
		m, ok := u.msgByID(dm.ID, id)
		return ok && len(m.Reactions) == 1 && len(m.Reactions["👍"]) == 2
	}
	eventually(t, "both see both reactions", func() bool { return both(alice) && both(bob) })
	// The server holds one opaque tag for both, never the emoji.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "tc.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tags []string
	rows, err := db.Query(`SELECT DISTINCT emoji FROM reactions WHERE msg = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var e string
		rows.Scan(&e)
		tags = append(tags, e)
	}
	rows.Close()
	if len(tags) != 1 || !e2ecrypto.ValidReactTag(tags[0]) {
		t.Fatalf("server reactions %q", tags)
	}
	if err := alice.Unreact(ctx, id, "👍"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob sees alice's reaction go", func() bool {
		m, _ := bob.msgByID(dm.ID, id)
		return len(m.Reactions["👍"]) == 1 && m.Reactions["👍"][0] == b
	})

	// The author's delete is signed; the admin's delete of bob's other
	// message isn't.
	if err := bob.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := alice.Delete(ctx, id2); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deletes applied", func() bool {
		m1, _ := alice.msgByID(dm.ID, id)
		m2, _ := bob.msgByID(dm.ID, id2)
		return m1.Deleted && m1.Sec == client.SecE2E && m1.Body == "" && m2.Deleted && m2.Sec == client.SecRemoved
	})
	if res, err := alice.Search(ctx, "zebra", dm.ID); err != nil || len(res) != 0 {
		t.Fatalf("deleted text still searchable: %+v %v", res, err)
	}
}
