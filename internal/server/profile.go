package server

import (
	"context"
	"database/sql"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

func (s *Server) opUserUpdate(ctx context.Context, u proto.User, a proto.UserUpdateArgs) (any, error) {
	if !validLine(a.Display, proto.MaxDisplay) {
		return nil, badRequest("display name must be one line of at most 64 bytes")
	}
	// Names tell people apart: nothing invisible, stacked or padded that
	// could make one look like another (PROTOCOL.md §10).
	if !sanitize.ValidName(a.Display) {
		return nil, badRequest("display name has invisible or stacked characters, or leading or trailing spaces")
	}
	var res proto.SeqRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		cur, err := getUser(ctx, tx, u.ID)
		if err != nil {
			return nil, err
		}
		if cur.Display == a.Display {
			return nil, nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET display = ? WHERE id = ?`, a.Display, u.ID); err != nil {
			return nil, err
		}
		cur.Display = a.Display
		ev, err := appendEvent(ctx, tx, proto.KUserUpdate, 0, u.ID, u.ID, s.now(), cur)
		res.Seq = ev.Seq
		return []proto.Event{ev}, err
	})
	return res, err
}
