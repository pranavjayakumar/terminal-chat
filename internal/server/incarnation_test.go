package server

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

// Issue #188: a server restored from a backup reuses seqs (message ids) from
// the backup point on. It must notice and start a new incarnation, so clients
// with a cursor from the lost history get a reset instead of a replay.

func stopServer(ts *testServer) {
	ts.stop()
	ts.Close()
}

// sqliteBackup is what `sqlite3 tc.db ".backup f"` does: a consistent copy
// of the live database, taken while the server runs.
func sqliteBackup(t *testing.T, ts *testServer) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "backup.db")
	if _, err := ts.st.db.Exec(`VACUUM INTO ?`, p); err != nil {
		t.Fatal(err)
	}
	return p
}

// cpBackup copies a stopped server's database files.
func cpBackup(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "backup.db")
	copyFile(t, filepath.Join(dir, "tc.db"), p)
	if _, err := os.Stat(filepath.Join(dir, "tc.db-wal")); err == nil {
		copyFile(t, filepath.Join(dir, "tc.db-wal"), p+"-wal")
	}
	return p
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// restoreDB puts a backup back as the (stopped) server's database, leaving
// the rest of the data dir as it is.
func restoreDB(t *testing.T, dir, backup string) {
	t.Helper()
	for _, s := range []string{"", "-wal", "-shm"} {
		os.Remove(filepath.Join(dir, "tc.db"+s))
	}
	copyFile(t, backup, filepath.Join(dir, "tc.db"))
	if _, err := os.Stat(backup + "-wal"); err == nil {
		copyFile(t, backup+"-wal", filepath.Join(dir, "tc.db-wal"))
	}
}

func (c *tclient) syncFrom(since uint64, sid, inc []byte) (proto.SyncRes, int) {
	c.t.Helper()
	c.nextID++
	c.codec.Write(proto.TReq, proto.Req{ID: c.nextID, Op: proto.OpSync,
		Args: proto.MustRaw(proto.SyncArgs{Since: since, ServerID: sid, Inc: inc})})
	evs := 0
	for {
		f := c.recv("sync")
		switch f.Type {
		case proto.TEvent:
			evs++
		case proto.TRes:
			var r proto.Res
			f.Decode(&r)
			var sr proto.SyncRes
			proto.Unmarshal(r.Data, &sr)
			if !r.OK {
				c.t.Fatalf("sync: %v", r.Err)
			}
			return sr, evs
		}
	}
}

func TestIncarnationRestore(t *testing.T) {
	for _, how := range []string{"sqlite-backup", "cp"} {
		t.Run(how, func(t *testing.T) {
			dir := t.TempDir()
			ts := startServerCfg(t, "tcp", Config{DataDir: dir})
			if !slices.Contains(ts.register(t, "probe", false).welcome.Caps, proto.CapIncarnation) {
				t.Fatal("WELCOME lacks cap incarnation")
			}
			alice := ts.register(t, "alice", true)
			bob := ts.register(t, "bob", false)
			snap, sr0 := alice.syncFresh()
			gen := generalID(t, snap)
			if len(sr0.Inc) != 16 {
				t.Fatalf("sync RES inc = %x", sr0.Inc)
			}
			_, srb := bob.syncFresh()
			alice.send(gen, "before")

			var backup string
			if how == "sqlite-backup" {
				backup = sqliteBackup(t, ts)
			} else {
				stopServer(ts)
				backup = cpBackup(t, dir)
				ts = startServerCfg(t, "tcp", Config{DataDir: dir})
				alice, _ = ts.connect(t, alice.signer, "", "")
				alice.syncFresh()
			}
			// A plain restart keeps the incarnation: no needless resets.
			if !bytes.Equal(ts.inc.inc, sr0.Inc) {
				t.Fatal("restart changed the incarnation")
			}
			var lost uint64
			for _, b := range []string{"lost 1", "lost 2", "lost 3"} {
				lost = alice.send(gen, b).Msg
			}
			stopServer(ts)

			restoreDB(t, dir, backup)
			ts = startServerCfg(t, "tcp", Config{DataDir: dir})
			if bytes.Equal(ts.inc.inc, sr0.Inc) {
				t.Fatal("restored database kept the incarnation")
			}
			alice, _ = ts.connect(t, alice.signer, "", "")
			alice.syncFresh()
			var last uint64
			for i := 0; i < 8; i++ {
				last = alice.send(gen, "new").Msg
			}
			if last <= lost {
				t.Fatalf("test needs the head (%d) past the lost cursor (%d)", last, lost)
			}

			// The bug: a cursor from the lost history, now below the head,
			// got a replay that skipped the reused ids.
			c, _ := ts.connect(t, bob.signer, "", "")
			sr, _ := c.syncFrom(lost, ts.st.serverID, sr0.Inc)
			if !sr.Reset || !bytes.Equal(sr.Inc, ts.inc.inc) {
				t.Fatalf("old-incarnation cursor: reset=%v inc=%x", sr.Reset, sr.Inc)
			}
			// The current incarnation replays as before.
			c, _ = ts.connect(t, bob.signer, "", "")
			if sr, n := c.syncFrom(last-1, ts.st.serverID, ts.inc.inc); sr.Reset || n == 0 {
				t.Fatalf("current-incarnation cursor: reset=%v events=%d", sr.Reset, n)
			}
			// An old client (no inc) gets what it always got.
			c, _ = ts.connect(t, bob.signer, "", "")
			if sr, _ := c.syncFrom(srb.Head, ts.st.serverID, nil); sr.Reset {
				t.Fatal("old client without inc got a reset")
			}

			// The new incarnation survives restarts too.
			cur := ts.inc.inc
			stopServer(ts)
			ts = startServerCfg(t, "tcp", Config{DataDir: dir})
			if !bytes.Equal(ts.inc.inc, cur) {
				t.Fatal("restart after a restore changed the incarnation again")
			}
		})
	}
}

