package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
)

// Files and images (PROTOCOL.md §16) between real clients over the real server.

func writeFile(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func noise(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(uint64(n), 7))
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	return b
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	copy(img.Pix, noise(len(img.Pix)))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fileMsg is u's cached message in ch carrying a file, if any.
func (u *user) fileMsg(ch uint64) (client.Message, bool) {
	for _, m := range u.Messages(ch, 200) {
		if m.ID != 0 && len(m.Files) > 0 {
			return m, true
		}
	}
	return client.Message{}, false
}

// serverDB opens the server's database next to it (WAL: safe alongside).
func serverDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "tc.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func blobPath(dir string, blob []byte) string {
	h := hex.EncodeToString(blob)
	return filepath.Join(dir, "blobs", h[:2], h)
}

type fileSetup struct {
	s          *srv
	dir        string
	alice, bob *user
	ch         uint64
	ctx        context.Context
}

// setupFiles: alice and bob share ch, an encrypted DM (e2e) or a private
// plaintext channel.
func setupFiles(t *testing.T, e2e bool) fileSetup {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	t.Setenv("TC_DOWNLOADS", t.TempDir())
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	var ch uint64
	if e2e {
		sid := alice.sid(t)
		a, b := alice.Me().ID, bob.Me().ID
		eventually(t, "devices verified both ways", func() bool {
			return alice.verifiedDevices(sid, b) == 1 && bob.verifiedDevices(sid, a) == 1
		})
		dm, err := alice.OpenDM(ctx, []uint64{b})
		if err != nil {
			t.Fatal(err)
		}
		ch = dm.ID
	} else {
		c, err := alice.CreateChannel(ctx, "files", true, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := alice.AddMember(ctx, c.ID, bob.Me().ID); err != nil {
			t.Fatal(err)
		}
		ch = c.ID
	}
	eventually(t, "bob has the channel", func() bool { _, ok := bob.Channel(ch); return ok })
	return fileSetup{s: s, dir: dir, alice: alice, bob: bob, ch: ch, ctx: ctx}
}

func TestFilesRoundTrip(t *testing.T) {
	for _, e2e := range []bool{false, true} {
		name := "plain"
		if e2e {
			name = "e2e"
		}
		t.Run(name, func(t *testing.T) {
			f := setupFiles(t, e2e)
			src := t.TempDir()
			img := pngBytes(t, 640, 480)
			doc := noise(3*proto.MaxBlobChunk + 99)
			local, err := f.alice.SendFiles(f.ch, 0, "", writeFile(t, src, "cat.png", img), writeFile(t, src, "../report.bin", doc))
			if err != nil {
				t.Fatal(err)
			}
			if local >= 0 {
				t.Fatalf("upload id %d, want negative", local)
			}
			if err := f.alice.WaitUpload(f.ctx, local); err != nil {
				t.Fatal(err)
			}
			var m client.Message
			eventually(t, "bob gets the files", func() bool { var ok bool; m, ok = f.bob.fileMsg(f.ch); return ok })
			if len(m.Files) != 2 || m.Files[0].Name != "cat.png" || m.Files[1].Name != "report.bin" {
				t.Fatalf("files %+v", m.Files)
			}
			if m.Body != client.FallbackBody(m.Files) {
				t.Fatalf("body %q", m.Body)
			}
			if e2e != (m.Sec == client.SecE2E) {
				t.Fatalf("sec %s", m.Sec)
			}
			if m.Files[0].Mime != "image/png" || len(m.Files[0].Thumb) == 0 || len(m.Files[0].Thumb) > proto.MaxThumb ||
				m.Files[0].W != 640 || m.Files[0].H != 480 {
				t.Fatalf("image meta: mime %q thumb %d %dx%d", m.Files[0].Mime, len(m.Files[0].Thumb), m.Files[0].W, m.Files[0].H)
			}
			dl := client.DownloadsDir()
			for i, want := range [][]byte{img, doc} {
				p, err := f.bob.Download(f.ctx, m.ID, i, dl, nil)
				if err != nil {
					t.Fatalf("download %d: %v", i, err)
				}
				got, _ := os.ReadFile(p)
				if !bytes.Equal(got, want) {
					t.Fatalf("file %d differs", i)
				}
				if st, _ := os.Stat(p); st.Mode().Perm()&0o111 != 0 {
					t.Fatalf("saved executable: %v", st.Mode())
				}
			}
			// Never over an existing file.
			p2, err := f.bob.Download(f.ctx, m.ID, 0, dl, nil)
			if err != nil || filepath.Base(p2) != "cat (1).png" {
				t.Fatalf("second download: %q %v", p2, err)
			}
			// Nothing else left in the downloads dir (no temporary files).
			ents, _ := os.ReadDir(dl)
			if len(ents) != 3 {
				t.Fatalf("downloads dir holds %d entries", len(ents))
			}
			// What the server stored: in an encrypted channel only blob ids
			// and sealed bytes; never the plaintext.
			db := serverDB(t, f.dir)
			var raw []byte
			if err := db.QueryRow(`SELECT files FROM messages WHERE id = ?`, m.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var stored []proto.File
			proto.Unmarshal(raw, &stored)
			if len(stored) != 2 || e2e && (stored[0].Key != nil || stored[0].Name != "" || stored[0].Thumb != nil) {
				t.Fatalf("server stored %+v", stored)
			}
			blob, _ := os.ReadFile(blobPath(f.dir, m.Files[1].Blob))
			if len(blob) == 0 || bytes.Contains(blob, doc[:64]) {
				t.Fatal("blob missing or holds plaintext")
			}
			// Staged copies are gone once sent.
			if ents, _ := os.ReadDir(filepath.Join(f.alice.home, "uploads")); len(ents) != 0 {
				t.Fatalf("%d staged files left", len(ents))
			}
		})
	}
}

// TestFilesTamper: a server that swaps, flips or truncates a blob is caught
// and nothing is saved.
func TestFilesTamper(t *testing.T) {
	for _, e2e := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "e2e"}[e2e], func(t *testing.T) {
			f := setupFiles(t, e2e)
			src := t.TempDir()
			a := noise(2*proto.MaxBlobChunk + 5)
			b := noise(2*proto.MaxBlobChunk + 5) // same size: a swap passes the size check
			for _, p := range []string{writeFile(t, src, "a.bin", a), writeFile(t, src, "b.bin", b)} {
				local, err := f.alice.SendFiles(f.ch, 0, "", p)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.alice.WaitUpload(f.ctx, local); err != nil {
					t.Fatal(err)
				}
			}
			var ms []client.Message
			eventually(t, "bob has both", func() bool {
				ms = ms[:0]
				for _, m := range f.bob.Messages(f.ch, 50) {
					if m.ID != 0 && len(m.Files) == 1 {
						ms = append(ms, m)
					}
				}
				return len(ms) == 2
			})
			pa, pb := blobPath(f.dir, ms[0].Files[0].Blob), blobPath(f.dir, ms[1].Files[0].Blob)
			orig, _ := os.ReadFile(pa)
			other, _ := os.ReadFile(pb)
			db := serverDB(t, f.dir)
			dl := t.TempDir()
			check := func(what string) {
				t.Helper()
				if _, err := f.bob.Download(f.ctx, ms[0].ID, 0, dl, nil); !errors.Is(err, client.ErrFileCheck) {
					t.Fatalf("%s: download err %v, want ErrFileCheck", what, err)
				}
				if ents, _ := os.ReadDir(dl); len(ents) != 0 {
					t.Fatalf("%s: something was saved", what)
				}
			}
			// Swap in another file's blob.
			os.WriteFile(pa, other, 0o600)
			check("swapped")
			// Flip one bit.
			flip := bytes.Clone(orig)
			flip[len(flip)/2] ^= 1
			os.WriteFile(pa, flip, 0o600)
			check("flipped")
			// Truncate a whole chunk, and say so in the database.
			cut := orig[:len(orig)-(proto.MaxBlobChunk/4)]
			os.WriteFile(pa, cut, 0o600)
			db.Exec(`UPDATE blobs SET size = ?, have = ? WHERE id = ?`, len(cut), len(cut), ms[0].Files[0].Blob)
			check("truncated")
			// Put back: it opens again.
			os.WriteFile(pa, orig, 0o600)
			db.Exec(`UPDATE blobs SET size = ?, have = ? WHERE id = ?`, len(orig), len(orig), ms[0].Files[0].Blob)
			p, err := f.bob.Download(f.ctx, ms[0].ID, 0, dl, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(p); !bytes.Equal(got, a) {
				t.Fatal("restored blob differs")
			}
		})
	}
}

