package client

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

// localClient opens a Client on a fresh home with no server: alice (id 1) is
// logged in and the cache is filled directly. Close it with closeLocal so
// the same home can be reopened.
func localClient(t *testing.T) (*Client, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "tc")
	if err := ensurePrivateDir(home); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(home, Config{Addr: "127.0.0.1:1", Pin: "sha256//x", UserID: alice, Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	return reopen(t, home), home
}

func reopen(t *testing.T, home string) *Client {
	t.Helper()
	c, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestDraftsPersistAcrossReopen(t *testing.T) {
	c, home := localClient(t)
	if err := c.SetDraft(general, 0, "half a thought"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetDraft(general, 55, "thread reply"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetDraft(dm, 0, "psst\nsecond line"); err != nil {
		t.Fatal(err)
	}
	if got := c.Draft(general, 0); got != "half a thought" {
		t.Fatalf("Draft = %q", got)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c = reopen(t, home)
	got := map[[2]uint64]string{}
	for _, d := range c.Drafts() {
		got[[2]uint64{d.Chan, d.Thread}] = d.Body
	}
	want := map[[2]uint64]string{
		{general, 0}: "half a thought", {general, 55}: "thread reply", {dm, 0}: "psst\nsecond line",
	}
	if len(got) != len(want) {
		t.Fatalf("drafts after reopen = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("draft %v = %q, want %q", k, got[k], v)
		}
	}
}

func TestDraftsEmptyRemoves(t *testing.T) {
	c, _ := localClient(t)
	c.SetDraft(general, 0, "x")
	c.SetDraft(general, 0, " \n\t")
	if d := c.Drafts(); len(d) != 0 {
		t.Fatalf("whitespace draft kept: %v", d)
	}
	if err := c.SetDraft(0, 0, "x"); err == nil {
		t.Fatal("draft without a channel accepted")
	}
	// Overwrite, not duplicate.
	c.SetDraft(general, 0, "one")
	c.SetDraft(general, 0, "two")
	if d := c.Drafts(); len(d) != 1 || d[0].Body != "two" {
		t.Fatalf("drafts = %v", d)
	}
}

func TestDraftsCappedAtMessageSize(t *testing.T) {
	c, _ := localClient(t)
	c.SetDraft(general, 0, strings.Repeat("é", proto.MaxBody)) // twice MaxBody bytes
	d := c.Draft(general, 0)
	if len(d) == 0 || len(d) > proto.MaxBody || strings.ContainsRune(d, '\uFFFD') {
		t.Fatalf("draft is %d bytes", len(d))
	}
}

func TestDraftsSurviveResetUnlessServerChanged(t *testing.T) {
	c, _ := localClient(t)
	if err := resetCache(c.db, serverID); err != nil { // first sync: nothing to compare with
		t.Fatal(err)
	}
	c.SetDraft(general, 0, "keep me")
	if err := resetCache(c.db, serverID); err != nil {
		t.Fatal(err)
	}
	if got := c.Draft(general, 0); got != "keep me" {
		t.Fatalf("draft after a reset of the same server = %q", got)
	}
	// Another server: its channel ids mean something else.
	if err := resetCache(c.db, bytes.Repeat([]byte{9}, 16)); err != nil {
		t.Fatal(err)
	}
	if got := c.Draft(general, 0); got != "" {
		t.Fatalf("draft survived a change of server: %q", got)
	}
}

func TestDraftsDroppedWithLeftChannel(t *testing.T) {
	c, _ := localClient(t)
	c.SetDraft(general, 0, "a")
	c.SetDraft(general, 7, "b")
	c.SetDraft(dm, 0, "c")
	if err := c.tx(func(tx *sql.Tx) error { return c.forgetChan(tx, general, alice) }); err != nil {
		t.Fatal(err)
	}
	d := c.Drafts()
	if len(d) != 1 || d[0].Chan != dm {
		t.Fatalf("drafts after leaving = %v", d)
	}
}

func TestWipeDropsDrafts(t *testing.T) {
	c, _ := localClient(t)
	c.SetDraft(general, 0, "a")
	if err := wipe(c.db); err != nil {
		t.Fatal(err)
	}
	if d := c.Drafts(); len(d) != 0 {
		t.Fatalf("drafts after wipe = %v", d)
	}
}
