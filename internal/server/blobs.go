package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// Files (PROTOCOL.md §16, cap blob1). A blob is an opaque, client-sealed
// file tied to one channel. Its bytes live on disk under DataDir/blobs (not
// in SQLite); the blobs table records who uploads it, how far, and which
// message attached it. Deleting that message deletes the blob; blobs never
// attached are collected after blobPendingTTL.

const blobsSchema = `
CREATE TABLE IF NOT EXISTS blobs (
	id BLOB PRIMARY KEY,
	chan INTEGER NOT NULL,
	owner INTEGER NOT NULL,
	size INTEGER NOT NULL,
	have INTEGER NOT NULL DEFAULT 0,
	msg INTEGER,
	created INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS blobs_msg ON blobs(msg);
CREATE INDEX IF NOT EXISTS blobs_pending ON blobs(owner) WHERE msg IS NULL;
`

// DefaultBlobQuota is the default room for all blobs together (--blob-quota).
const DefaultBlobQuota = 10 << 30

var (
	// blobPendingTTL: an upload not attached to a message by then is dropped.
	blobPendingTTL = 24 * time.Hour
	// blobGCEvery is how often unattached uploads and stray files are collected.
	blobGCEvery = time.Hour
	// blobStrayAge: a file with no row is removed once this old (a crash
	// between removing a row and its file, or a restore from backup).
	blobStrayAge = time.Hour
)

// blobStore is the on-disk side of blobs.
type blobStore struct {
	dir string
	// mu serializes blob rows and files: creates (the quota check), writes
	// and collection. Lock order: Server.mu before mu.
	mu sync.Mutex
}

func migrateBlobs(db *sql.DB) error {
	if _, err := db.Exec(blobsSchema); err != nil {
		return fmt.Errorf("creating blobs: %w", err)
	}
	// messages.files: msgpack []proto.File as attached (§16.2).
	return addColumn(db, "messages", "files", "BLOB")
}

func (b *blobStore) path(id []byte) string {
	h := hex.EncodeToString(id)
	return filepath.Join(b.dir, h[:2], h)
}

// maxUpload is the largest plaintext file; maxBlob the largest blob.
func (s *Server) maxUpload() int64 {
	if s.cfg.MaxUpload > 0 {
		return s.cfg.MaxUpload
	}
	return proto.DefaultMaxUpload
}

func (s *Server) maxBlob() int64 { return e2ecrypto.FileSealedSize(s.maxUpload()) }

func (s *Server) blobQuota() int64 {
	if s.cfg.BlobQuota > 0 {
		return s.cfg.BlobQuota
	}
	return DefaultBlobQuota
}

