package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The incarnation (PROTOCOL.md §6, cap incarnation) names one unbroken history
// of the event log. It lives in a file next to the database, together with a
// mark: the newest seq this data directory is known to have written and a
// fingerprint of that event. A database restored from a backup (sqlite3
// .backup, cp, anything) carries none of that: its log either lacks the
// marked event or holds a different one there, and the server starts a new
// incarnation, so clients holding a cursor from the lost history resync from
// scratch instead of mixing reused message ids into their cache.
const incarnationFile = "incarnation"

// incMarkEvery is how often a running server records its head. A backup
// taken in the last interval before a crash, then restored, goes unnoticed.
const incMarkEvery = time.Second

type incarnation struct {
	path     string
	serverID []byte
	inc      []byte

	mu      sync.Mutex // serializes writes of the file
	written bool
	lastSeq uint64
}

type incState struct {
	serverID []byte
	inc      []byte
	seq      uint64
	mark     []byte
}

// openIncarnation checks the database against the file and returns the
// incarnation to run as, writing a fresh one if the file is missing,
// unreadable, belongs to another database or doesn't match its log.
func openIncarnation(ctx context.Context, dir string, db *sql.DB, serverID []byte) (*incarnation, string, error) {
	in := &incarnation{path: filepath.Join(dir, incarnationFile), serverID: serverID}
	reason := ""
	st, err := readIncFile(in.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		reason = "new"
	case err != nil:
		reason = "unreadable"
	case !bytes.Equal(st.serverID, serverID):
		reason = "other server"
	default:
		ok, err := eventMatches(ctx, db, st.seq, st.mark)
		if err != nil {
			return nil, "", err
		}
		if ok {
			in.inc = st.inc
		} else {
			reason = "restored"
		}
	}
	if in.inc == nil {
		in.inc = make([]byte, 16)
		rand.Read(in.inc)
	}
	if err := in.record(ctx, db); err != nil {
		return nil, "", err
	}
	return in, reason, nil
}

// record writes the file with the database's current head, if it moved.
func (in *incarnation) record(ctx context.Context, db *sql.DB) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	var (
		seq       uint64
		kind      string
		tm        int64
		actor     uint64
		hasEvents = true
	)
	err := db.QueryRowContext(ctx, `SELECT seq, kind, time, actor FROM events ORDER BY seq DESC LIMIT 1`).Scan(&seq, &kind, &tm, &actor)
	if errors.Is(err, sql.ErrNoRows) {
		hasEvents, err = false, nil
	}
	if err != nil {
		return fmt.Errorf("reading head for incarnation: %w", err)
	}
	if in.written && seq == in.lastSeq {
		return nil
	}
	mark := "-"
	if hasEvents {
		mark = hex.EncodeToString(eventMark(seq, kind, tm, actor))
	}
	line := fmt.Sprintf("v1 %x %x %d %s\n", in.serverID, in.inc, seq, mark)
	if err := writeFileAtomic(in.path, []byte(line)); err != nil {
		return fmt.Errorf("writing incarnation: %w", err)
	}
	in.written, in.lastSeq = true, seq
	return nil
}

func eventMark(seq uint64, kind string, tm int64, actor uint64) []byte {
	h := sha256.New()
	var b [8]byte
	h.Write([]byte("tc-incarnation-mark\x00"))
	binary.BigEndian.PutUint64(b[:], seq)
	h.Write(b[:])
	h.Write([]byte(kind))
	h.Write([]byte{0})
	binary.BigEndian.PutUint64(b[:], uint64(tm))
	h.Write(b[:])
	binary.BigEndian.PutUint64(b[:], actor)
	h.Write(b[:])
	return h.Sum(nil)[:16]
}

// eventMatches reports whether the log holds the marked event. Events keep
// seq, kind, time and actor for good (edits rewrite only data, §6), and are
// never deleted.
func eventMatches(ctx context.Context, db *sql.DB, seq uint64, mark []byte) (bool, error) {
	if seq == 0 {
		return len(mark) == 0, nil
	}
	var (
		kind  string
		tm    int64
		actor uint64
	)
	err := db.QueryRowContext(ctx, `SELECT kind, time, actor FROM events WHERE seq = ?`, seq).Scan(&kind, &tm, &actor)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking incarnation mark: %w", err)
	}
	return bytes.Equal(eventMark(seq, kind, tm, actor), mark), nil
}

func readIncFile(path string) (incState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return incState{}, err
	}
	f := strings.Fields(string(b))
	if len(f) != 5 || f[0] != "v1" {
		return incState{}, errors.New("malformed incarnation file")
	}
	var st incState
	if st.serverID, err = hex.DecodeString(f[1]); err != nil {
		return st, err
	}
	if st.inc, err = hex.DecodeString(f[2]); err != nil || len(st.inc) != 16 {
		return st, errors.New("malformed incarnation")
	}
	if st.seq, err = strconv.ParseUint(f[3], 10, 64); err != nil {
		return st, err
	}
	if f[4] != "-" {
		if st.mark, err = hex.DecodeString(f[4]); err != nil {
			return st, err
		}
	}
	return st, nil
}

// writeFileAtomic replaces path so that a crash leaves the old or the new
// contents, never a torn file.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// incLoop records the head while the server runs, and once more on the way out.
func (s *Server) incLoop(ctx context.Context) {
	t := time.NewTicker(incMarkEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.inc.record(ctx, s.st.db); err != nil && ctx.Err() == nil {
				s.log.Warn("recording incarnation", "err", err)
			}
		}
	}
}
