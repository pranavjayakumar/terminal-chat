package server

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

// Deleted text must not survive in the database file, including freed pages
// and the full-text index, once the WAL is checkpointed.
func TestDeletedTextGoneFromDisk(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	snap, _ := alice.syncFresh()
	gen := generalID(t, snap)
	const secret = "zqhunter2launchcodesqz"
	msg := alice.send(gen, "the password is "+secret).Msg
	alice.ok(proto.OpEdit, proto.EditArgs{Msg: msg, Body: "edited " + secret}, nil)
	alice.ok(proto.OpDelete, proto.MsgArgs{Msg: msg}, nil)
	if _, err := ts.st.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(ts.cfg.DataDir, "tc.db*"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("deleted text still present in %s", filepath.Base(f))
		}
	}
}
