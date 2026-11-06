package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/client"
)

// Issue #188: after a server restore, a client offline through it must not
// keep messages from the lost history, whose ids the restored server hands
// out again, nor miss the new messages that reuse them.

// reopen stops u's client and starts it again over the same TC_HOME.
func (u *user) reopen(t *testing.T) {
	t.Helper()
	u.offline(t)
	u.online2(t)
}

func (u *user) offline(t *testing.T) {
	t.Helper()
	u.cancel()
	u.Client.Close()
}

func (u *user) online2(t *testing.T) {
	t.Helper()
	c, err := client.Open(u.home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	u.Client, u.cancel = c, cancel
	t.Cleanup(func() { cancel(); c.Close() })
}

func cacheMeta(t *testing.T, home, k string) []byte {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v []byte
	if err := db.QueryRow(`SELECT v FROM meta WHERE k = ?`, k).Scan(&v); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return v
}

func copyFileT(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreNoStaleMessages(t *testing.T) {
	for _, how := range []string{"sqlite-backup", "cp"} {
		t.Run(how, func(t *testing.T) {
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
			for _, b := range []string{"pre 1", "pre 2"} {
				alice.send(t, ctx, gen, b)
			}
			eventually(t, "bob has pre", func() bool { return bob.has(gen, "pre 2") })
			if len(cacheMeta(t, bob.home, "server_inc")) != 16 {
				t.Fatal("client didn't store the incarnation")
			}

			switch how {
			case "sqlite-backup": // sqlite3 tc.db ".backup f", server running
				db, err := sql.Open("sqlite", filepath.Join(dir, "tc.db"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`VACUUM INTO ?`, backup); err != nil {
					t.Fatal(err)
				}
				db.Close()
			case "cp": // cp of the stopped server's tc.db
				s.stop()
				copyFileT(t, filepath.Join(dir, "tc.db"), backup)
				s, _ = startServer(t, dir, addr)
				eventually(t, "online", func() bool { return alice.online() && bob.online() })
			}

			for i := 1; i <= 4; i++ {
				alice.send(t, ctx, gen, fmt.Sprintf("lost %d", i))
			}
			eventually(t, "bob has lost", func() bool { return bob.has(gen, "lost 4") })
			bob.offline(t)

			s.stop()
			for _, sfx := range []string{"", "-wal", "-shm"} {
				os.Remove(filepath.Join(dir, "tc.db"+sfx))
			}
			copyFileT(t, backup, filepath.Join(dir, "tc.db"))
			s, _ = startServer(t, dir, addr)
			eventually(t, "alice online", alice.online)
			// More than were lost: the restored head passes bob's cursor
			// before he reconnects.
			var want []string
			for i := 1; i <= 10; i++ {
				b := fmt.Sprintf("new %d", i)
				alice.send(t, ctx, gen, b)
				want = append(want, b)
			}
			bob.online2(t)
			for _, u := range []*user{alice, bob} {
				eventually(t, "has every new message", func() bool {
					for _, b := range want {
						if !u.has(gen, b) {
							return false
						}
					}
					return true
				})
				ids := map[uint64]string{}
				seen := map[string]int{}
				for _, m := range u.Messages(gen, 200) {
					if strings.HasPrefix(m.Body, "lost") {
						t.Errorf("%s still has %q (id %d) from the lost history", u.Me().Name, m.Body, m.ID)
					}
					if prev, ok := ids[m.ID]; ok && m.ID != 0 {
						t.Errorf("%s: id %d is both %q and %q", u.Me().Name, m.ID, prev, m.Body)
					}
					ids[m.ID] = m.Body
					seen[m.Body]++
				}
				for _, b := range append([]string{"pre 1", "pre 2"}, want...) {
					if seen[b] != 1 {
						t.Errorf("%s has %q %d times", u.Me().Name, b, seen[b])
					}
				}
			}

			// A plain reconnect afterwards replays: nothing to re-lose.
			before := cacheMeta(t, bob.home, "server_inc")
			bob.reopen(t)
			eventually(t, "bob online", bob.online)
			alice.send(t, ctx, gen, "after")
			eventually(t, "bob live", func() bool { return bob.has(gen, "after") && bob.has(gen, "new 1") })
			if string(cacheMeta(t, bob.home, "server_inc")) != string(before) {
				t.Fatal("incarnation changed on a plain reconnect")
			}
		})
	}
}
