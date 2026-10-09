package client

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/skailama/tc/internal/proto"
)

// migrateBots adds users.bot to caches created before webhooks existed.
func migrateBots(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'bot'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE users ADD COLUMN bot INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrating cache: %w", err)
		}
	}
	return nil
}

// CreateHook creates an incoming webhook posting into ch as bot user name.
// url is secret; curl is a ready-to-run command for it.
func (c *Client) CreateHook(ctx context.Context, ch uint64, name string) (url, curl string, err error) {
	var r proto.HookCreateRes
	err = c.call(ctx, proto.OpHookCreate, proto.HookCreateArgs{Chan: ch, Name: name}, &r, nil)
	return r.URL, r.Curl, err
}

// ListHooks lists webhooks in channels we belong to (all, for admins).
func (c *Client) ListHooks(ctx context.Context) ([]proto.Hook, error) {
	var r proto.HooksRes
	err := c.call(ctx, proto.OpHookList, proto.Empty{}, &r, nil)
	return r.Hooks, err
}

// DeleteHook deletes a webhook (ours, or any as admin).
func (c *Client) DeleteHook(ctx context.Context, id uint64) error {
	return c.call(ctx, proto.OpHookDelete, proto.HookArgs{ID: id}, nil, nil)
}
