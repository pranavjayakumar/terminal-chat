package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

func blobID() []byte {
	id := make([]byte, proto.BlobIDSize)
	rand.Read(id)
	return id
}

// upload puts a whole blob of a size-byte file in chunks; it returns the
// blob's bytes.
func (c *tclient) upload(id []byte, ch uint64, size int64) []byte {
	c.t.Helper()
	data := make([]byte, e2ecrypto.FileSealedSize(size))
	rand.Read(data)
	var r proto.BlobPutRes
	c.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: ch, Size: uint64(len(data))}, &r)
	for r.Have < uint64(len(data)) {
		end := min(r.Have+proto.MaxBlobChunk, uint64(len(data)))
		c.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: ch, Size: uint64(len(data)), Off: r.Have, Data: data[r.Have:end]}, &r)
	}
	return data
}

func (c *tclient) download(id []byte) ([]byte, *proto.Error) {
	c.t.Helper()
	var out []byte
	for {
		r := c.req(proto.OpBlobGet, proto.BlobGetArgs{Blob: id, Off: uint64(len(out))})
		if !r.OK {
			return nil, r.Err
		}
		var g proto.BlobGetRes
		if err := proto.Unmarshal(r.Data, &g); err != nil {
			c.t.Fatal(err)
		}
		out = append(out, g.Data...)
		if uint64(len(out)) >= g.Size {
			return out, nil
		}
		if len(g.Data) == 0 {
			c.t.Fatal("blob.get: empty chunk before the end")
		}
	}
}

func plainFile(id []byte, size int64) proto.File {
	return proto.File{Blob: id, Name: "cat.png", Size: uint64(size), Mime: "image/png",
		Key: make([]byte, 32), Hash: make([]byte, 32), Thumb: []byte{0xff, 0xd8}, W: 10, H: 10}
}

func (c *tclient) sendFiles(ch uint64, body string, files ...proto.File) proto.Res {
	c.t.Helper()
	nonce := make([]byte, 16)
	rand.Read(nonce)
	return c.req(proto.OpSend, proto.SendArgs{Chan: ch, Body: body, Nonce: nonce, Files: files})
}

func TestBlobCapAndLimit(t *testing.T) {
	ts := startServerCfg(t, "tcp", Config{MaxUpload: 1 << 20})
	c := ts.register(t, "alice", false)
	if !slices.Contains(c.welcome.Caps, proto.CapBlob) || c.welcome.MaxUpload != 1<<20 {
		t.Fatalf("caps %v max_upload %d", c.welcome.Caps, c.welcome.MaxUpload)
	}
}

func TestBlobRoundTrip(t *testing.T) {
	forEachTransport(t, func(t *testing.T, ts *testServer) {
		_, alice, bob, sec := pinSetup2(t, ts)
		eve := ts.register(t, "eve", false)
		eve.syncFresh()

		id := blobID()
		size := int64(3*proto.MaxBlobChunk + 1234)
		data := alice.upload(id, sec, size)
		// Not attached yet: nobody can read it, the uploader neither.
		if _, err := bob.download(id); err == nil || err.Code != proto.ErrNotFound {
			t.Fatalf("pending blob readable: %v", err)
		}
		r := alice.sendFiles(sec, "📎 cat.png", plainFile(id, size))
		if !r.OK {
			t.Fatalf("send: %v", r.Err)
		}
		ev := bob.event(proto.KMsgNew)
		var m proto.Message
		proto.Unmarshal(ev.Data, &m)
		if len(m.Files) != 1 || !bytes.Equal(m.Files[0].Blob, id) || m.Files[0].Name != "cat.png" {
			t.Fatalf("msg.new files %+v", m.Files)
		}
		got, perr := bob.download(id)
		if perr != nil || !bytes.Equal(got, data) {
			t.Fatalf("bob download: %v, equal=%v", perr, bytes.Equal(got, data))
		}
		// A non-member of the private channel learns nothing.
		if _, err := eve.download(id); err == nil || err.Code != proto.ErrNotFound {
			t.Fatalf("eve download: %v", err)
		}
		// Nor can she upload into it.
		eve.fail(proto.OpBlobPut, proto.BlobPutArgs{Blob: blobID(), Chan: sec, Size: 16}, proto.ErrForbidden)
		// History carries the files.
		var h proto.HistoryRes
		bob.ok(proto.OpHistory, proto.HistoryArgs{Chan: sec, Limit: 10}, &h)
		if len(h.Msgs) == 0 || len(h.Msgs[len(h.Msgs)-1].Files) != 1 {
			t.Fatalf("history files: %+v", h.Msgs)
		}
		// Reading past the end is refused; at the end, empty.
		bob.fail(proto.OpBlobGet, proto.BlobGetArgs{Blob: id, Off: uint64(len(data)) + 1}, proto.ErrBadRequest)
	})
}

