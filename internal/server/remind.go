package server

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// Reminders are rows in the reminders table, delivered by one scheduler
// goroutine that Serve owns. A row is deleted in the same transaction that
// posts its message, so a reminder is delivered exactly once however often
// the server restarts, and its text doesn't linger after delivery.
const remindSchema = `
CREATE TABLE IF NOT EXISTS reminders (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user INTEGER NOT NULL,
	at INTEGER NOT NULL,
	text TEXT NOT NULL,
	chan INTEGER NOT NULL DEFAULT 0,
	created INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS reminders_at ON reminders(at);
CREATE INDEX IF NOT EXISTS reminders_user ON reminders(user);
`

const (
	// remindMaxWait bounds one sleep so a clock step or a suspended machine
	// (monotonic timers pause in sleep) is noticed without polling.
	remindMaxWait = time.Minute
	// remindBatch is how many due reminders one pass delivers.
	remindBatch = 100
	// remindRetry is the pause after a failed delivery.
	remindRetry = 5 * time.Second
)

func (s *Server) opRemindCreate(ctx context.Context, u proto.User, a proto.RemindCreateArgs) (any, error) {
	if len(a.Text) > proto.MaxRemindText {
		return nil, proto.Errf(proto.ErrTooLarge, "reminder text is over 4 KiB")
	}
	if err := validBody(a.Text); err != nil {
		return nil, err
	}
	now := s.now()
	if a.At <= 0 || a.At > now+proto.MaxRemindAhead.Milliseconds() {
		return nil, badRequest("reminder time must be within a year")
	}
	if a.Chan != 0 {
		if _, ok := s.canRead(a.Chan, u.ID); !ok {
			return nil, errForbidden
		}
	}
	var res proto.RemindCreateRes
	err := s.txLocked(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reminders WHERE user = ?`, u.ID).Scan(&n); err != nil {
			return err
		}
		if n >= proto.MaxReminders {
			return proto.Errf(proto.ErrConflict, "too many pending reminders")
		}
		r, err := tx.ExecContext(ctx, `INSERT INTO reminders (user, at, text, chan, created) VALUES (?, ?, ?, ?, ?)`,
			u.ID, a.At, a.Text, a.Chan, now)
		if err != nil {
			return err
		}
		id, _ := r.LastInsertId()
		res.ID = uint64(id)
		return nil
	})
	if err == nil {
		s.wakeReminders()
	}
	return res, err
}

func (s *Server) opRemindList(ctx context.Context, u proto.User) (any, error) {
	rows, err := s.st.db.QueryContext(ctx, `SELECT id, at, text, chan FROM reminders WHERE user = ? ORDER BY at, id`, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := proto.RemindListRes{Reminders: []proto.Reminder{}}
	for rows.Next() {
		var r proto.Reminder
		if err := rows.Scan(&r.ID, &r.At, &r.Text, &r.Chan); err != nil {
			return nil, err
		}
		out.Reminders = append(out.Reminders, r)
	}
	return out, rows.Err()
}

func (s *Server) opRemindDelete(ctx context.Context, u proto.User, a proto.RemindDeleteArgs) (any, error) {
	err := s.txLocked(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE id = ? AND user = ?`, a.ID, u.ID)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return errNotFound
		}
		return nil
	})
	return proto.Empty{}, err
}