func TestFilesDeleteAndMembership(t *testing.T) {
	f := setupFiles(t, false)
	src := t.TempDir()
	local, err := f.alice.SendFiles(f.ch, 0, "the plan", writeFile(t, src, "plan.txt", []byte("step 1: files\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.alice.WaitUpload(f.ctx, local); err != nil {
		t.Fatal(err)
	}
	var m client.Message
	eventually(t, "bob gets it", func() bool { var ok bool; m, ok = f.bob.fileMsg(f.ch); return ok })
	if m.Body != "the plan" || m.Files[0].Mime != "text/plain" {
		t.Fatalf("body %q mime %q", m.Body, m.Files[0].Mime)
	}
	path := blobPath(f.dir, m.Files[0].Blob)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	// bob leaves: he can't fetch it any more (the server checks).
	ctx := f.ctx
	if err := f.alice.Delete(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob sees the delete", func() bool { _, ok := f.bob.fileMsg(f.ch); return !ok })
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("blob file still on disk: %v", err)
	}
	if _, err := f.bob.Download(ctx, m.ID, 0, t.TempDir(), nil); err == nil {
		t.Fatal("downloaded a deleted message's file")
	}
}

// TestFilesRemovedMember: a member removed from a private channel can no
// longer fetch its files, even with the message cached.
func TestFilesRemovedMember(t *testing.T) {
	f := setupFiles(t, false)
	src := t.TempDir()
	local, err := f.alice.SendFiles(f.ch, 0, "", writeFile(t, src, "x.bin", noise(1000)))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.alice.WaitUpload(f.ctx, local); err != nil {
		t.Fatal(err)
	}
	var m client.Message
	eventually(t, "bob gets it", func() bool { var ok bool; m, ok = f.bob.fileMsg(f.ch); return ok })
	if err := f.alice.RemoveMember(f.ctx, f.ch, f.bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob removed", func() bool {
		cv, ok := f.bob.Channel(f.ch)
		return !ok || !cv.Member
	})
	_, err = f.bob.Download(f.ctx, m.ID, 0, t.TempDir(), nil)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.ErrNotFound {
		t.Fatalf("removed member download: %v", err)
	}
}

// TestFilesResumeAfterDisconnect: the connection drops mid-upload; the
// client reconnects and finishes from where the server's copy ends.
func TestFilesResumeAfterDisconnect(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	proxy := newChaosProxy(t, addr) // ~7 ms per 16 KiB each way: an upload takes a while
	alice := login(t, s, proxy.addr(), "alice", false)
	eventually(t, "online", alice.online)
	gen := alice.channel("general")
	data := noise(16 * proto.MaxBlobChunk)
	local, err := alice.SendFiles(gen, 0, "", writeFile(t, t.TempDir(), "big.bin", data))
	if err != nil {
		t.Fatal(err)
	}
	db := serverDB(t, dir)
	var have, size int64
	eventually(t, "upload under way", func() bool {
		db.QueryRow(`SELECT have, size FROM blobs`).Scan(&have, &size)
		return have > 0
	})
	if have >= size {
		t.Fatalf("upload finished before the cut (%d/%d)", have, size)
	}
	proxy.minAge = 0
	proxy.killRandom()
	t.Logf("cut at %d of %d bytes", have, size)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := alice.WaitUpload(ctx, local); err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d blobs on the server, want 1 (resumed, not restarted)", n)
	}
	var m client.Message
	eventually(t, "message", func() bool { var ok bool; m, ok = alice.fileMsg(gen); return ok })
	p, err := alice.Download(ctx, m.ID, 0, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, data) {
		t.Fatal("resumed upload differs")
	}
}

