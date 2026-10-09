package client

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/skailama/tc/internal/proto"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v);
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY, name TEXT NOT NULL, display TEXT NOT NULL,
	admin INTEGER NOT NULL, disabled INTEGER NOT NULL, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS chans (
	id INTEGER PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL, topic TEXT NOT NULL,
	members TEXT NOT NULL, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS msgs (
	id INTEGER PRIMARY KEY, chan INTEGER NOT NULL, author INTEGER NOT NULL, body TEXT NOT NULL,
	thread INTEGER NOT NULL, time INTEGER NOT NULL, edited INTEGER NOT NULL,
	deleted INTEGER NOT NULL, reactions TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS msgs_chan ON msgs (chan, thread, id);
CREATE INDEX IF NOT EXISTS msgs_thread ON msgs (thread, id);
CREATE TABLE IF NOT EXISTS reads (
	chan INTEGER PRIMARY KEY, seq INTEGER NOT NULL, dirty INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS outbox (
	id INTEGER PRIMARY KEY AUTOINCREMENT, nonce BLOB NOT NULL UNIQUE, chan INTEGER NOT NULL,
	thread INTEGER NOT NULL, body TEXT NOT NULL, time INTEGER NOT NULL,
	failed TEXT NOT NULL DEFAULT '', msg INTEGER NOT NULL DEFAULT 0, sent INTEGER NOT NULL DEFAULT 0);
`

// dbtx is the subset of *sql.DB / *sql.Tx the cache helpers need.
type dbtx interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func openCache(path string) (*sql.DB, error) {
	// Create with 0600 before SQLite does (it would honour the umask).
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creating cache: %w", err)
	}
	// An existing cache keeps its mode, and SQLite copies it to -wal/-shm.
	err = f.Chmod(0o600)
	f.Close()
	if err != nil {
		return nil, err
	}
	// IMMEDIATE transactions take the write lock up front, so busy_timeout
	// covers writer contention (e.g. `tc tail` and the TUI sharing a cache).
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening cache: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating cache schema: %w", err)
	}
	if err := migrateBots(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ---- meta ----

func metaUint(q dbtx, k string) (uint64, error) {
	var v uint64
	err := q.QueryRow(`SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

func metaBytes(q dbtx, k string) ([]byte, error) {
	var v []byte
	err := q.QueryRow(`SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

func setMeta(q dbtx, k string, v any) error {
	_, err := q.Exec(`INSERT INTO meta (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

// resetCache drops everything the server can resend; the outbox and read
// markers the server hasn't acknowledged yet survive.
func resetCache(q dbtx, serverID []byte) error {
	for _, s := range []string{
		`DELETE FROM users`, `DELETE FROM chans`, `DELETE FROM msgs`, `DELETE FROM reads WHERE dirty = 0`,
	} {
		if _, err := q.Exec(s); err != nil {
			return err
		}
	}
	if err := setMeta(q, "last_seq", 0); err != nil {
		return err
	}
	return setMeta(q, "server_id", serverID)
}

// ---- users ----

func upsertUser(q dbtx, u proto.User) error {
	_, err := q.Exec(`INSERT OR REPLACE INTO users (id, name, display, admin, disabled, created, bot)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, u.ID, u.Name, u.Display, u.Admin, u.Disabled, u.Created, u.Bot)
	return err
}

func loadUsers(q dbtx) (map[uint64]proto.User, error) {
	rows, err := q.Query(`SELECT id, name, display, admin, disabled, created, bot FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[uint64]proto.User{}
	for rows.Next() {
		var u proto.User
		if err := rows.Scan(&u.ID, &u.Name, &u.Display, &u.Admin, &u.Disabled, &u.Created, &u.Bot); err != nil {
			return nil, err
		}
		m[u.ID] = u
	}
	return m, rows.Err()
}

// ---- channels ----

func upsertChan(q dbtx, c proto.Channel) error {
	members, _ := json.Marshal(c.Members)
	_, err := q.Exec(`INSERT OR REPLACE INTO chans (id, name, kind, topic, members, created)
		VALUES (?, ?, ?, ?, ?, ?)`, c.ID, c.Name, c.Kind, c.Topic, string(members), c.Created)
	return err
}

const chanCols = `id, name, kind, topic, members, created`

func scanChan(sc interface{ Scan(...any) error }) (proto.Channel, error) {
	var c proto.Channel
	var members string
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &c.Topic, &members, &c.Created); err != nil {
		return c, err
	}
	_ = json.Unmarshal([]byte(members), &c.Members)
	return c, nil
}

func getChan(q dbtx, id uint64) (proto.Channel, bool, error) {
	c, err := scanChan(q.QueryRow(`SELECT `+chanCols+` FROM chans WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// setMember adds or removes user from a cached channel's member list.
func setMember(q dbtx, chanID, user uint64, in bool) error {
	c, ok, err := getChan(q, chanID)
	if err != nil || !ok {
		return err
	}
	out := c.Members[:0:0]
	for _, m := range c.Members {
		if m != user {
			out = append(out, m)
		}
	}
	if in {
		out = append(out, user)
	}
	c.Members = out
	return upsertChan(q, c)
}

// ---- messages ----

const msgCols = `id, chan, author, body, thread, time, edited, deleted, reactions`

func upsertMsg(q dbtx, m proto.Message, replace bool) error {
	verb := "INSERT OR REPLACE"
	if !replace {
		verb = "INSERT OR IGNORE"
	}
	_, err := q.Exec(verb+` INTO msgs (`+msgCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Chan, m.Author, m.Body, m.Thread, m.Time, m.Edited, m.Deleted, encodeReactions(m.Reactions))
	return err
}

func encodeReactions(r map[string][]uint64) string {
	if len(r) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(r)
	return string(b)
}

func scanMsg(sc interface{ Scan(...any) error }, extra ...any) (proto.Message, error) {
	var m proto.Message
	var reactions string
	dst := append([]any{&m.ID, &m.Chan, &m.Author, &m.Body, &m.Thread, &m.Time, &m.Edited, &m.Deleted, &reactions}, extra...)
	if err := sc.Scan(dst...); err != nil {
		return m, err
	}
	if reactions != "{}" {
		_ = json.Unmarshal([]byte(reactions), &m.Reactions)
	}
	return m, nil
}

func getMsg(q dbtx, id uint64) (proto.Message, bool, error) {
	m, err := scanMsg(q.QueryRow(`SELECT `+msgCols+` FROM msgs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}

// react toggles user in msg's emoji set. Returns the message's channel, or 0
// if the message isn't cached.
func react(q dbtx, msgID uint64, emoji string, user uint64, add bool) (uint64, error) {
	m, ok, err := getMsg(q, msgID)
	if err != nil || !ok {
		return 0, err
	}
	users := m.Reactions[emoji]
	var out []uint64
	for _, u := range users {
		if u != user {
			out = append(out, u)
		}
	}
	if add {
		out = append(out, user)
	}
	if m.Reactions == nil {
		m.Reactions = map[string][]uint64{}
	}
	if len(out) == 0 {
		delete(m.Reactions, emoji)
	} else {
		m.Reactions[emoji] = out
	}
	_, err = q.Exec(`UPDATE msgs SET reactions = ? WHERE id = ?`, encodeReactions(m.Reactions), msgID)
	return m.Chan, err
}

// ---- read markers ----

// setRead moves a read marker forward. Local marks are dirty until the
// server echoes a marker at least as far.
func setRead(q dbtx, chanID, seq uint64, local bool) error {
	if local {
		_, err := q.Exec(`INSERT INTO reads (chan, seq, dirty) VALUES (?, ?, 1)
			ON CONFLICT (chan) DO UPDATE SET
				dirty = CASE WHEN excluded.seq > seq THEN 1 ELSE dirty END,
				seq = max(seq, excluded.seq)`, chanID, seq)
		return err
	}
	_, err := q.Exec(`INSERT INTO reads (chan, seq, dirty) VALUES (?, ?, 0)
		ON CONFLICT (chan) DO UPDATE SET
			dirty = CASE WHEN excluded.seq >= seq THEN 0 ELSE dirty END,
			seq = max(seq, excluded.seq)`, chanID, seq)
	return err
}

// ---- outbox ----

type outboxEntry struct {
	id     int64
	nonce  []byte
	chan_  uint64
	thread uint64
	body   string
	time   int64
	failed string
	msg    uint64
}

const outboxCols = `id, nonce, chan, thread, body, time, failed, msg`

func scanOutbox(sc interface{ Scan(...any) error }) (outboxEntry, error) {
	var e outboxEntry
	err := sc.Scan(&e.id, &e.nonce, &e.chan_, &e.thread, &e.body, &e.time, &e.failed, &e.msg)
	return e, err
}

// matchOutbox hides the oldest unacknowledged outbox copy of a message of
// ours that just arrived, so the real and pending copies never show
// together. Only entries already sent at least once can match (an older
// message with the same text must not hide a queued one); a wrong match
// among those only hides a pending copy early, and its RES still removes it.
func matchOutbox(q dbtx, m proto.Message) error {
	_, err := q.Exec(`UPDATE outbox SET msg = ? WHERE id = (
		SELECT id FROM outbox WHERE msg = 0 AND sent = 1 AND failed = '' AND chan = ? AND thread = ? AND body = ?
		ORDER BY id LIMIT 1)`, m.ID, m.Chan, m.Thread, m.Body)
	return err
}

// wipe empties the whole cache, outbox included (new login).
func wipe(q dbtx) error {
	for _, t := range []string{"meta", "users", "chans", "msgs", "reads", "outbox"} {
		if _, err := q.Exec(`DELETE FROM ` + t); err != nil {
			return err
		}
	}
	return nil
}

// likeEscape escapes LIKE wildcards with '\'.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ensurePrivateDir creates dir 0700, tightening it if it already exists.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}
