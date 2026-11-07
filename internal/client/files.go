package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/media"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Files and images (PROTOCOL.md §16).
//
// Sending: SendFiles seals each file at once with a fresh key into
// TC_HOME/uploads (so the original may change or go away), makes a
// thumbnail of images, and queues an upload. The upload loop puts the
// sealed blobs on the server, resuming where it stopped after a
// disconnect, then hands the message (caption plus the files' keys and
// metadata) to the outbox, which sends it like any other: sealed and
// signed in encrypted channels, so the server sees only blobs and sizes.
//
// Receiving: Download fetches a blob, opens it with the file's key (a
// truncated, extended, reordered or swapped blob fails), checks the size
// and hash the message promised, and saves it under a safe name that never
// overwrites anything. Nothing is ever opened or run by itself.

// MaxDownload bounds a file this client fetches, whatever a message claims.
const MaxDownload = 2 << 30

// ErrFileCheck: a downloaded file didn't match what its message promised.
var ErrFileCheck = errors.New("the file failed its integrity check (corrupted or tampered with on the server); nothing was saved")

// validFiles is what a received message's files must look like (§16.2).
func validFiles(fs []proto.File) bool {
	if len(fs) > proto.MaxFiles {
		return false
	}
	thumbs := 0
	for i, f := range fs {
		if len(f.Blob) != proto.BlobIDSize || len(f.Key) != e2ecrypto.KeySize || len(f.Hash) != sha256.Size ||
			f.Name == "" || len(f.Name) > proto.MaxFileName || !sanitize.Valid(f.Name) || strings.ContainsAny(f.Name, "\n\t") ||
			f.Mime == "" || len(f.Mime) > proto.MaxMime || !sanitize.Valid(f.Mime) || f.Size > MaxDownload {
			return false
		}
		for _, g := range fs[:i] {
			if bytes.Equal(g.Blob, f.Blob) {
				return false
			}
		}
		thumbs += len(f.Thumb)
	}
	return thumbs <= proto.MaxThumb
}

// blobIDs is files as an encrypted channel's send carries them.
func blobIDs(fs []proto.File) []proto.File {
	if len(fs) == 0 {
		return nil
	}
	out := make([]proto.File, len(fs))
	for i, f := range fs {
		out[i] = proto.File{Blob: f.Blob}
	}
	return out
}

// FallbackBody is the text of a message that attaches files without a
// caption: what clients without files support show. The TUI hides it
// under the files' cards.
func FallbackBody(fs []proto.File) string {
	names := make([]string, len(fs))
	for i, f := range fs {
		names[i] = media.SafeName(f.Name)
	}
	return "📎 " + strings.Join(names, ", ")
}

// MaxUpload is the largest file the server takes (WELCOME max_upload, or
// the last one seen; proto.DefaultMaxUpload before any). 0 if the server
// doesn't take files.
func (c *Client) MaxUpload() int64 {
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s != nil {
		if !slices.Contains(s.welcome.Caps, proto.CapBlob) {
			return 0
		}
		if s.welcome.MaxUpload > 0 {
			return int64(s.welcome.MaxUpload)
		}
	}
	if v, err := metaUint(c.db, "max_upload"); err == nil && v > 0 {
		return int64(v)
	}
	return proto.DefaultMaxUpload
}

func (c *Client) uploadsDir() string { return filepath.Join(c.home, "uploads") }

func (c *Client) stagedPath(blob []byte) string {
	return filepath.Join(c.uploadsDir(), hex.EncodeToString(blob))
}

