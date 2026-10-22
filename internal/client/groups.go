package client

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/skailama/tc/internal/proto"
)

// User groups (PROTOCOL.md §5, §6): @name mentions every member. Which
// messages count as mentions is decided here, from the cached groups, not by
// the server: a message containing @group (same whole-word rule as @user) is
// a mention for each member, in ChannelView.Mention, Activity and the TUI's
// notifications. Bots never trigger one.

const groupsSchema = `
CREATE TABLE IF NOT EXISTS groups (name TEXT PRIMARY KEY, members TEXT NOT NULL);
`

func storeGroup(q dbtx, g proto.Group) error {
	if len(g.Members) == 0 {
		_, err := q.Exec(`DELETE FROM groups WHERE name = ?`, g.Name)
		return err
	}
	raw, err := json.Marshal(g.Members)
	if err != nil {
		return err
	}
	_, err = q.Exec(`INSERT OR REPLACE INTO groups (name, members) VALUES (?, ?)`, g.Name, string(raw))
	return err
}

func loadGroups(q dbtx) []proto.Group {
	rows, err := q.Query(`SELECT name, members FROM groups ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []proto.Group
	for rows.Next() {
		var g proto.Group
		var raw string
		if rows.Scan(&g.Name, &raw) == nil && json.Unmarshal([]byte(raw), &g.Members) == nil {
			out = append(out, g)
		}
	}
	return out
}

// Groups lists the user groups, by name, from the local cache.
func (c *Client) Groups() []proto.Group { return loadGroups(c.db) }

// MentionNames are the names that @mention me: my own, then each group I'm
// a member of. Lowercase names are compared case-insensitively.
func (c *Client) MentionNames() []string { return mentionNames(c.db, c.Me()) }

func mentionNames(q dbtx, me proto.User) []string {
	return MentionNamesOf(me, loadGroups(q))
}

// MentionNamesOf is the names that @mention me given the groups: my own
// name, then each group containing me.
func MentionNamesOf(me proto.User, groups []proto.Group) []string {
	var out []string
	if me.Name != "" {
		out = append(out, me.Name)
	}
	for _, g := range groups {
		if slices.Contains(g.Members, me.ID) {
			out = append(out, g.Name)
		}
	}
	return out
}

// Mentioned reports whether body contains @name as a whole word
// (case-insensitive, not inside an email address or a longer name) for any
// of names.
func Mentioned(body string, names []string) bool {
	for _, n := range names {
		if mentions(body, n) {
			return true
		}
	}
	return false
}

// SetGroup (admin) sets a group's members; no members deletes it. The name
// must be a valid user/channel name that no user has.
func (c *Client) SetGroup(ctx context.Context, name string, members []uint64) error {
	name = strings.ToLower(strings.TrimPrefix(name, "@"))
	if !proto.ValidName(name) {
		return errors.New("group names are 1-32 of a-z 0-9 _ - (lowercase)")
	}
	if name == proto.NameMe && len(members) > 0 {
		return errors.New(`"me" is reserved: @me means your own notes`)
	}
	if len(members) > proto.MaxGroupMembers {
		return errors.New("a group has at most 50 members")
	}
	if err := c.requireCap(proto.CapGroups, "user groups"); err != nil {
		return err
	}
	// The group.update event updates the cache; nothing to do on the RES.
	return c.call(ctx, proto.OpGroupSet, proto.GroupSetArgs{Name: name, Members: members}, nil, nil)
}

// FetchGroups asks the server for the groups and replaces the cached ones,
// which repairs any drift. The cache is normally current via events.
func (c *Client) FetchGroups(ctx context.Context) ([]proto.Group, error) {
	if err := c.requireCap(proto.CapGroups, "user groups"); err != nil {
		return nil, err
	}
	var r proto.GroupsRes
	if err := c.call(ctx, proto.OpGroupsList, proto.Empty{}, &r, nil); err != nil {
		return nil, err
	}
	err := c.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM groups`); err != nil {
			return err
		}
		for _, g := range r.Groups {
			if err := storeGroup(tx, g); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.post(Update{Channels: true})
	return r.Groups, nil
}

// requireCap fails when we're offline or the server lacks cap (an older
// server would answer unknown_op).
func (c *Client) requireCap(cp, what string) error {
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return ErrOffline
	}
	if !slices.Contains(s.welcome.Caps, cp) {
		return errors.New("this server doesn't support " + what)
	}
	return nil
}