// openInc reopens dir's server (stopped) and returns its incarnation.
func openInc(t *testing.T, dir string) []byte {
	t.Helper()
	ts := startServerCfg(t, "tcp", Config{DataDir: dir})
	defer stopServer(ts)
	return ts.inc.inc
}

func TestIncarnationEdges(t *testing.T) {
	dir := t.TempDir()
	ts := startServerCfg(t, "tcp", Config{DataDir: dir})
	alice := ts.register(t, "alice", true)
	snap, _ := alice.syncFresh()
	gen := generalID(t, snap)
	alice.send(gen, "one")
	inc := ts.inc.inc
	backup := sqliteBackup(t, ts)
	whole := t.TempDir()
	alice.send(gen, "two")
	stopServer(ts)
	for _, f := range []string{"tc.db", incarnationFile, "server.key", "server.crt"} {
		copyFile(t, filepath.Join(dir, f), filepath.Join(whole, f))
	}

	// A crash leaves the file behind the database: still the same history.
	in, _ := readIncFile(filepath.Join(dir, incarnationFile))
	ts = startServerCfg(t, "tcp", Config{DataDir: dir})
	alice, _ = ts.connect(t, alice.signer, "", "")
	alice.syncFresh()
	alice.send(gen, "three")
	ts.stop() // Serve's loop records; then pretend the last write was lost
	ts.Close()
	writeFileAtomic(filepath.Join(dir, incarnationFile), []byte(incLine(in)))
	if got := openInc(t, dir); !bytes.Equal(got, inc) {
		t.Fatal("a lagging mark changed the incarnation")
	}

	// Restored, then written past the mark by an older server that keeps
	// no file: the seq exists again but holds another event.
	restoreDB(t, dir, backup)
	ts = startServerCfg(t, "tcp", Config{DataDir: dir})
	if bytes.Equal(ts.inc.inc, inc) {
		t.Fatal("restore not detected")
	}
	alice, _ = ts.connect(t, alice.signer, "", "")
	alice.syncFresh()
	var head uint64
	for i := 0; i < 5; i++ {
		head = alice.send(gen, "more").Msg
	}
	stopServer(ts)
	if head < in.seq {
		t.Fatalf("test needs the head (%d) past the mark (%d)", head, in.seq)
	}
	writeFileAtomic(filepath.Join(dir, incarnationFile), []byte(incLine(in)))
	if got := openInc(t, dir); bytes.Equal(got, inc) {
		t.Fatal("a mismatching event at the mark kept the incarnation")
	}

	// Malformed or missing file: a new incarnation (clients resync once).
	cur := openInc(t, dir)
	os.WriteFile(filepath.Join(dir, incarnationFile), []byte("garbage"), 0o600)
	if got := openInc(t, dir); bytes.Equal(got, cur) {
		t.Fatal("malformed file kept the incarnation")
	}
	cur = openInc(t, dir)
	os.Remove(filepath.Join(dir, incarnationFile))
	if got := openInc(t, dir); bytes.Equal(got, cur) {
		t.Fatal("missing file kept the incarnation")
	}

	// Documented limit: restoring the whole directory, file included, is
	// indistinguishable from never having left it; deleting the file after
	// such a restore is the way out.
	if got := openInc(t, whole); !bytes.Equal(got, inc) {
		t.Fatal("whole-dir copy: expected the old incarnation (documented limit)")
	}
	if fi, err := os.Stat(filepath.Join(dir, incarnationFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("incarnation file mode: %v %v", fi, err)
	}
}

func incLine(st incState) string {
	return fmt.Sprintf("v1 %x %x %d %x\n", st.serverID, st.inc, st.seq, st.mark)
}