// TestFilesOfflineAndLimits: files queue offline; the server's limit is
// enforced on both sides.
func TestFilesOfflineAndLimits(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	dir := t.TempDir()
	s, addr := startServerConfig(t, "127.0.0.1:0", server.Config{DataDir: dir, MaxUpload: 1 << 20})
	alice := login(t, s, addr, "alice", false)
	eventually(t, "online", alice.online)
	if alice.MaxUpload() != 1<<20 {
		t.Fatalf("MaxUpload %d", alice.MaxUpload())
	}
	gen := alice.channel("general")
	src := t.TempDir()
	_, err := alice.SendFiles(gen, 0, "", writeFile(t, src, "big.bin", noise(1<<20+1)))
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.ErrTooLarge || !strings.Contains(err.Error(), "big.bin") {
		t.Fatalf("over the limit: %v", err)
	}
	// Not a regular file.
	if _, err := alice.SendFiles(gen, 0, "", src); err == nil {
		t.Fatal("sent a directory")
	}
	// Offline: queued, shown pending, sent on reconnect.
	s.stop()
	eventually(t, "offline", func() bool { return !alice.online() })
	local, err := alice.SendFiles(gen, 0, "later", writeFile(t, src, "small.txt", []byte("hi")))
	if err != nil {
		t.Fatal(err)
	}
	pending := false
	for _, m := range alice.Messages(gen, 10) {
		pending = pending || m.Local == local && m.Pending && m.UpTotal > 0
	}
	if !pending {
		t.Fatal("queued upload not shown as pending")
	}
	s2, _ := startServerConfig(t, addr, server.Config{DataDir: dir, MaxUpload: 1 << 20})
	_ = s2
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := alice.WaitUpload(ctx, local); err != nil {
		t.Fatal(err)
	}
	eventually(t, "sent", func() bool { m, ok := alice.fileMsg(gen); return ok && m.Body == "later" })
}