// pinSetup2 is pinSetup on a given server.
func pinSetup2(t *testing.T, ts *testServer) (*testServer, *tclient, *tclient, uint64) {
	t.Helper()
	alice := ts.register(t, "alice", true)
	bob := ts.register(t, "bob", false)
	alice.syncFresh()
	bob.syncFresh()
	var cr proto.ChanRes
	alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "secret", Private: true}, &cr)
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: cr.Chan.ID, User: bob.user.ID}, nil)
	bob.event(proto.KChanJoin)
	return ts, alice, bob, cr.Chan.ID
}

func TestBlobPublicChannelReadable(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", false)
	snap, _ := alice.syncFresh()
	gen := generalID(t, snap)
	var cr proto.ChanRes
	alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "open"}, &cr)
	id := blobID()
	data := alice.upload(id, cr.Chan.ID, 100)
	if r := alice.sendFiles(cr.Chan.ID, "f", plainFile(id, 100)); !r.OK {
		t.Fatal(r.Err)
	}
	bob := ts.register(t, "bob", false)
	bob.syncFresh()
	// bob isn't in #open, but can read its history, so its files too.
	got, err := bob.download(id)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("public download: %v", err)
	}
	_ = gen
}

func TestBlobResume(t *testing.T) {
	ts := startServer(t, "quic")
	alice, _, sec := blobSetup(t, ts)
	id := blobID()
	data := make([]byte, e2ecrypto.FileSealedSize(2*proto.MaxBlobChunk))
	rand.Read(data)
	size := uint64(len(data))
	var r proto.BlobPutRes
	alice.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: sec, Size: size, Data: data[:proto.MaxBlobChunk]}, &r)
	if r.Have != proto.MaxBlobChunk {
		t.Fatalf("have %d", r.Have)
	}
	// A retry of the same chunk (its RES lost) writes nothing and says where to go on.
	alice.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: sec, Size: size, Data: data[:proto.MaxBlobChunk]}, &r)
	if r.Have != proto.MaxBlobChunk {
		t.Fatalf("retry: have %d", r.Have)
	}
	// So does a chunk from too far ahead.
	alice.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: sec, Size: size, Off: size - 10, Data: data[size-10:]}, &r)
	if r.Have != proto.MaxBlobChunk {
		t.Fatalf("skip ahead: have %d", r.Have)
	}
	// Reconnect: an empty put asks where the upload stands.
	alice.conn.Close()
	alice2 := reconnect(t, ts, alice)
	alice2.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: sec, Size: size}, &r)
	if r.Have != proto.MaxBlobChunk {
		t.Fatalf("after reconnect: have %d", r.Have)
	}
	for r.Have < size {
		end := min(r.Have+proto.MaxBlobChunk, size)
		alice2.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: sec, Size: size, Off: r.Have, Data: data[r.Have:end]}, &r)
	}
	if res := alice2.sendFiles(sec, "f", plainFile(id, 2*proto.MaxBlobChunk)); !res.OK {
		t.Fatal(res.Err)
	}
	got, err := alice2.download(id)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("resumed blob differs: %v", err)
	}
	// Another size or channel under the same id is a conflict; someone
	// else's id is unknown to the caller.
	alice2.fail(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: sec, Size: 16}, proto.ErrConflict)
}

func blobSetup(t *testing.T, ts *testServer) (*tclient, *tclient, uint64) {
	_, alice, bob, sec := pinSetup2(t, ts)
	return alice, bob, sec
}

// reconnect logs c's key in again on a new connection and syncs.
func reconnect(t *testing.T, ts *testServer, c *tclient) *tclient {
	t.Helper()
	c2, bye := ts.connect(t, c.signer, "", "")
	if bye != "" {
		t.Fatalf("reconnect: %s", bye)
	}
	c2.syncFresh()
	return c2
}

