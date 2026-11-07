package server

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// blob.put writes the blobs table outside Server.write (store.go: "All
// writes happen inside Server.write, which serializes them"). A send
// transaction (deferred BEGIN, reads first, then writes) that started its
// read before a blob.put committed can't upgrade to a writer: SQLite says
// SQLITE_BUSY_SNAPSHOT, which busy_timeout never retries. So while one user
// uploads, other users' plain text sends fail with `internal`.
func TestBlobPutConcurrentSend_BreakFiles(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	snap, _ := alice.syncFresh()
	gen := generalID(t, snap)

	const senders, perSender = 6, 25 // inside the send bucket's burst of 30
	var users []*tclient
	for i := 0; i < senders; i++ {
		u := ts.register(t, fmt.Sprintf("u%d", i), false)
		u.syncFresh()
		users = append(users, u)
	}

	// One big blob, uploaded in tiny chunks: every put is an UPDATE.
	size := uint64(e2ecrypto.FileSealedSize(1 << 20))
	id := blobID()
	alice.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: gen, Size: size}, nil)

	var mu sync.Mutex
	var internal []string
	note := func(who string, r proto.Res) {
		if !r.OK && r.Err.Code == proto.ErrInternal {
			mu.Lock()
			internal = append(internal, who)
			mu.Unlock()
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		have := uint64(0)
		chunk := make([]byte, 64)
		for i := 0; i < 380; i++ {
			r := alice.req(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: gen, Size: size, Off: have, Data: chunk})
			note("blob.put", r)
			if r.OK {
				var pr proto.BlobPutRes
				proto.Unmarshal(r.Data, &pr)
				have = pr.Have
			}
		}
	}()
	for _, u := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perSender; i++ {
				nonce := make([]byte, 16)
				rand.Read(nonce)
				note("send", u.req(proto.OpSend, proto.SendArgs{Chan: gen, Body: "hi", Nonce: nonce}))
			}
		}()
	}
	wg.Wait()
	if len(internal) > 0 {
		t.Fatalf("%d ops failed with internal while one user uploaded and %d sent text (first: %s)",
			len(internal), senders, internal[0])
	}
}

// writeBlobAt only fsyncs the last chunk, and blobs.have is committed
// after an unsynced write, so after a power loss the row can say more than
// the file holds. The next put then writes at have: Truncate(have) extends
// the short file with zeros, the upload "completes", the sender's message
// goes out, and every receiver gets a blob that fails its integrity check
// (shown as tampering). The server should resume from what the file
// really holds (min(have, file size)), or sync each write before
// recording it.
func TestBlobResumeAfterLostWrite_BreakFiles(t *testing.T) {
	ts := startServer(t, "tcp")
	alice, bob, sec := blobSetup(t, ts)
	id := blobID()
	data := make([]byte, e2ecrypto.FileSealedSize(3*proto.MaxBlobChunk))
	rand.Read(data)
	size := uint64(len(data))
	var r proto.BlobPutRes
	alice.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: sec, Size: size, Data: data[:proto.MaxBlobChunk]}, &r)
	alice.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: sec, Size: size, Off: r.Have, Data: data[r.Have : r.Have+proto.MaxBlobChunk]}, &r)
	// The crash: the second chunk's row survived, its bytes didn't.
	if err := os.Truncate(ts.blobs.path(id), proto.MaxBlobChunk); err != nil {
		t.Fatal(err)
	}
	// The client reconnects and asks where to go on.
	alice.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: sec, Size: size}, &r)
	for r.Have < size {
		end := min(r.Have+proto.MaxBlobChunk, size)
		alice.ok(proto.OpBlobPut, proto.BlobPutArgs{Blob: id, Chan: sec, Size: size, Off: r.Have, Data: data[r.Have:end]}, &r)
	}
	if res := alice.sendFiles(sec, "f", plainFile(id, 3*proto.MaxBlobChunk)); !res.OK {
		t.Fatal(res.Err)
	}
	got, perr := bob.download(id)
	if perr != nil {
		t.Fatal(perr)
	}
	if !bytes.Equal(got, data) {
		i := 0
		for i < len(got) && got[i] == data[i] {
			i++
		}
		t.Fatalf("resumed blob differs from what was uploaded from byte %d (zeros where the lost write was)", i)
	}
}