// SendFiles queues paths as one message into ch (thread: a reply), with
// caption as its text ("" = FallbackBody). Each file is read and sealed
// now; the upload runs when online. It returns the upload's id as
// Message.Local shows it on the pending message (negative).
func (c *Client) SendFiles(ch, thread uint64, caption string, paths ...string) (int64, error) {
	if ch == 0 || len(paths) == 0 {
		return 0, errors.New("send: no channel or file")
	}
	if len(paths) > proto.MaxFiles {
		return 0, fmt.Errorf("at most %d files per message", proto.MaxFiles)
	}
	limit := c.MaxUpload()
	if limit == 0 {
		return 0, errors.New("this server doesn't take files (update it)")
	}
	if err := c.sendAllowed(ch); err != nil {
		return 0, err
	}
	if err := ensurePrivateDir(c.uploadsDir()); err != nil {
		return 0, err
	}
	var files []proto.File
	staged := []string{}
	ok := false
	defer func() {
		if !ok {
			for _, p := range staged {
				os.Remove(p)
			}
		}
	}()
	thumbs := 0
	for _, p := range paths {
		f, err := c.stageFile(p, limit, proto.MaxThumb-thumbs)
		if err != nil {
			return 0, err
		}
		staged = append(staged, c.stagedPath(f.Blob))
		thumbs += len(f.Thumb)
		files = append(files, f)
	}
	body := caption
	if strings.TrimSpace(body) == "" {
		body = FallbackBody(files)
	}
	if len(body) > proto.MaxBody || !validText(body) {
		return 0, proto.Errf(proto.ErrBadRequest, "caption too long or has control characters")
	}
	if c.needsEnc(ch) {
		// Everything travels in one sealed message: it must fit.
		for plainSize(body, files) > e2ecrypto.MaxPlainSize-1024 {
			i := slices.IndexFunc(files, func(f proto.File) bool { return f.Thumb != nil })
			if i < 0 {
				return 0, proto.Errf(proto.ErrTooLarge, "caption too long to go with files")
			}
			files[i].Thumb, files[i].W, files[i].H = nil, 0, 0
		}
	}
	raw, err := proto.Marshal(files)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := c.db.QueryRow(`INSERT INTO uploads (chan, thread, body, files, time) VALUES (?, ?, ?, ?, ?) RETURNING id`,
		ch, thread, body, raw, time.Now().UnixMilli()).Scan(&id); err != nil {
		return 0, fmt.Errorf("queueing upload: %w", err)
	}
	ok = true
	c.uploadsChanged(ch)
	return -id, nil
}

func plainSize(body string, files []proto.File) int {
	b, _ := proto.Marshal(plainMsg{T: "msg", TS: time.Now().UnixMilli(), Body: body, Files: files,
		ThreadRef: make([]byte, e2ecrypto.IDSize)})
	return len(b)
}

// stageFile seals the file at path into the uploads dir and describes it.
// thumbRoom is how many thumbnail bytes this message has left.
func (c *Client) stageFile(path string, limit int64, thumbRoom int) (proto.File, error) {
	st, err := os.Stat(path)
	if err != nil {
		return proto.File{}, err
	}
	if !st.Mode().IsRegular() {
		return proto.File{}, fmt.Errorf("%s isn't a regular file", sanitize.Line(filepath.Base(path)))
	}
	if st.Size() > limit {
		return proto.File{}, proto.Errf(proto.ErrTooLarge, fmt.Sprintf("%s is %s; the server takes up to %s",
			sanitize.Line(filepath.Base(path)), media.Size(uint64(st.Size())), media.Size(uint64(limit))))
	}
	src, err := os.Open(path)
	if err != nil {
		return proto.File{}, err
	}
	defer src.Close()
	head := make([]byte, media.SniffLen)
	n, _ := io.ReadFull(src, head)
	head = head[:n]
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return proto.File{}, err
	}
	f := proto.File{Blob: make([]byte, proto.BlobIDSize), Key: make([]byte, e2ecrypto.KeySize),
		Name: media.SafeName(filepath.Base(path)), Mime: media.Sniff(head, path)}
	rand.Read(f.Blob)
	rand.Read(f.Key)
	var r io.Reader = src
	if media.IsImage(f.Mime) && thumbRoom > 0 {
		// Images are read whole (≤ limit) for the thumbnail.
		b, err := io.ReadAll(io.LimitReader(src, limit+1))
		if err != nil {
			return proto.File{}, err
		}
		if th, w, h, err := media.Thumbnail(b); err == nil && len(th) <= thumbRoom {
			f.Thumb, f.W, f.H = th, uint32(w), uint32(h)
		} else if w, h, _, err := media.ImageSize(bytes.NewReader(b)); err == nil {
			f.W, f.H = uint32(w), uint32(h)
		}
		r = bytes.NewReader(b)
	}
	out, err := os.CreateTemp(c.uploadsDir(), ".staging-*")
	if err != nil {
		return proto.File{}, err
	}
	defer os.Remove(out.Name())
	size, sum, err := e2ecrypto.SealFile(out, r, e2ecrypto.Key(f.Key), limit)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, e2ecrypto.ErrTooLarge) {
		return proto.File{}, proto.Errf(proto.ErrTooLarge, "file grew past the server's limit while reading it")
	}
	if err != nil {
		return proto.File{}, fmt.Errorf("sealing %s: %w", sanitize.Line(filepath.Base(path)), err)
	}
	if err := os.Rename(out.Name(), c.stagedPath(f.Blob)); err != nil {
		return proto.File{}, err
	}
	f.Size, f.Hash = uint64(size), sum[:]
	return f, nil
}

