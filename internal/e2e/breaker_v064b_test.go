package e2e

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A read marker bob moved while the server was down (dirty: never
// confirmed) points into the history a restore throws away. After the
// reset it must not mark the restored server's new messages, which reuse
// those ids, as read: not in bob's cache, and not on the server (where it
// would mark them read on all of bob's devices).
func TestRestoreDirtyReadMarker_Break(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	dir := t.TempDir()
	backup := filepath.Join(t.TempDir(), "tc.db")
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	gen := alice.channel("general")
	ctx := ctxRestore(t)
	alice.send(t, ctx, gen, "pre")
	eventually(t, "bob has pre", func() bool { return bob.has(gen, "pre") })
	pre, _ := bob.msg(gen, "pre")
	if err := bob.MarkRead(gen, pre.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "server has bob's marker", func() bool {
		var seq uint64
		db, err := sql.Open("sqlite", filepath.Join(dir, "tc.db"))
		if err != nil {
			return false
		}
		defer db.Close()
		db.QueryRow(`SELECT seq FROM reads WHERE user = ? AND chan = ?`, bob.Me().ID, gen).Scan(&seq)
		return seq == pre.ID
	})

	db, err := sql.Open("sqlite", filepath.Join(dir, "tc.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`VACUUM INTO ?`, backup); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for i := 1; i <= 6; i++ {
		alice.send(t, ctx, gen, fmt.Sprintf("lost %d", i))
	}
	eventually(t, "bob has lost", func() bool { return bob.has(gen, "lost 6") })
	var lastLost uint64
	for _, m := range bob.Messages(gen, 50) {
		lastLost = max(lastLost, m.ID)
	}

	// The server goes down; bob reads what he has, offline.
	s.stop()
	eventually(t, "bob offline", func() bool { return !bob.online() })
	if err := bob.MarkRead(gen, lastLost); err != nil {
		t.Fatal(err)
	}
	bob.offline(t)

	for _, sfx := range []string{"", "-wal", "-shm"} {
		os.Remove(filepath.Join(dir, "tc.db"+sfx))
	}
	copyFileT(t, backup, filepath.Join(dir, "tc.db"))
	s, _ = startServer(t, dir, addr)
	eventually(t, "alice online", alice.online)
	var want []string
	for i := 1; i <= 10; i++ {
		b := fmt.Sprintf("new %d", i)
		alice.send(t, ctx, gen, b)
		want = append(want, b)
	}

	bob.online2(t)
	eventually(t, "bob has the new messages", func() bool { return bob.online() && bob.has(gen, "new 10") })
	time.Sleep(300 * time.Millisecond) // let pushed markers settle
	unread := func(u *user) int {
		v, _ := u.Channel(gen)
		return v.Unread
	}
	if n := unread(bob); n != len(want) {
		v, _ := bob.Channel(gen)
		t.Errorf("bob sees %d unread (read marker %d), want %d: a marker from the lost history read the new messages", n, v.Read, len(want))
	}
	// What the server keeps for bob: a second device of his syncs from scratch.
	bob.reopen(t)
	eventually(t, "bob online again", bob.online)
	if n := unread(bob); n != len(want) {
		t.Errorf("after a reconnect bob sees %d unread, want %d", n, len(want))
	}
	b2 := secondDevice(t, bob)
	eventually(t, "second device synced", func() bool { return b2.online() && b2.has(gen, "new 10") })
	if n := unread(b2); n != len(want) {
		v, _ := b2.Channel(gen)
		t.Errorf("bob's other device sees %d unread (server marker %d), want %d", n, v.Read, len(want))
	}
}