// opBlobPut creates or appends to one of the caller's blobs.
func (s *Server) opBlobPut(ctx context.Context, sess *session, a proto.BlobPutArgs) (any, error) {
	u := sess.user.ID
	switch {
	case len(a.Blob) != proto.BlobIDSize:
		return nil, badRequest("blob id must be 16 bytes")
	case len(a.Data) > proto.MaxBlobChunk:
		return nil, proto.Errf(proto.ErrTooLarge, "chunk over 256 KiB")
	case a.Size > uint64(s.maxBlob()):
		return nil, proto.Errf(proto.ErrTooLarge, fmt.Sprintf("file over the server's limit (%d MiB)", s.maxUpload()>>20))
	case a.Off > a.Size || uint64(len(a.Data)) > a.Size-a.Off:
		return nil, badRequest("chunk past the end of the blob")
	}
	if _, ok := e2ecrypto.FilePlainSize(int64(a.Size)); !ok {
		return nil, badRequest("not the size of a sealed file")
	}
	s.mu.Lock()
	member := s.members[a.Chan][u]
	limited := !s.take(s.blimits, u, blobBurst, blobRefill)
	s.mu.Unlock()
	if !member {
		return nil, errForbidden
	}
	if limited {
		return nil, proto.Errf(proto.ErrRateLimited, "slow down")
	}
	bs := s.blobs
	bs.mu.Lock()
	defer bs.mu.Unlock()
	db := s.st.db
	var ch, owner, size, have uint64
	var msg sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT chan, owner, size, have, msg FROM blobs WHERE id = ?`, a.Blob).
		Scan(&ch, &owner, &size, &have, &msg)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if a.Off != 0 {
			// Collected meanwhile (or never made): start again.
			return nil, proto.Errf(proto.ErrNotFound, "upload expired")
		}
		if err := s.blobRoom(ctx, u, a.Size); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(bs.path(a.Blob)), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(bs.path(a.Blob), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		f.Close()
		if _, err := db.ExecContext(ctx, `INSERT INTO blobs (id, chan, owner, size, have, created) VALUES (?, ?, ?, ?, 0, ?)`,
			a.Blob, a.Chan, u, a.Size, s.now()); err != nil {
			os.Remove(bs.path(a.Blob))
			return nil, err
		}
		ch, owner, size = a.Chan, u, a.Size
	case err != nil:
		return nil, err
	}
	switch {
	case owner != u:
		return nil, errNotFound
	case ch != a.Chan || size != a.Size:
		return nil, proto.Errf(proto.ErrConflict, "blob id in use for another upload")
	case msg.Valid || a.Off != have || len(a.Data) == 0:
		return proto.BlobPutRes{Have: have}, nil
	}
	if err := writeBlobAt(bs.path(a.Blob), a.Data, int64(have), have+uint64(len(a.Data)) == size); err != nil {
		return nil, err
	}
	have += uint64(len(a.Data))
	if _, err := db.ExecContext(ctx, `UPDATE blobs SET have = ? WHERE id = ?`, have, a.Blob); err != nil {
		return nil, err
	}
	return proto.BlobPutRes{Have: have}, nil
}

// blobRoom checks a new blob of size against the caller's unfinished
// uploads and the server's quota. Caller holds blobs.mu.
func (s *Server) blobRoom(ctx context.Context, u, size uint64) error {
	var n int
	var pending, total int64
	err := s.st.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size), 0) FROM blobs WHERE owner = ? AND msg IS NULL`, u).
		Scan(&n, &pending)
	if err != nil {
		return err
	}
	if n >= proto.MaxPendingBlobs || pending+int64(size) > int64(proto.MaxFiles)*s.maxBlob() {
		return proto.Errf(proto.ErrConflict, "too many unfinished uploads; send or wait for them")
	}
	if err := s.st.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM blobs`).Scan(&total); err != nil {
		return err
	}
	if total+int64(size) > s.blobQuota() {
		return proto.Errf(proto.ErrTooLarge, "the server's file storage is full")
	}
	return nil
}

// writeBlobAt writes data at off, dropping whatever a crashed write left
// past off first; last syncs the finished file.
func writeBlobAt(path string, data []byte, off int64, last bool) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(off); err != nil {
		return err
	}
	if _, err := f.WriteAt(data, off); err != nil {
		return err
	}
	if last {
		return f.Sync()
	}
	return nil
}

// opBlobGet reads part of an attached blob the caller may see: its message
// isn't deleted and its channel is public or the caller's.
func (s *Server) opBlobGet(ctx context.Context, sess *session, a proto.BlobGetArgs) (any, error) {
	u := sess.user.ID
	if len(a.Blob) != proto.BlobIDSize {
		return nil, badRequest("blob id must be 16 bytes")
	}
	n := int64(a.Len)
	if n == 0 || n > proto.MaxBlobChunk {
		n = proto.MaxBlobChunk
	}
	var ch, size uint64
	var deleted bool
	err := s.st.db.QueryRowContext(ctx, `SELECT b.chan, b.size, m.deleted FROM blobs b JOIN messages m ON m.id = b.msg
		WHERE b.id = ? AND b.have = b.size`, a.Blob).Scan(&ch, &size, &deleted)
	if errors.Is(err, sql.ErrNoRows) || err == nil && deleted {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	visible := s.members[ch][u] || s.kinds[ch] == proto.ChanPublic
	limited := !s.take(s.glimits, u, blobBurst, 2*blobRefill)
	s.mu.Unlock()
	if !visible {
		return nil, errNotFound
	}
	if limited {
		return nil, proto.Errf(proto.ErrRateLimited, "slow down")
	}
	if a.Off > size {
		return nil, badRequest("offset past the end of the blob")
	}
	n = min(n, int64(size-a.Off))
	f, err := os.Open(s.blobs.path(a.Blob))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errNotFound // deleted just now
		}
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, int64(a.Off)); err != nil && !(errors.Is(err, io.EOF) && n == 0) {
		return nil, fmt.Errorf("reading blob: %w", err)
	}
	return proto.BlobGetRes{Size: size, Data: buf}, nil
}

// Blob transfer buckets, in chunks per user (§16.4): burst 400 (100 MiB),
// refill 40/s (10 MiB/s) for puts, twice that for gets.
const (
	blobBurst  = 400
	blobRefill = 40
)

// checkFiles validates send's files before the transaction (§16.2): the
// shape of each, and in plaintext channels the metadata (e2e is decided in
// the transaction; it must then carry blob ids only).
func checkFiles(files []proto.File, maxUpload int64) error {
	if len(files) > proto.MaxFiles {
		return badRequest(fmt.Sprintf("at most %d files per message", proto.MaxFiles))
	}
	thumbs := 0
	for i, f := range files {
		if len(f.Blob) != proto.BlobIDSize {
			return badRequest("file blob id must be 16 bytes")
		}
		for _, g := range files[:i] {
			if bytes.Equal(g.Blob, f.Blob) {
				return badRequest("file attached twice")
			}
		}
		thumbs += len(f.Thumb)
		if f.Size > uint64(maxUpload) {
			return proto.Errf(proto.ErrTooLarge, "file over the server's limit")
		}
	}
	if thumbs > proto.MaxThumb {
		return proto.Errf(proto.ErrTooLarge, "thumbnails over 32 KiB")
	}
	return nil
}

// fileMetaOK is a plaintext channel's File: a name, a type, a key and a hash.
func fileMetaOK(f proto.File) bool {
	return f.Name != "" && validLine(f.Name, proto.MaxFileName) && strings.TrimSpace(f.Name) == f.Name &&
		validMime(f.Mime) && len(f.Key) == e2ecrypto.KeySize && len(f.Hash) == 32
}

func validMime(m string) bool {
	if m == "" || len(m) > proto.MaxMime || strings.Count(m, "/") != 1 {
		return false
	}
	for _, r := range m {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("/.+-_", r)) {
			return false
		}
	}
	return true
}

// blobOnly reports whether f carries nothing but its blob id (e2e).
func blobOnly(f proto.File) bool {
	return f.Name == "" && f.Size == 0 && f.Mime == "" && f.Key == nil && f.Hash == nil && f.Thumb == nil && f.W == 0 && f.H == 0
}

// attachFiles ties a new message's files to their blobs inside its
// transaction: each must be the author's, finished, uploaded for this
// channel and not attached yet; in a plaintext channel its size must be
// the sealed size of the declared one.
func attachFiles(ctx context.Context, tx *sql.Tx, files []proto.File, e2e bool, msg, author, ch uint64) error {
	for _, f := range files {
		if e2e && !blobOnly(f) {
			return badRequest("an encrypted channel's files carry only blob ids")
		}
		if !e2e && !fileMetaOK(f) {
			return badRequest("invalid file name, type, key or hash")
		}
		var size uint64
		err := tx.QueryRowContext(ctx, `UPDATE blobs SET msg = ? WHERE id = ? AND owner = ? AND chan = ? AND msg IS NULL AND have = size
			RETURNING size`, msg, f.Blob, author, ch).Scan(&size)
		if errors.Is(err, sql.ErrNoRows) {
			return badRequest("file not uploaded, expired or already attached")
		}
		if err != nil {
			return err
		}
		if !e2e && uint64(e2ecrypto.FileSealedSize(int64(f.Size))) != size {
			return badRequest("file size doesn't match its upload")
		}
	}
	return nil
}

// dropMsgBlobs deletes a message's blob rows inside its delete's
// transaction and returns their ids; removeBlobFiles removes the files
// after the commit.
func dropMsgBlobs(ctx context.Context, tx *sql.Tx, msg uint64) ([][]byte, error) {
	rows, err := tx.QueryContext(ctx, `DELETE FROM blobs WHERE msg = ? RETURNING id`, msg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids [][]byte
	for rows.Next() {
		var id []byte
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Server) removeBlobFiles(ids [][]byte) {
	if len(ids) == 0 {
		return
	}
	s.blobs.mu.Lock()
	defer s.blobs.mu.Unlock()
	for _, id := range ids {
		if err := os.Remove(s.blobs.path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("removing blob", "err", err) // the stray sweep retries
		}
	}
}

// blobLoop collects unattached uploads and stray files now and every blobGCEvery.
func (s *Server) blobLoop(ctx context.Context) {
	t := time.NewTicker(blobGCEvery)
	defer t.Stop()
	for {
		if err := s.gcBlobs(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("collecting blobs", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// gcBlobs removes uploads never attached within blobPendingTTL, rows of
// blobs whose message is gone or deleted, and files without a row.
func (s *Server) gcBlobs(ctx context.Context) error {
	bs := s.blobs
	bs.mu.Lock()
	defer bs.mu.Unlock()
	cutoff := s.cfg.Now().Add(-blobPendingTTL).UnixMilli()
	rows, err := s.st.db.QueryContext(ctx, `DELETE FROM blobs WHERE (msg IS NULL AND created < ?)
		OR (msg IS NOT NULL AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = blobs.msg AND m.deleted = 0)) RETURNING id`, cutoff)
	if err != nil {
		return err
	}
	var ids [][]byte
	for rows.Next() {
		var id []byte
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := os.Remove(bs.path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	// Stray files: no row (removed after a crash, or a restored database).
	stray := time.Now().Add(-blobStrayAge)
	return filepath.WalkDir(bs.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || ctx.Err() != nil {
			return ctx.Err()
		}
		id, herr := hex.DecodeString(d.Name())
		if herr != nil || len(id) != proto.BlobIDSize {
			return nil // not ours
		}
		info, ierr := d.Info()
		if ierr != nil || info.ModTime().After(stray) {
			return nil
		}
		var n int
		if err := s.st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blobs WHERE id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			os.Remove(p)
		}
		return nil
	})
}