// ---- uploading ----

type upload struct {
	id     int64
	ch     uint64
	thread uint64
	body   string
	files  []proto.File
	time   int64
	failed string
}

const uploadCols = `id, chan, thread, body, files, time, failed`

func scanUpload(sc interface{ Scan(...any) error }) (upload, error) {
	var u upload
	var raw []byte
	err := sc.Scan(&u.id, &u.ch, &u.thread, &u.body, &raw, &u.time, &u.failed)
	if err == nil {
		err = proto.Unmarshal(raw, &u.files)
	}
	return u, err
}

// upProgress is how far the running upload got (bytes of all its blobs).
type upProgress struct {
	mu         sync.Mutex
	id         int64
	sent, size int64
	posted     time.Time
}

func (c *Client) uploadsChanged(ch uint64) {
	u := Update{}
	u.msgs(ch)
	c.post(u)
	select {
	case c.upKick <- struct{}{}:
	default:
	}
}

// uploadLoop uploads queued files oldest first, one at a time, while the
// session lasts; the next session resumes what's left.
func (c *Client) uploadLoop(ctx context.Context, s *session) {
	c.sweepFiles()
	if !slices.Contains(s.welcome.Caps, proto.CapBlob) {
		return
	}
	if s.welcome.MaxUpload > 0 {
		_ = setMeta(c.db, "max_upload", s.welcome.MaxUpload)
	}
	for {
		up, err := scanUpload(c.db.QueryRow(`SELECT ` + uploadCols + ` FROM uploads WHERE failed = '' ORDER BY id LIMIT 1`))
		if errors.Is(err, sql.ErrNoRows) {
			select {
			case <-ctx.Done():
				return
			case <-c.upKick:
			}
			continue
		}
		if err == nil {
			err = c.runUpload(ctx, s, up)
		}
		var perr *proto.Error
		var ferr failErr
		reason := ""
		switch {
		case errors.As(err, &ferr):
			reason = ferr.reason
		case errors.As(err, &perr) && permanent(perr.Code):
			reason = sanitize.Line(perr.Error())
		case err != nil && !errors.Is(err, ErrOffline) && ctx.Err() == nil && !isTransient(err):
			reason = sanitize.Line(err.Error())
		}
		switch {
		case err == nil:
		case reason != "":
			if _, err := c.db.Exec(`UPDATE uploads SET failed = ? WHERE id = ?`, "not sent: "+reason, up.id); err == nil {
				up.failed = "not sent: " + reason
				m := up.message(c.Me().ID)
				u := Update{Failed: []Message{m}}
				u.msgs(up.ch)
				c.post(u)
			}
		case errors.Is(err, ErrOffline) || ctx.Err() != nil:
			return
		default:
			t := time.NewTimer(c.timing.retry)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

// isTransient: a local error worth retrying (none so far).
func isTransient(error) bool { return false }

// runUpload puts each of up's blobs, then queues its message.
func (c *Client) runUpload(ctx context.Context, s *session, up upload) error {
	var total int64
	for _, f := range up.files {
		total += e2ecrypto.FileSealedSize(int64(f.Size))
	}
	c.up.mu.Lock()
	c.up.id, c.up.sent, c.up.size = up.id, 0, total
	c.up.mu.Unlock()
	defer func() {
		c.up.mu.Lock()
		c.up.id = 0
		c.up.mu.Unlock()
	}()
	var done int64
	for _, f := range up.files {
		size := e2ecrypto.FileSealedSize(int64(f.Size))
		if err := c.putBlob(ctx, s, up.ch, f, size, done); err != nil {
			return err
		}
		done += size
	}
	// Every blob is on the server: the message goes to the outbox, which
	// sends it (sealed in an encrypted channel).
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var still int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM uploads WHERE id = ? AND failed = ''`, up.id).Scan(&still); err != nil {
		return err
	}
	if still == 0 {
		return nil // discarded meanwhile
	}
	if _, err := c.queueOutbox(tx, outboxEntry{chan_: up.ch, thread: up.thread, body: up.body, time: time.Now().UnixMilli(),
		files: up.files, upload: up.id}); err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) {
			return err
		}
		return failErr{sanitize.Line(err.Error())}
	}
	if _, err := tx.Exec(`DELETE FROM uploads WHERE id = ?`, up.id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, f := range up.files {
		os.Remove(c.stagedPath(f.Blob))
	}
	c.outboxChanged(up.ch, nil)
	return nil
}

// putBlob uploads one sealed file from where the server has it up to.
func (c *Client) putBlob(ctx context.Context, s *session, ch uint64, f proto.File, size, before int64) error {
	args := proto.BlobPutArgs{Blob: f.Blob, Chan: ch, Size: uint64(size)}
	var r proto.BlobPutRes
	restarted := false
	for {
		raw, err := s.call(ctx, proto.OpBlobPut, args, nil)
		var pe *proto.Error
		if errors.As(err, &pe) && pe.Code == proto.ErrNotFound && !restarted {
			// Collected while we were away: start this blob over.
			restarted = true
			args.Off, args.Data = 0, nil
			continue
		}
		if err != nil {
			return err
		}
		if err := proto.Unmarshal(raw, &r); err != nil {
			return err
		}
		c.progress(ch, before+int64(r.Have))
		if r.Have >= uint64(size) {
			return nil
		}
		chunk, err := c.readStaged(f.Blob, int64(r.Have), min(proto.MaxBlobChunk, size-int64(r.Have)))
		if err != nil {
			return failErr{"the file to send is gone from " + sanitize.Line(c.uploadsDir())}
		}
		args.Off, args.Data = r.Have, chunk
	}
}

func (c *Client) readStaged(blob []byte, off, n int64) ([]byte, error) {
	f, err := os.Open(c.stagedPath(blob))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil {
		return nil, err
	}
	return b, nil
}

func (c *Client) progress(ch uint64, sent int64) {
	c.up.mu.Lock()
	c.up.sent = sent
	post := time.Since(c.up.posted) > 200*time.Millisecond || sent >= c.up.size
	if post {
		c.up.posted = time.Now()
	}
	c.up.mu.Unlock()
	if post {
		u := Update{}
		u.msgs(ch)
		c.post(u)
	}
}

// message shows a pending upload like a pending message.
func (up upload) message(me uint64) Message {
	return Message{
		Message: proto.Message{Chan: up.ch, Author: me, Body: up.body, Thread: up.thread, Time: up.time, Files: up.files},
		Pending: up.failed == "", Failed: up.failed, Local: -up.id,
	}
}

// uploadMsgs lists pending uploads as messages (where: an SQL filter).
func (c *Client) uploadMsgs(where string, args ...any) []Message {
	rows, err := c.db.Query(`SELECT `+uploadCols+` FROM uploads WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	me := c.Me().ID
	var out []Message
	for rows.Next() {
		up, err := scanUpload(rows)
		if err != nil {
			return out
		}
		m := up.message(me)
		for _, f := range up.files {
			m.UpTotal += e2ecrypto.FileSealedSize(int64(f.Size))
		}
		c.up.mu.Lock()
		if c.up.id == up.id {
			m.UpSent = c.up.sent
		}
		c.up.mu.Unlock()
		out = append(out, m)
	}
	return out
}

// retryUpload / discardUpload are RetryFailed / DiscardFailed for an upload.
func (c *Client) retryUpload(id int64) error {
	var ch uint64
	if err := c.db.QueryRow(`UPDATE uploads SET failed = '' WHERE id = ? AND failed != '' RETURNING chan`, id).Scan(&ch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("no such failed upload")
		}
		return err
	}
	c.uploadsChanged(ch)
	return nil
}

func (c *Client) discardUpload(id int64) error {
	up, err := scanUpload(c.db.QueryRow(`SELECT `+uploadCols+` FROM uploads WHERE id = ?`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("no such upload")
		}
		return err
	}
	if up.failed == "" {
		c.up.mu.Lock()
		running := c.up.id == id
		c.up.mu.Unlock()
		if running {
			return errors.New("that file is uploading; wait for it, or retry if it fails")
		}
	}
	if _, err := c.db.Exec(`DELETE FROM uploads WHERE id = ?`, id); err != nil {
		return err
	}
	for _, f := range up.files {
		os.Remove(c.stagedPath(f.Blob))
	}
	c.uploadsChanged(up.ch)
	return nil
}

// WaitUpload waits until the upload SendFiles returned (local, negative)
// was sent, and returns an error if it or its message was refused.
func (c *Client) WaitUpload(ctx context.Context, local int64) error {
	id := -local
	for {
		c.obMu.Lock()
		changed := c.obChanged
		c.obMu.Unlock()
		var failed string
		err := c.db.QueryRow(`SELECT failed FROM uploads WHERE id = ?`, id).Scan(&failed)
		switch {
		case err == nil && failed != "":
			return errors.New(failed)
		case errors.Is(err, sql.ErrNoRows):
			var ob int64
			if err := c.db.QueryRow(`SELECT id FROM outbox WHERE upload = ?`, id).Scan(&ob); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil // sent (or discarded)
				}
				return err
			}
			return c.FlushLocal(ctx, ob)
		case err != nil:
			return err
		}
		t := time.NewTimer(250 * time.Millisecond) // progress isn't an outbox change
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-changed:
			t.Stop()
		case <-t.C:
		}
	}
}