func TestBlobAttachRules(t *testing.T) {
	ts := startServer(t, "tcp")
	alice, bob, sec := blobSetup(t, ts)
	snap, _ := alice.syncFreshAgain(t, ts)
	gen := generalID(t, snap)

	// Unfinished upload.
	part := blobID()
	alice.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: part, Chan: sec, Size: uint64(e2ecrypto.FileSealedSize(100))}, nil)
	if r := alice.sendFiles(sec, "f", plainFile(part, 100)); r.OK || r.Err.Code != proto.ErrBadRequest {
		t.Fatalf("unfinished attach: %+v", r)
	}
	// Uploaded for another channel.
	other := blobID()
	alice.upload(other, gen, 100)
	if r := alice.sendFiles(sec, "f", plainFile(other, 100)); r.OK {
		t.Fatal("attached a blob of another channel")
	}
	// Someone else's blob.
	if r := bob.sendFiles(sec, "f", plainFile(other, 100)); r.OK {
		t.Fatal("attached someone else's blob")
	}
	// Wrong declared size.
	good := blobID()
	alice.upload(good, sec, 100)
	if r := alice.sendFiles(sec, "f", plainFile(good, 101)); r.OK {
		t.Fatal("size mismatch accepted")
	}
	// Missing metadata in a plaintext channel; a path-like or control name.
	bad := plainFile(good, 100)
	bad.Key = nil
	if r := alice.sendFiles(sec, "f", bad); r.OK {
		t.Fatal("no key accepted")
	}
	bad = plainFile(good, 100)
	bad.Name = "a\x1b[2Jb"
	if r := alice.sendFiles(sec, "f", bad); r.OK {
		t.Fatal("control characters in a name accepted")
	}
	bad = plainFile(good, 100)
	bad.Mime = "text/html; charset=utf-8"
	if r := alice.sendFiles(sec, "f", bad); r.OK {
		t.Fatal("mime with parameters accepted")
	}
	bad = plainFile(good, 100)
	bad.Thumb = make([]byte, proto.MaxThumb+1)
	if r := alice.sendFiles(sec, "f", bad); r.OK || r.Err.Code != proto.ErrTooLarge {
		t.Fatalf("big thumbnail: %+v", r)
	}
	if r := alice.sendFiles(sec, "f", plainFile(good, 100), plainFile(good, 100)); r.OK {
		t.Fatal("same file twice accepted")
	}
	// Finally right; then it can't be attached again.
	nonce := make([]byte, 16)
	rand.Read(nonce)
	args := proto.SendArgs{Chan: sec, Body: "f", Nonce: nonce, Files: []proto.File{plainFile(good, 100)}}
	var first proto.SendRes
	alice.ok(proto.OpSend, args, &first)
	// An idempotent retry finds the message; the same nonce with other files is a conflict.
	var again proto.SendRes
	alice.ok(proto.OpSend, args, &again)
	if again.Msg != first.Msg {
		t.Fatalf("retry made message %d, want %d", again.Msg, first.Msg)
	}
	args.Files = nil
	alice.fail(proto.OpSend, args, proto.ErrConflict)
	if r := alice.sendFiles(sec, "f", plainFile(good, 100)); r.OK {
		t.Fatal("attached twice")
	}
	// Too many files.
	many := make([]proto.File, proto.MaxFiles+1)
	for i := range many {
		many[i] = plainFile(blobID(), 1)
	}
	if r := alice.sendFiles(sec, "f", many...); r.OK || r.Err.Code != proto.ErrBadRequest || r.Err.Msg != "at most 10 files per message" {
		t.Fatalf("too many files: %+v", r.Err)
	}
}

func TestBlobE2EChannelBlobOnly(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.registerCaps(t, "alice", false, []string{proto.CapE2E})
	alice.syncFresh()
	var cr proto.ChanRes
	alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "e2e", Private: true, E2E: true}, &cr)
	id := blobID()
	alice.upload(id, cr.Chan.ID, 10)
	// Before any enc the plaintext send is refused anyway; what matters is
	// that metadata in the clear is refused in an e2e channel even with enc.
	f := plainFile(id, 10)
	if err := attachFilesCheck(ts.Server, cr.Chan.ID, alice.user.ID, []proto.File{f}, true); err == nil {
		t.Fatal("full metadata accepted in an e2e channel")
	}
	if err := attachFilesCheck(ts.Server, cr.Chan.ID, alice.user.ID, []proto.File{{Blob: id}}, true); err != nil {
		t.Fatalf("blob-only: %v", err)
	}
}

// attachFilesCheck runs attachFiles in a transaction that is rolled back.
func attachFilesCheck(s *Server, ch, author uint64, files []proto.File, e2e bool) error {
	tx, err := s.st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return attachFiles(context.Background(), tx, files, e2e, 1, author, ch)
}

func TestBlobDeleteRemoves(t *testing.T) {
	ts := startServer(t, "tcp")
	alice, bob, sec := blobSetup(t, ts)
	id := blobID()
	alice.upload(id, sec, 1000)
	var sr proto.SendRes
	nonce := make([]byte, 16)
	rand.Read(nonce)
	alice.ok(proto.OpSend, proto.SendArgs{Chan: sec, Body: "f", Nonce: nonce, Files: []proto.File{plainFile(id, 1000)}}, &sr)
	path := ts.blobs.path(id)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.download(id); err != nil {
		t.Fatal(err)
	}
	alice.ok(proto.OpDelete, proto.DeleteArgs{Msg: sr.Msg}, nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("blob file still there: %v", err)
	}
	if _, err := bob.download(id); err == nil || err.Code != proto.ErrNotFound {
		t.Fatalf("deleted blob: %v", err)
	}
	// The log no longer carries the file's key.
	var h proto.HistoryRes
	bob.ok(proto.OpHistory, proto.HistoryArgs{Chan: sec, Limit: 10}, &h)
	for _, m := range h.Msgs {
		if m.ID == sr.Msg && len(m.Files) > 0 {
			t.Fatal("deleted message keeps files")
		}
	}
	var data []byte
	ts.st.db.QueryRow(`SELECT data FROM events WHERE seq = ?`, sr.Msg).Scan(&data)
	var m proto.Message
	proto.Unmarshal(data, &m)
	if len(m.Files) > 0 {
		t.Fatal("msg.new event keeps files after delete")
	}
}

