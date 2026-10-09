package server

import (
	"context"
	"database/sql"

	"github.com/skailama/tc/internal/proto"
)

const pinsSchema = `
CREATE TABLE IF NOT EXISTS pins (
	chan INTEGER NOT NULL,
	msg INTEGER NOT NULL PRIMARY KEY,
	by INTEGER NOT NULL,
	at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS pins_chan ON pins(chan, at);
`

// channelPins lists a channel's pinned message ids, newest pin first.
func channelPins(ctx context.Context, q querier, ch uint64) ([]uint64, error) {
	rows, err := q.QueryContext(ctx, `SELECT msg FROM pins WHERE chan = ? ORDER BY rowid DESC`, ch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// opPin implements pin/unpin with set semantics: a no-op returns seq 0.
func (s *Server) opPin(ctx context.Context, u proto.User, a proto.MsgArgs, add bool) (any, error) {
	var res proto.SeqRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		m, err := getMessage(ctx, tx, a.Msg)
		if err != nil || m.Deleted || !s.members[m.Chan][u.ID] {
			return nil, errNotFound
		}
		kind := proto.KPinAdd
		now := s.now()
		if add {
			var have, n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pins WHERE msg = ?`, m.ID).Scan(&have); err != nil {
				return nil, err
			}
			if have > 0 {
				return nil, nil
			}
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pins WHERE chan = ?`, m.Chan).Scan(&n); err != nil {
				return nil, err
			}
			if n >= proto.MaxPins {
				return nil, proto.Errf(proto.ErrConflict, "too many pinned messages")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO pins (chan, msg, by, at) VALUES (?, ?, ?, ?)`, m.Chan, m.ID, u.ID, now); err != nil {
				return nil, err
			}
		} else {
			kind = proto.KPinRemove
			r, err := tx.ExecContext(ctx, `DELETE FROM pins WHERE msg = ?`, m.ID)
			if err != nil {
				return nil, err
			}
			if c, _ := r.RowsAffected(); c == 0 {
				return nil, nil
			}
		}
		ev, err := appendEvent(ctx, tx, kind, m.Chan, u.ID, 0, now, proto.MsgRef{Msg: m.ID})
		if err != nil {
			return nil, err
		}
		res.Seq = ev.Seq
		return []proto.Event{ev}, nil
	})
	return res, err
}

// unpinInTx drops m's pin, if any, inside the caller's transaction so a
// deleted message never stays pinned.
func (s *Server) unpinInTx(ctx context.Context, tx *sql.Tx, m proto.Message, actor uint64) ([]proto.Event, error) {
	r, err := tx.ExecContext(ctx, `DELETE FROM pins WHERE msg = ?`, m.ID)
	if err != nil {
		return nil, err
	}
	if c, _ := r.RowsAffected(); c == 0 {
		return nil, nil
	}
	ev, err := appendEvent(ctx, tx, proto.KPinRemove, m.Chan, actor, 0, s.now(), proto.MsgRef{Msg: m.ID})
	if err != nil {
		return nil, err
	}
	return []proto.Event{ev}, nil
}

func (s *Server) opPinsList(ctx context.Context, u proto.User, a proto.ChanArgs) (any, error) {
	if _, ok := s.canRead(a.Chan, u.ID); !ok {
		return nil, errForbidden
	}
	msgs, err := scanMessages(ctx, s.st.db, `SELECT `+prefixCols("m.", msgCols)+` FROM pins p JOIN messages m ON m.id = p.msg
		WHERE p.chan = ? AND m.deleted = 0 ORDER BY p.rowid DESC LIMIT ?`, a.Chan, proto.MaxPins)
	if err != nil {
		return nil, err
	}
	var more bool
	msgs = trimBytes(msgs, &more)
	if msgs == nil {
		msgs = []proto.Message{}
	}
	return proto.MsgsRes{Msgs: msgs}, nil
}
