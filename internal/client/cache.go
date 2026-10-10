package client

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	members TEXT NOT NULL, created INTEGER NOT NULL,
	e2e INTEGER NOT NULL DEFAULT 0, epoch INTEGER NOT NULL DEFAULT 0, creator INTEGER NOT NULL DEFAULT 0,
	legacy INTEGER NOT NULL DEFAULT 0);
-- Private channels someone else removed us from: kept (with the history we
-- had) until we close them, so the UI can say so (membership.go).
CREATE TABLE IF NOT EXISTS removed (chan INTEGER PRIMARY KEY, actor INTEGER NOT NULL, time INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS msgs (
	id INTEGER PRIMARY KEY, chan INTEGER NOT NULL, author INTEGER NOT NULL, body TEXT NOT NULL,
	thread INTEGER NOT NULL, time INTEGER NOT NULL, edited INTEGER NOT NULL,
	deleted INTEGER NOT NULL, reactions TEXT NOT NULL,
	enc BLOB, sec INTEGER NOT NULL DEFAULT 0, mid BLOB,
	eenc BLOB, epend BLOB, ev INTEGER NOT NULL DEFAULT 0, ets INTEGER NOT NULL DEFAULT 0, denc BLOB, cmid BLOB,
	sts INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS msgs_chan ON msgs (chan, thread, id);
CREATE INDEX IF NOT EXISTS msgs_thread ON msgs (thread, id);
-- Encrypted reactions (encops.go): the server's (msg, user, tag, enc) and
-- our verdict: st 0 not judged yet, 1 verified (emoji is the real one), 2 invalid.
CREATE TABLE IF NOT EXISTS rencs (
	msg INTEGER NOT NULL, user INTEGER NOT NULL, tag TEXT NOT NULL, enc BLOB NOT NULL,
	emoji TEXT NOT NULL DEFAULT '', st INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (msg, user, tag));
CREATE TABLE IF NOT EXISTS reads (
	chan INTEGER PRIMARY KEY, seq INTEGER NOT NULL, dirty INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS outbox (
	id INTEGER PRIMARY KEY AUTOINCREMENT, nonce BLOB NOT NULL UNIQUE, chan INTEGER NOT NULL,
	thread INTEGER NOT NULL, body TEXT NOT NULL, time INTEGER NOT NULL,
	failed TEXT NOT NULL DEFAULT '', msg INTEGER NOT NULL DEFAULT 0, sent INTEGER NOT NULL DEFAULT 0,
	enc BLOB, encnonce BLOB);
-- Server-provided E2E objects (msgpack Signed), unverified; see devices.go.
CREATE TABLE IF NOT EXISTS devices (cert BLOB PRIMARY KEY, dev BLOB NOT NULL, user INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS devices_dev ON devices (dev);
CREATE INDEX IF NOT EXISTS devices_user ON devices (user);
CREATE TABLE IF NOT EXISTS device_endorse (endorse BLOB PRIMARY KEY, user INTEGER NOT NULL, new_ssh BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS device_revokes (revoke BLOB PRIMARY KEY, dev BLOB NOT NULL, user INTEGER NOT NULL,
	seq INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS device_revokes_dev ON device_revokes (dev);
-- Epoch bundles (msgpack Signed) not yet verified into the trust store, with
-- the seq of their key.epoch event; see e2e.go.
CREATE TABLE IF NOT EXISTS epochs (
	chan INTEGER NOT NULL, epoch INTEGER NOT NULL, hash BLOB NOT NULL, bundle BLOB NOT NULL,
	seq INTEGER NOT NULL, PRIMARY KEY (chan, epoch, hash));
-- key.share events (K sealed to this device), until the worker handles
-- them: one per (chan, epoch, dev, sharer), the newest (by event seq) wins.
CREATE TABLE IF NOT EXISTS key_shares (
	chan INTEGER NOT NULL, epoch INTEGER NOT NULL, dev BLOB NOT NULL, sharer INTEGER NOT NULL,
	seq INTEGER NOT NULL, eph BLOB NOT NULL, ct BLOB NOT NULL,
	PRIMARY KEY (chan, epoch, dev, sharer));
-- Local-only state (never sent to the server, kept across snapshot resets).
CREATE TABLE IF NOT EXISTS drafts (
	chan INTEGER NOT NULL, thread INTEGER NOT NULL, body TEXT NOT NULL, time INTEGER NOT NULL,
	PRIMARY KEY (chan, thread));
CREATE TABLE IF NOT EXISTS highlights (word TEXT PRIMARY KEY, time INTEGER NOT NULL);
-- Reactions other people added to my messages, from react.add events (the
-- message itself only keeps who reacted, not when). Feeds the activity view.
CREATE TABLE IF NOT EXISTS reacts (
	msg INTEGER NOT NULL, actor INTEGER NOT NULL, emoji TEXT NOT NULL, time INTEGER NOT NULL,
	PRIMARY KEY (msg, actor, emoji));
CREATE INDEX IF NOT EXISTS reacts_time ON reacts (time);
` + pinsSchema + groupsSchema

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
	// secure_delete: deleted rows (a discarded or sent outbox entry's
	// plaintext, a message deleted for everyone) are zeroed, not left in
	// free pages.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=secure_delete(ON)&_txlock=immediate"
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
	if err := migrateE2E(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// connQ adapts a dedicated connection to dbtx.
type connQ struct{ c *sql.Conn }

func (q connQ) Exec(s string, a ...any) (sql.Result, error) {
	return q.c.ExecContext(context.Background(), s, a...)
}
func (q connQ) Query(s string, a ...any) (*sql.Rows, error) {
	return q.c.QueryContext(context.Background(), s, a...)
}
func (q connQ) QueryRow(s string, a ...any) *sql.Row {
	return q.c.QueryRowContext(context.Background(), s, a...)
}

// readTx runs f on a deferred (read-only, non-locking) transaction. The
// database/sql Begin is IMMEDIATE here, which would take the write lock.
func (c *Client) readTx(f func(dbtx) error) error {
	conn, err := c.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN DEFERRED`); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)
	return f(connQ{conn})
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

// resetCache drops everything the server can resend; the outbox, unacknowledged
// read markers and local-only state (drafts) survive.
func resetCache(q dbtx, serverID []byte) error {
	// Local-only state survives a reset of the same server; another server's
	// ids mean nothing here.
	old, err := metaBytes(q, "server_id")
	if err != nil {
		return err
	}
	if len(old) > 0 && !bytes.Equal(old, serverID) {
		for _, t := range []string{"drafts", "reacts"} {
			if _, err := q.Exec(`DELETE FROM ` + t); err != nil {
				return err
			}
		}
		if _, err := q.Exec(`DELETE FROM meta WHERE k = 'activity_seen'`); err != nil {
			return err
		}
	}
	for _, s := range []string{
		`DELETE FROM users`, `DELETE FROM chans`, `DELETE FROM msgs`, `DELETE FROM rencs`, `DELETE FROM pins`, `DELETE FROM groups`, `DELETE FROM reads WHERE dirty = 0`,
		`DELETE FROM devices`, `DELETE FROM device_endorse`, `DELETE FROM device_revokes`,
		`DELETE FROM meta WHERE k = 'devices_full'`, `DELETE FROM epochs`, `DELETE FROM key_shares`, `DELETE FROM removed`,
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

// upsertChan stores a channel. The epoch never moves back here (key.epoch
// events raise it; a channel object from an older event mustn't lower it);
// a reset snapshot starts from scratch, so the server's claim is kept as is.
func upsertChan(q dbtx, c proto.Channel) error {
	members, _ := json.Marshal(c.Members)
	_, err := q.Exec(`INSERT INTO chans (id, name, kind, topic, members, created, e2e, epoch, creator, legacy)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, kind = excluded.kind, topic = excluded.topic,
			members = excluded.members, created = excluded.created, e2e = excluded.e2e,
			epoch = max(epoch, excluded.epoch), creator = excluded.creator, legacy = excluded.legacy`,
		c.ID, c.Name, c.Kind, c.Topic, string(members), c.Created, c.E2E, c.Epoch, c.Creator, min(c.Legacy, math.MaxInt64)) // SQLite ints are signed
	return err
}

const chanCols = `id, name, kind, topic, members, created, e2e, epoch, creator, legacy`

func scanChan(sc interface{ Scan(...any) error }) (proto.Channel, error) {
	var c proto.Channel
	var members string
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &c.Topic, &members, &c.Created, &c.E2E, &c.Epoch, &c.Creator, &c.Legacy); err != nil {
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

// upsertMsg stores a message of a plaintext channel as is (tests; server
// messages go through Client.saveMsg, which judges encrypted ones).
func upsertMsg(q dbtx, m proto.Message, replace bool) error {
	return storeMsg(q, m, SecPlain, nil, replace)
}

// storeMsg stores m with its verdict; m.Body is the text to show and m.Enc
// the ciphertext, if any (kept so a waiting message can be opened later).
func storeMsg(q dbtx, m proto.Message, sec Sec, mid []byte, replace bool) error {
	verb := "INSERT OR REPLACE"
	if !replace {
		verb = "INSERT OR IGNORE"
	}
	_, err := q.Exec(verb+` INTO msgs (`+msgCols+`, enc, sec, mid) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Chan, m.Author, m.Body, m.Thread, m.Time, m.Edited, m.Deleted, encodeReactions(m.Reactions), m.Enc, sec, mid)
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
	// enc is what we send into an encrypted channel (nil until encrypted):
	// every retry sends these exact bytes; only a stale_epoch replaces it,
	// re-encrypting with the same encNonce (encmsg.go).
	enc      []byte
	encNonce []byte
	sent     uint64 // 0: never handed to the server
}

const outboxCols = `id, nonce, chan, thread, body, time, failed, msg, enc, encnonce, sent`

func scanOutbox(sc interface{ Scan(...any) error }) (outboxEntry, error) {
	var e outboxEntry
	err := sc.Scan(&e.id, &e.nonce, &e.chan_, &e.thread, &e.body, &e.time, &e.failed, &e.msg, &e.enc, &e.encNonce, &e.sent)
	return e, err
}

// matchOutbox hides the pending copy of a message of ours that just arrived
// (live, replayed or in a snapshot), so the real and pending copies never
// show together.
//
// The wire protocol doesn't echo the send nonce in msg.new, and the EVENT
// arrives before the RES that names the message id, so the match can't be
// exact. It doesn't use the body (identical texts, future E2E ciphertext);
// it relies on the outbox loop sending strictly one entry at a time, oldest
// first: the only entry that can be on the server unacknowledged is the
// oldest unfailed one, and only once it has been sent. sent holds last_seq+1
// from its first send, and a message created by that send has a larger id,
// so older messages of ours (snapshots, history) can't match. A wrong match
// (another device of ours posting in the same channel in that window) only
// hides the entry until its RES, which deletes it on success or unhides it
// on failure; the RES's msg id is the authoritative mapping.
func matchOutbox(q dbtx, m proto.Message) error {
	_, err := q.Exec(`UPDATE outbox SET msg = ?1
		WHERE id = (SELECT id FROM outbox WHERE failed = '' ORDER BY id LIMIT 1)
		AND msg = 0 AND sent > 0 AND sent <= ?1 AND chan = ?2 AND thread = ?3`, m.ID, m.Chan, m.Thread)
	return err
}

// wipe empties the whole cache, outbox included (new login).
func wipe(q dbtx) error {
	for _, t := range []string{"meta", "users", "chans", "msgs", "rencs", "reads", "outbox", "devices", "device_endorse", "device_revokes", "drafts", "highlights", "reacts", "epochs", "key_shares", "pins", "groups"} {
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