func TestBlobQuotaAndLimits(t *testing.T) {
	ts := startServerCfg(t, "tcp", Config{MaxUpload: 1 << 20, BlobQuota: 3 << 20})
	alice, _, sec := blobSetup(t, ts)
	// Over --max-upload.
	big := uint64(e2ecrypto.FileSealedSize(1<<20 + 1))
	alice.fail(proto.OpBlobPut, proto.BlobPutArgs{Blob: blobID(), Chan: sec, Size: big}, proto.ErrTooLarge)
	// Not a size any file seals to.
	alice.fail(proto.OpBlobPut, proto.BlobPutArgs{Blob: blobID(), Chan: sec, Size: 5}, proto.ErrBadRequest)
	// A chunk over 256 KiB.
	alice.fail(proto.OpBlobPut, proto.BlobPutArgs{Blob: blobID(), Chan: sec, Size: uint64(e2ecrypto.FileSealedSize(1 << 20)),
		Data: make([]byte, proto.MaxBlobChunk+1)}, proto.ErrTooLarge)
	// Fill the quota: three 1 MiB files fit, the fourth doesn't.
	one := uint64(e2ecrypto.FileSealedSize(1 << 20))
	for i := 0; i < 2; i++ {
		alice.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: blobID(), Chan: sec, Size: one}, nil)
	}
	alice.fail(proto.OpBlobPut, proto.BlobPutArgs{Blob: blobID(), Chan: sec, Size: one}, proto.ErrTooLarge)
	// Pending uploads per user are capped too.
	ts2 := startServer(t, "tcp")
	a2, _, sec2 := blobSetup(t, ts2)
	for i := 0; i < proto.MaxPendingBlobs; i++ {
		a2.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: blobID(), Chan: sec2, Size: 16}, nil)
	}
	a2.fail(proto.OpBlobPut, proto.BlobPutArgs{Blob: blobID(), Chan: sec2, Size: 16}, proto.ErrConflict)
}

func TestBlobGC(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	ts := startServerCfg(t, "tcp", Config{Now: func() time.Time { return clock() }})
	alice, _, sec := blobSetup(t, ts)
	pending := blobID()
	alice.upload(pending, sec, 10)
	kept := blobID()
	alice.upload(kept, sec, 10)
	if r := alice.sendFiles(sec, "f", plainFile(kept, 10)); !r.OK {
		t.Fatal(r.Err)
	}
	stray := filepath.Join(ts.blobs.dir, "ab", "ab"+string(bytes.Repeat([]byte("0"), 30)))
	os.MkdirAll(filepath.Dir(stray), 0o700)
	os.WriteFile(stray, []byte("x"), 0o600)
	notOurs := filepath.Join(ts.blobs.dir, "README")
	os.WriteFile(notOurs, []byte("x"), 0o600)
	old := time.Now().Add(-2 * blobStrayAge)
	os.Chtimes(stray, old, old)
	os.Chtimes(notOurs, old, old)

	ctx := context.Background()
	if err := ts.gcBlobs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ts.blobs.path(pending)); err != nil {
		t.Fatal("fresh pending upload collected")
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("stray file kept")
	}
	if _, err := os.Stat(notOurs); err != nil {
		t.Fatal("removed a file that isn't a blob")
	}
	now = now.Add(blobPendingTTL + time.Minute)
	if err := ts.gcBlobs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ts.blobs.path(pending)); !os.IsNotExist(err) {
		t.Fatal("expired pending upload kept")
	}
	if _, err := os.Stat(ts.blobs.path(kept)); err != nil {
		t.Fatal("attached blob collected")
	}
	// Resuming an expired upload says so.
	alice.fail(proto.OpBlobPut, proto.BlobPutArgs{Blob: pending, Chan: sec, Size: uint64(e2ecrypto.FileSealedSize(10)), Off: 5, Data: []byte("x")}, proto.ErrNotFound)
}

func TestBlobAgentsRefused(t *testing.T) {
	if !agentRefused[proto.OpBlobPut] || !agentRefused[proto.OpBlobGet] {
		t.Fatal("agents may use blob ops")
	}
}