// txLocked runs fn in a transaction under the write lock, for changes that
// produce no events (a reminder row isn't part of the event log).
func (s *Server) txLocked(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// wakeReminders makes the scheduler recompute when it is next due.
func (s *Server) wakeReminders() {
	select {
	case s.remindWake <- struct{}{}:
	default:
	}
}

// remindLoop delivers reminders when they fall due, until ctx ends. It sleeps
// until the earliest pending reminder (or a wake-up), not on a fixed tick;
// reminders already overdue when it starts (the server was down) go out at
// once.
func (s *Server) remindLoop(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for ctx.Err() == nil {
		wait, err := s.remindWait(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Error("reminders", "err", err)
			wait = remindRetry
		}
		if wait == 0 {
			continue // something was delivered; look again
		}
		var fire <-chan time.Time
		if wait > 0 {
			timer.Reset(min(wait, remindMaxWait))
			fire = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case <-s.remindWake:
		case <-fire:
		}
		if fire != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

// remindWait delivers whatever is due and returns how long to sleep: 0 if it
// delivered something (so ask again), -1 if nothing is pending.
func (s *Server) remindWait(ctx context.Context) (time.Duration, error) {
	var at sql.NullInt64
	if err := s.st.db.QueryRowContext(ctx, `SELECT MIN(at) FROM reminders`).Scan(&at); err != nil {
		return 0, err
	}
	if !at.Valid {
		return -1, nil
	}
	if d := time.Duration(at.Int64-s.now()) * time.Millisecond; d > 0 {
		return d, nil
	}
	return 0, s.deliverDue(ctx)
}

// deliverDue delivers up to remindBatch reminders that are due.
func (s *Server) deliverDue(ctx context.Context) error {
	rows, err := s.st.db.QueryContext(ctx, `SELECT id FROM reminders WHERE at <= ? ORDER BY at, id LIMIT ?`, s.now(), remindBatch)
	if err != nil {
		return err
	}
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.deliverReminder(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// deliverReminder posts reminder id into its user's private notes and
// deletes it, in one transaction.
func (s *Server) deliverReminder(ctx context.Context, id uint64) error {
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		var user, ch uint64
		var text string
		err := tx.QueryRowContext(ctx, `DELETE FROM reminders WHERE id = ? RETURNING user, text, chan`, id).Scan(&user, &text, &ch)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // deleted or delivered meanwhile
		}
		if err != nil {
			return nil, err
		}
		if s.disabled[user] {
			return nil, nil
		}
		now := s.now()
		var evs []proto.Event
		notes, err := notesChannel(ctx, tx, now, user, &evs)
		if err != nil {
			return nil, err
		}
		body := proto.RemindPrefix + text
		if from := s.remindOrigin(ctx, tx, ch, user); from != "" {
			body += " (from " + from + ")"
		}
		// A fixed nonce per reminder makes a second delivery impossible even
		// if the row somehow came back.
		nonce := make([]byte, 16)
		copy(nonce, "remind")
		binary.BigEndian.PutUint64(nonce[8:], id)
		var dup int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE author = ? AND nonce = ?`, user, nonce).Scan(&dup); err != nil {
			return nil, err
		}
		if dup > 0 {
			return evs, nil
		}
		m := proto.Message{Chan: notes, Author: user, Body: body, Time: now}
		ev, err := appendEvent(ctx, tx, proto.KMsgNew, notes, user, 0, now, m)
		if err != nil {
			return nil, err
		}
		m.ID = ev.Seq
		if err := patchEventData(ctx, tx, &ev, m); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, chan, author, body, thread, time, nonce) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.Chan, m.Author, m.Body, m.Thread, m.Time, nonce); err != nil {
			return nil, err
		}
		return append(evs, ev), nil
	})
	return err
}

// remindOrigin names the channel a reminder was set in, "" if it has none or
// the user can no longer read it. Called inside s.write.
func (s *Server) remindOrigin(ctx context.Context, tx *sql.Tx, ch, user uint64) string {
	if ch == 0 {
		return ""
	}
	kind, ok := s.kinds[ch]
	if !ok || !(kind == proto.ChanPublic || s.members[ch][user]) {
		return ""
	}
	if kind == proto.ChanDM {
		return "a DM"
	}
	var name string
	if tx.QueryRowContext(ctx, `SELECT name FROM channels WHERE id = ?`, ch).Scan(&name) != nil {
		return ""
	}
	return "#" + name
}

// notesChannel returns user's private notes DM, creating it (not encrypted:
// the server writes into it) if the user has never opened it.
func notesChannel(ctx context.Context, tx *sql.Tx, now int64, user uint64, evs *[]proto.Event) (uint64, error) {
	var id uint64
	err := tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE dmkey = ?`, dmKey([]uint64{user})).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	c, ev, err := createChannel(ctx, tx, now, user, "", proto.ChanDM, "", []uint64{user}, false)
	if err != nil {
		return 0, err
	}
	*evs = append(*evs, ev)
	return c.ID, nil
}
