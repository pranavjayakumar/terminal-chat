package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// sparseFile makes a size-byte file of zeros without writing them.
func sparseFile(t *testing.T, dir, name string, size int64) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

// One message of eight 25 MiB files (the default --max-upload; ten are
// allowed) is 200 MiB of blob.put, past the per-user bucket's burst of 400
// chunks (100 MiB), so on a fast link the server answers `rate_limited`.
// uploadLoop treats a *proto.Error whose code permanent() calls transient
// (rate_limited, internal) as a "local error" (files.go: the third case,
// `!isTransient(err)`), so the whole upload is marked failed for good
// instead of backing off and resuming.
func TestFilesUploadRateLimited_BreakFiles(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", false)
	eventually(t, "online", alice.online)
	gen := alice.channel("general")
	src := t.TempDir()
	var paths []string
	for i := range 8 {
		paths = append(paths, sparseFile(t, src, fmt.Sprintf("part%d.bin", i), proto.DefaultMaxUpload))
	}
	local, err := alice.SendFiles(gen, 0, "", paths...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := alice.WaitUpload(ctx, local); err != nil {
		t.Fatalf("upload of 8 files within the default limits: %v", err)
	}
}

// Downloads have the same problem: fetchBlob returns blob.get's
// `rate_limited` as the download's error instead of waiting, so fetching
// a handful of 25 MiB files (more than 100 MiB together) fails.
func TestFilesDownloadRateLimited_BreakFiles(t *testing.T) {
	f := setupFiles(t, false)
	src := t.TempDir()
	// Three files, sent one at a time with a pause so the uploads stay
	// inside alice's own bucket.
	for i := range 3 {
		local, err := f.alice.SendFiles(f.ch, 0, fmt.Sprintf("f%d", i), sparseFile(t, src, fmt.Sprintf("f%d.bin", i), proto.DefaultMaxUpload))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.alice.WaitUpload(f.ctx, local); err != nil {
			t.Fatal(err)
		}
	}
	var ms []client.Message
	eventually(t, "bob has all three", func() bool {
		ms = ms[:0]
		for _, m := range f.bob.Messages(f.ch, 50) {
			if m.ID != 0 && len(m.Files) == 1 {
				ms = append(ms, m)
			}
		}
		return len(ms) == 3
	})
	dl := t.TempDir()
	for round := range 2 {
		for _, m := range ms {
			if _, err := f.bob.Download(f.ctx, m.ID, 0, dl, nil); err != nil {
				t.Fatalf("round %d: download of a 25 MiB file: %v", round, err)
			}
		}
	}
}

// Once every blob is on the server, runUpload hands the message to the
// outbox and deletes the staged sealed copies (files.go, after the
// commit). If the send doesn't happen before the server collects
// unattached blobs (24 h: a laptop that sleeps or goes offline right after
// the upload, or a send stuck behind others), the send fails with
// bad_request "file not uploaded, expired or already attached", for good:
// RetryFailed resends the same blob ids and the staged copies that could
// have re-uploaded them are gone. The file never arrives (PROTOCOL §16.1
// promises a resumed upload that finds its blob collected "starts again").
func TestFilesSendAfterBlobCollected_BreakFiles(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	dir := t.TempDir()
	var skew atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	s, addr := startServerClock(t, dir, "127.0.0.1:0", now)
	alice := login(t, s, addr, "alice", false)
	eventually(t, "online", alice.online)
	gen := alice.channel("general")
	// Fill the send bucket (burst 30, 3/s) so the file's message waits in
	// the outbox for a few seconds after its upload.
	for i := range 45 {
		if _, err := alice.SendLocal(gen, fmt.Sprintf("text %d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	local, err := alice.SendFiles(gen, 0, "the file", writeFile(t, t.TempDir(), "doc.txt", []byte("important")))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "uploaded, message waiting in the outbox", func() bool {
		for _, m := range alice.Messages(gen, 100) {
			if m.Local > 0 && m.Pending && len(m.Files) == 1 {
				return true
			}
		}
		return false
	})
	// Offline for a day.
	s.stop()
	eventually(t, "offline", func() bool { return !alice.online() })
	skew.Store(int64(25 * time.Hour))
	startServerClock(t, dir, addr, now)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := alice.WaitUpload(ctx, local); err != nil {
		t.Fatalf("file message after a day offline: %v", err)
	}
}
