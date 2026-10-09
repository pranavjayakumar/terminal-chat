package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/skailama/tc/internal/proto"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	display TEXT NOT NULL DEFAULT '',
	admin INTEGER NOT NULL DEFAULT 0,
	disabled INTEGER NOT NULL DEFAULT 0,
	created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS keys (
	fp TEXT PRIMARY KEY,
	user INTEGER NOT NULL REFERENCES users(id),
	pubkey BLOB NOT NULL,
	created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS invites (
	token TEXT PRIMARY KEY,
	admin INTEGER NOT NULL DEFAULT 0,
	user INTEGER,
	created INTEGER NOT NULL,
	expires INTEGER NOT NULL,
	used INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS channels (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	kind TEXT NOT NULL,
	topic TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL,
	dmkey TEXT UNIQUE
);
CREATE UNIQUE INDEX IF NOT EXISTS channels_name ON channels(name) WHERE kind != 'dm';
-- Membership intervals: visibility of an event depends on membership at its seq.
CREATE TABLE IF NOT EXISTS membership (
	chan INTEGER NOT NULL,
	user INTEGER NOT NULL,
	join_seq INTEGER NOT NULL,
	leave_seq INTEGER
);
CREATE INDEX IF NOT EXISTS membership_user ON membership(user, chan);
CREATE TABLE IF NOT EXISTS events (
	seq INTEGER PRIMARY KEY,
	kind TEXT NOT NULL,
	chan INTEGER,
	time INTEGER NOT NULL,
	actor INTEGER NOT NULL,
	about INTEGER,
	data BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
	id INTEGER PRIMARY KEY,
	chan INTEGER NOT NULL,
	author INTEGER NOT NULL,
	body TEXT NOT NULL,
	thread INTEGER NOT NULL DEFAULT 0,
	time INTEGER NOT NULL,
	edited INTEGER NOT NULL DEFAULT 0,
	deleted INTEGER NOT NULL DEFAULT 0,
	nonce BLOB NOT NULL,
	UNIQUE(author, nonce)
);
CREATE INDEX IF NOT EXISTS messages_chan ON messages(chan, id);
CREATE TABLE IF NOT EXISTS reactions (
	msg INTEGER NOT NULL,
	user INTEGER NOT NULL,
	emoji TEXT NOT NULL,
	PRIMARY KEY (msg, user, emoji)
);
CREATE TABLE IF NOT EXISTS reads (
	user INTEGER NOT NULL,
	chan INTEGER NOT NULL,
	seq INTEGER NOT NULL,
	PRIMARY KEY (user, chan)
);
CREATE VIRTUAL TABLE IF NOT EXISTS msg_fts USING fts5(body, content='messages', content_rowid='id');
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
	INSERT INTO msg_fts(rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE OF body ON messages BEGIN
	INSERT INTO msg_fts(msg_fts, rowid, body) VALUES ('delete', old.id, old.body);
	INSERT INTO msg_fts(rowid, body) VALUES (new.id, new.body);
END;
`

// store wraps the server's SQLite database. All writes happen inside
// Server.write, which serializes them; reads may run concurrently (WAL).
type store struct {
	db       *sql.DB
	serverID []byte
}

func openStore(path string) (*store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening db: %w", err)
	}
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	s := &store{db: db}
	err = db.QueryRow(`SELECT v FROM meta WHERE k = 'server_id'`).Scan(&s.serverID)
	if errors.Is(err, sql.ErrNoRows) {
		s.serverID = make([]byte, 16)
		rand.Read(s.serverID)
		_, err = db.Exec(`INSERT INTO meta (k, v) VALUES ('server_id', ?)`, s.serverID)
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("server id: %w", err)
	}
	return s, nil
}

func (s *store) close() error { return s.db.Close() }

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func head(ctx context.Context, q querier) (uint64, error) {
	var h uint64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&h)
	return h, err
}

// appendEvent writes an event and returns it with its new seq.
func appendEvent(ctx context.Context, tx *sql.Tx, kind string, ch, actor, about uint64, now int64, data any) (proto.Event, error) {
	raw, err := proto.Marshal(data)
	if err != nil {
		return proto.Event{}, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO events (kind, chan, time, actor, about, data) VALUES (?, ?, ?, ?, ?, ?)`,
		kind, nullable(ch), now, actor, nullable(about), raw)
	if err != nil {
		return proto.Event{}, fmt.Errorf("appending %s: %w", kind, err)
	}
	seq, _ := res.LastInsertId()
	return proto.Event{Seq: uint64(seq), Kind: kind, Chan: ch, Time: now, Actor: actor, Data: raw}, nil
}

// patchEventData rewrites an event's data after the fact; used when the data
// embeds the event's own seq (msg.new's Message.id).
func patchEventData(ctx context.Context, tx *sql.Tx, ev *proto.Event, data any) error {
	raw, err := proto.Marshal(data)
	if err != nil {
		return err
	}
	ev.Data = raw
	_, err = tx.ExecContext(ctx, `UPDATE events SET data = ? WHERE seq = ?`, raw, ev.Seq)
	return err
}

// scrubMessageEvents rewrites the event log after an edit or delete so a
// replay can't resurrect text the author replaced or removed: the message's
// msg.new event (seq == message id) gets its current state, and every
// msg.edit for it with seq < before keeps its place but loses its body.
// before == 0 means all of them (delete).
func scrubMessageEvents(ctx context.Context, tx *sql.Tx, m proto.Message, before uint64) error {
	m.Reactions = nil // replayed through react.* events
	if err := patchEventData(ctx, tx, &proto.Event{Seq: m.ID}, m); err != nil {
		return fmt.Errorf("scrubbing msg.new %d: %w", m.ID, err)
	}
	if before == 0 {
		before = math.MaxInt64
	}
	// msg.edit data isn't indexed by message, so scan the channel's edits
	// since the message was sent.
	rows, err := tx.QueryContext(ctx, `SELECT seq, data FROM events
		WHERE kind = ? AND chan = ? AND seq > ? AND seq < ?`, proto.KMsgEdit, m.Chan, m.ID, before)
	if err != nil {
		return err
	}
	var stale []proto.Event
	for rows.Next() {
		var seq uint64
		var data []byte
		if err := rows.Scan(&seq, &data); err != nil {
			rows.Close()
			return err
		}
		var d proto.MsgEdit
		if proto.Unmarshal(data, &d) == nil && d.Msg == m.ID && d.Body != "" {
			d.Body = ""
			raw, err := proto.Marshal(d)
			if err != nil {
				rows.Close()
				return err
			}
			stale = append(stale, proto.Event{Seq: seq, Data: raw})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, ev := range stale {
		if _, err := tx.ExecContext(ctx, `UPDATE events SET data = ? WHERE seq = ?`, []byte(ev.Data), ev.Seq); err != nil {
			return fmt.Errorf("scrubbing msg.edit %d: %w", ev.Seq, err)
		}
	}
	return nil
}

func nullable(v uint64) any {
	if v == 0 {
		return nil
	}
	return int64(v)
}

const userCols = `id, name, display, admin, disabled, created`

func scanUser(sc interface{ Scan(...any) error }) (proto.User, error) {
	var u proto.User
	err := sc.Scan(&u.ID, &u.Name, &u.Display, &u.Admin, &u.Disabled, &u.Created)
	return u, err
}

func getUser(ctx context.Context, q querier, id uint64) (proto.User, error) {
	return scanUser(q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func listUsers(ctx context.Context, q querier) ([]proto.User, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []proto.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func userByKey(ctx context.Context, q querier, fp string) (proto.User, error) {
	return scanUser(q.QueryRowContext(ctx,
		`SELECT `+prefixCols("u.", userCols)+` FROM users u JOIN keys k ON k.user = u.id WHERE k.fp = ?`, fp))
}

func prefixCols(p, cols string) string {
	parts := strings.Split(cols, ", ")
	for i := range parts {
		parts[i] = p + parts[i]
	}
	return strings.Join(parts, ", ")
}

func getChannel(ctx context.Context, q querier, id uint64) (proto.Channel, error) {
	var c proto.Channel
	err := q.QueryRowContext(ctx, `SELECT id, name, kind, topic, created FROM channels WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &c.Kind, &c.Topic, &c.Created)
	if err != nil {
		return c, err
	}
	c.Members, err = currentMembers(ctx, q, id)
	return c, err
}

func currentMembers(ctx context.Context, q querier, ch uint64) ([]uint64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT user FROM membership WHERE chan = ? AND leave_seq IS NULL ORDER BY user`, ch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uint64{}
	for rows.Next() {
		var u uint64
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// visibleChannels returns public channels and channels u currently belongs to.
func visibleChannels(ctx context.Context, q querier, u uint64) ([]proto.Channel, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM channels c WHERE kind = 'public'
		   OR EXISTS (SELECT 1 FROM membership m WHERE m.chan = c.id AND m.user = ? AND m.leave_seq IS NULL)
		ORDER BY id`, u)
	if err != nil {
		return nil, err
	}
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]proto.Channel, 0, len(ids))
	for _, id := range ids {
		c, err := getChannel(ctx, q, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

const msgCols = `id, chan, author, body, thread, time, edited, deleted`

func scanMessages(ctx context.Context, q querier, query string, args ...any) ([]proto.Message, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []proto.Message
	for rows.Next() {
		var m proto.Message
		if err := rows.Scan(&m.ID, &m.Chan, &m.Author, &m.Body, &m.Thread, &m.Time, &m.Edited, &m.Deleted); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := loadReactions(ctx, q, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func loadReactions(ctx context.Context, q querier, m *proto.Message) error {
	rows, err := q.QueryContext(ctx, `SELECT emoji, user FROM reactions WHERE msg = ? ORDER BY rowid`, m.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e string
		var u uint64
		if err := rows.Scan(&e, &u); err != nil {
			return err
		}
		if m.Reactions == nil {
			m.Reactions = map[string][]uint64{}
		}
		m.Reactions[e] = append(m.Reactions[e], u)
	}
	return rows.Err()
}

func getMessage(ctx context.Context, q querier, id uint64) (proto.Message, error) {
	msgs, err := scanMessages(ctx, q, `SELECT `+msgCols+` FROM messages WHERE id = ?`, id)
	if err != nil {
		return proto.Message{}, err
	}
	if len(msgs) == 0 {
		return proto.Message{}, sql.ErrNoRows
	}
	return msgs[0], nil
}

// isMember reports whether u is currently a member of ch.
func isMember(ctx context.Context, q querier, ch, u uint64) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM membership WHERE chan = ? AND user = ? AND leave_seq IS NULL`, ch, u).Scan(&n)
	return n > 0, err
}

// visibleEvents returns events in (since, h] that u may see (PROTOCOL.md §6 Visibility).
func visibleEvents(ctx context.Context, q querier, u, since, h uint64, limit int) ([]proto.Event, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT seq, kind, COALESCE(chan, 0), time, actor, data FROM events e
		WHERE seq > ?1 AND seq <= ?2 AND (
			kind IN ('user.new', 'user.update')
			OR (kind = 'read.update' AND actor = ?3)
			OR (kind IN ('chan.join', 'chan.leave') AND about = ?3)
			OR (kind IN ('chan.new', 'chan.update')
				AND EXISTS (SELECT 1 FROM channels c WHERE c.id = e.chan AND c.kind = 'public'))
			OR (kind != 'read.update' AND chan IS NOT NULL AND EXISTS (
				SELECT 1 FROM membership m WHERE m.chan = e.chan AND m.user = ?3
				AND m.join_seq <= e.seq AND (m.leave_seq IS NULL OR e.seq < m.leave_seq)))
		)
		ORDER BY seq LIMIT ?4`, since, h, u, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []proto.Event
	for rows.Next() {
		var e proto.Event
		var data []byte
		if err := rows.Scan(&e.Seq, &e.Kind, &e.Chan, &e.Time, &e.Actor, &data); err != nil {
			return nil, err
		}
		e.Data = data
		out = append(out, e)
	}
	return out, rows.Err()
}

func readMarkers(ctx context.Context, q querier, u uint64) (map[uint64]uint64, error) {
	rows, err := q.QueryContext(ctx, `SELECT chan, seq FROM reads WHERE user = ?`, u)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint64]uint64{}
	for rows.Next() {
		var c, s uint64
		if err := rows.Scan(&c, &s); err != nil {
			return nil, err
		}
		out[c] = s
	}
	return out, rows.Err()
}

// ftsQuery turns free text into a safe FTS5 query: each whitespace-separated
// term becomes a quoted phrase prefix, so user input can't use FTS syntax.
func ftsQuery(q string) string {
	var terms []string
	for i, t := range strings.Fields(q) {
		if i == maxSearchTerms {
			break
		}
		terms = append(terms, `"`+strings.ReplaceAll(t, `"`, `""`)+`"*`)
	}
	return strings.Join(terms, " ")
}
