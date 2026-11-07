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

// DefaultUserQuota is the default room for one person's files (--user-quota).
const DefaultUserQuota = 1 << 30

func (s *Server) userQuota() int64 {
	if s.cfg.UserQuota > 0 {
		return s.cfg.UserQuota
	}
	return DefaultUserQuota
}

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
	limited := member && !s.take(s.blimits, u, blobBurst, blobRefill)
	s.mu.Unlock()
	if !member {
		return nil, errForbidden
	}
	if limited {
		return nil, proto.Errf(proto.ErrRateLimited, "slow down")
	}
	bs := s.blobs
	bs.mu.Lock() // lock order: blobs.mu, then Server.mu (s.write)
	defer bs.mu.Unlock()
	path := bs.path(a.Blob)
	var ch, owner, size, have uint64
	var attached, created bool
	// Rows change only through s.write, like every other write: a
	// transaction of its own here would make concurrent sends fail with
	// SQLITE_BUSY_SNAPSHOT.
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		var msg sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT chan, owner, size, have, msg FROM blobs WHERE id = ?`, a.Blob).
			Scan(&ch, &owner, &size, &have, &msg)
		if errors.Is(err, sql.ErrNoRows) {
			if a.Off != 0 {
				// Collected meanwhile (or never made): start again.
				return nil, proto.Errf(proto.ErrNotFound, "upload expired")
			}
			if err := s.blobRoom(ctx, tx, u, a.Size); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO blobs (id, chan, owner, size, have, created) VALUES (?, ?, ?, ?, 0, ?)`,
				a.Blob, a.Chan, u, a.Size, s.now()); err != nil {
				return nil, err
			}
			ch, owner, size, created = a.Chan, u, a.Size, true
			return nil, nil
		}
		attached = msg.Valid
		return nil, err
	})
	if err != nil {
		return nil, err
	}
	if created {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		f.Close()
	}
	switch {
	case owner != u:
		return nil, errNotFound
	case ch != a.Chan || size != a.Size:
		return nil, proto.Errf(proto.ErrConflict, "blob id in use for another upload")
	case attached:
		return proto.BlobPutRes{Have: have}, nil
	}
	// After a crash the row may say more than reached the disk (only the
	// last chunk is synced): go on from what the file holds.
	if st, err := os.Stat(path); err != nil {
		return nil, err
	} else if uint64(st.Size()) < have {
		have = uint64(st.Size())
		if err := s.setBlobHave(ctx, a.Blob, have); err != nil {
			return nil, err
		}
	}
	if a.Off != have || len(a.Data) == 0 {
		return proto.BlobPutRes{Have: have}, nil
	}
	if err := writeBlobAt(path, a.Data, int64(have), have+uint64(len(a.Data)) == size); err != nil {
		return nil, err
	}
	have += uint64(len(a.Data))
	if err := s.setBlobHave(ctx, a.Blob, have); err != nil {
		return nil, err
	}
	return proto.BlobPutRes{Have: have}, nil
}

// setBlobHave records how far a blob got, and when (an upload that keeps
// moving isn't collected).
func (s *Server) setBlobHave(ctx context.Context, id []byte, have uint64) error {
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		_, err := tx.ExecContext(ctx, `UPDATE blobs SET have = ?, created = ? WHERE id = ? AND msg IS NULL`, have, s.now(), id)
		return nil, err
	})
	return err
}

// blobCharge is what a blob counts against quotas: at least one disk
// block, so empty files don't come for free.
const blobCharge = `MAX(size, 4096)`

// blobRoom checks a new blob of size against the caller's unfinished
// uploads, the caller's share and the server's quota.
func (s *Server) blobRoom(ctx context.Context, tx *sql.Tx, u, size uint64) error {
	charge := max(int64(size), 4096)
	var n int
	var pending, mine, total int64
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE msg IS NULL), COALESCE(SUM(`+blobCharge+`) FILTER (WHERE msg IS NULL), 0),
		COALESCE(SUM(`+blobCharge+`), 0) FROM blobs WHERE owner = ?`, u).Scan(&n, &pending, &mine)
	if err != nil {
		return err
	}
	if n >= proto.MaxPendingBlobs || pending+charge > int64(proto.MaxFiles)*s.maxBlob() {
		return proto.Errf(proto.ErrConflict, "too many unfinished uploads; send or wait for them")
	}
	if mine+charge > s.userQuota() {
		return proto.Errf(proto.ErrTooLarge, "you've used your share of the server's file storage; delete old files")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+blobCharge+`), 0) FROM blobs`).Scan(&total); err != nil {
		return err
	}
	if total+charge > s.blobQuota() {
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
	s.mu.Lock()
	limited := !s.take(s.glimits, u, blobBurst, 2*blobRefill)
	s.mu.Unlock()
	if limited {
		return nil, proto.Errf(proto.ErrRateLimited, "slow down")
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
	s.mu.Unlock()
	if !visible {
		return nil, errNotFound
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
		proto.ValidMime(f.Mime) && len(f.Key) == e2ecrypto.KeySize && len(f.Hash) == 32
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
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM blobs WHERE id = ?`, f.Blob).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return proto.Errf(proto.ErrNotFound, "file upload expired; upload it again")
			}
			return badRequest("file not uploaded yet, someone else's, for another channel or already attached")
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
	s.mu.Lock() // tests swap cfg.Now under it
	cutoff := s.cfg.Now().Add(-blobPendingTTL).UnixMilli()
	s.mu.Unlock()
	var ids [][]byte
	bs.mu.Lock()
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		ids = nil
		rows, err := tx.QueryContext(ctx, `DELETE FROM blobs WHERE (msg IS NULL AND created < ?)
			OR (msg IS NOT NULL AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = blobs.msg AND m.deleted = 0)) RETURNING id`, cutoff)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id []byte
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return nil, rows.Err()
	})
	if err == nil {
		for _, id := range ids {
			if rerr := os.Remove(bs.path(id)); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = rerr
			}
		}
	}
	bs.mu.Unlock()
	if err != nil {
		return err
	}
	// Stray files (no row: removed after a crash, or a restored database).
	// Candidates are found without the lock, then each is checked again
	// under it, so uploads and deletes never wait on the walk.
	stray := time.Now().Add(-blobStrayAge)
	var cands []string
	root, err := filepath.EvalSymlinks(bs.dir) // the operator may have moved it
	if err != nil {
		return err
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || ctx.Err() != nil {
			return ctx.Err()
		}
		if id, herr := hex.DecodeString(d.Name()); herr != nil || len(id) != proto.BlobIDSize {
			return nil // not ours
		}
		if info, ierr := d.Info(); ierr == nil && info.ModTime().Before(stray) {
			cands = append(cands, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, p := range cands {
		id, _ := hex.DecodeString(filepath.Base(p))
		bs.mu.Lock()
		var n int
		err := s.st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blobs WHERE id = ?`, id).Scan(&n)
		if err == nil && n == 0 {
			os.Remove(p)
		}
		bs.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