// ---- downloading ----

// DownloadsDir is where files are saved: $TC_DOWNLOADS, else ~/Downloads.
func DownloadsDir() string {
	if d := os.Getenv("TC_DOWNLOADS"); d != "" {
		return d
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, "Downloads")
	}
	return "."
}

// Downloadable reports whether file i of m can be fetched: a message
// whose text is trusted (plaintext, or decrypted and verified), not
// deleted, with a well-formed file.
func Downloadable(m Message, i int) bool {
	return !m.Deleted && m.ID != 0 && m.Sec.Trusted() && i >= 0 && i < len(m.Files) && validFiles(m.Files[i:i+1])
}

// Download fetches file i of cached message msg, checks it, and saves it
// in dir under its safe name, never overwriting a file (" (1)" etc.). It
// returns the saved path. progress (optional) gets bytes fetched so far.
func (c *Client) Download(ctx context.Context, msg uint64, i int, dir string, progress func(done, total int64)) (string, error) {
	m, ok, err := c.cachedMessage(msg)
	if err != nil {
		return "", err
	}
	if !ok || !Downloadable(m, i) {
		return "", errors.New("that file can't be downloaded")
	}
	f := m.Files[i]
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// The sealed blob goes to TC_HOME first, the plaintext to a hidden
	// temporary file next to its final name, renamed only once checked.
	if err := ensurePrivateDir(filepath.Join(c.home, "downloads")); err != nil {
		return "", err
	}
	sealed, err := os.CreateTemp(filepath.Join(c.home, "downloads"), "blob-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(sealed.Name())
	defer sealed.Close()
	size := e2ecrypto.FileSealedSize(int64(f.Size))
	if err := c.fetchBlob(ctx, f.Blob, size, sealed, progress); err != nil {
		return "", err
	}
	if _, err := sealed.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tc-download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	n, sum, err := e2ecrypto.OpenFile(tmp, sealed, e2ecrypto.Key(f.Key), int64(f.Size))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	cryptoErr := errors.Is(err, e2ecrypto.ErrDecrypt) || errors.Is(err, e2ecrypto.ErrFileTruncated) || errors.Is(err, e2ecrypto.ErrTooLarge)
	switch {
	case err != nil && !cryptoErr:
		return "", err // a local error (disk full)
	case err != nil || uint64(n) != f.Size || !bytes.Equal(sum[:], f.Hash):
		return "", ErrFileCheck
	}
	return placeFile(tmp.Name(), dir, media.SafeName(f.Name))
}

// cachedMessage is one cached message with its verdict.
func (c *Client) cachedMessage(id uint64) (Message, bool, error) {
	ms := c.queryMsgs(`SELECT `+msgCols+`, sec, sts, 0 FROM msgs WHERE id = ?`, id)
	if len(ms) == 0 {
		return Message{}, false, nil
	}
	return ms[0], true, nil
}

// fetchBlob reads a blob of the expected size into w, a few chunks in
// flight at a time.
func (c *Client) fetchBlob(ctx context.Context, blob []byte, size int64, w io.WriterAt, progress func(done, total int64)) error {
	const window = 4
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		off int64
		err error
		n   int
	}
	offs := make(chan int64)
	results := make(chan result)
	var wg sync.WaitGroup
	for range window {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for off := range offs {
				var g proto.BlobGetRes
				err := c.call(ctx, proto.OpBlobGet, proto.BlobGetArgs{Blob: blob, Off: uint64(off)}, &g, nil)
				want := min(proto.MaxBlobChunk, size-off)
				var pe *proto.Error
				if err == nil && (g.Size != uint64(size) || int64(len(g.Data)) != want) ||
					errors.As(err, &pe) && pe.Code == proto.ErrBadRequest {
					err = ErrFileCheck // not the blob the message described (shorter, longer)
				}
				if err == nil {
					_, err = w.WriteAt(g.Data, off)
				}
				select {
				case results <- result{off, err, len(g.Data)}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(offs)
		for off := int64(0); off < size; off += proto.MaxBlobChunk {
			select {
			case offs <- off:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	var done int64
	chunks := (size + proto.MaxBlobChunk - 1) / proto.MaxBlobChunk
	for got := int64(0); got < chunks; got++ {
		r, ok := <-results
		if !ok {
			if err := ctx.Err(); err != nil {
				return err
			}
			return ErrOffline
		}
		if r.err != nil {
			cancel()
			for range results {
			}
			return r.err
		}
		done += int64(r.n)
		if progress != nil {
			progress(done, size)
		}
	}
	return nil
}

// placeFile moves tmp into dir as name, or "name (1).ext" and so on if
// that exists: never over an existing file. It returns the path.
func placeFile(tmp, dir, name string) (string, error) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		cand := name
		if i > 0 {
			cand = base + " (" + strconv.Itoa(i) + ")" + ext
		}
		p := filepath.Join(dir, cand)
		// A hard link fails if p exists: no window where it could be replaced.
		err := os.Link(tmp, p)
		if err == nil {
			markDownloaded(p)
			return p, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue
		}
		// No hard links here (some filesystems): claim the name, then move.
		fh, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		fh.Close()
		if err := os.Rename(tmp, p); err != nil {
			os.Remove(p)
			return "", err
		}
		markDownloaded(p)
		return p, nil
	}
	return "", errors.New("too many files with that name in " + dir)
}

// sweepFiles removes what a crash or a new login left in TC_HOME's uploads
// and downloads dirs: sealed files no upload refers to, temporary files.
// Only files older than an hour, so it never races a SendFiles or Download.
func (c *Client) sweepFiles() {
	keep := map[string]bool{}
	rows, err := c.db.Query(`SELECT files FROM uploads`)
	if err != nil {
		return
	}
	for rows.Next() {
		var raw []byte
		var fs []proto.File
		if rows.Scan(&raw) == nil && proto.Unmarshal(raw, &fs) == nil {
			for _, f := range fs {
				keep[hex.EncodeToString(f.Blob)] = true
			}
		}
	}
	rows.Close()
	old := time.Now().Add(-time.Hour)
	for _, dir := range []string{c.uploadsDir(), filepath.Join(c.home, "downloads")} {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if info, err := e.Info(); err == nil && !e.IsDir() && !keep[e.Name()] && info.ModTime().Before(old) {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}
