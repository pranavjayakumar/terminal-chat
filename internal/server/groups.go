package server

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"github.com/skailama/tc/internal/proto"
)

const groupsSchema = `
CREATE TABLE IF NOT EXISTS groups (
	name TEXT PRIMARY KEY,
	members BLOB NOT NULL
);
`

func listGroups(ctx context.Context, q querier) ([]proto.Group, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, members FROM groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.Group{}
	for rows.Next() {
		var g proto.Group
		var raw []byte
		if err := rows.Scan(&g.Name, &raw); err != nil {
			return nil, err
		}
		if err := proto.Unmarshal(raw, &g.Members); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Server) opGroupsList(ctx context.Context) (any, error) {
	gs, err := listGroups(ctx, s.st.db)
	return proto.GroupsRes{Groups: gs}, err
}

// opGroupSet implements group.set (admin only). Setting the members a group
// already has, or deleting one that doesn't exist, is a no-op (seq 0).
func (s *Server) opGroupSet(ctx context.Context, u proto.User, a proto.GroupSetArgs) (any, error) {
	if !u.Admin {
		return nil, errForbidden
	}
	if !proto.ValidName(a.Name) || strings.HasSuffix(a.Name, proto.BotSuffix) {
		return nil, badRequest("group name must match ^[a-z0-9][a-z0-9_-]{0,31}$ and not end in -bot")
	}
	if len(a.Members) > proto.MaxGroupMembers {
		return nil, badRequest("a group has at most 50 members")
	}
	members := slices.Clone(a.Members)
	slices.Sort(members)
	members = slices.Compact(members)
	var res proto.SeqRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE name = ?`, a.Name).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			return nil, proto.Errf(proto.ErrConflict, "a user is named "+a.Name)
		}
		var old []byte
		err := tx.QueryRowContext(ctx, `SELECT members FROM groups WHERE name = ?`, a.Name).Scan(&old)
		exists := err == nil
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if len(members) == 0 {
			if !exists {
				return nil, nil
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM groups WHERE name = ?`, a.Name); err != nil {
				return nil, err
			}
			return s.groupEvent(ctx, tx, u, a.Name, members, &res)
		}
		for _, id := range members {
			usr, err := getUser(ctx, tx, id)
			if err != nil || usr.Disabled {
				return nil, errNotFound
			}
			if usr.Bot {
				return nil, badRequest("bots can't be group members")
			}
		}
		raw, err := proto.Marshal(members)
		if err != nil {
			return nil, err
		}
		if exists && string(raw) == string(old) {
			return nil, nil
		}
		if !exists {
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM groups`).Scan(&n); err != nil {
				return nil, err
			}
			if n >= proto.MaxGroups {
				return nil, proto.Errf(proto.ErrConflict, "too many groups")
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO groups (name, members) VALUES (?, ?)
			ON CONFLICT (name) DO UPDATE SET members = excluded.members`, a.Name, raw); err != nil {
			return nil, err
		}
		return s.groupEvent(ctx, tx, u, a.Name, members, &res)
	})
	return res, err
}

func (s *Server) groupEvent(ctx context.Context, tx *sql.Tx, u proto.User, name string, members []uint64, res *proto.SeqRes) ([]proto.Event, error) {
	if members == nil {
		members = []uint64{}
	}
	ev, err := appendEvent(ctx, tx, proto.KGroupUpdate, 0, u.ID, 0, s.now(), proto.Group{Name: name, Members: members})
	if err != nil {
		return nil, err
	}
	res.Seq = ev.Seq
	return []proto.Event{ev}, nil
}

// groupNameTaken reports whether name is a group, so a new user can't take
// a name that @name already means.
func groupNameTaken(ctx context.Context, q querier, name string) bool {
	var n int
	q.QueryRowContext(ctx, `SELECT COUNT(*) FROM groups WHERE name = ?`, name).Scan(&n)
	return n > 0
}
