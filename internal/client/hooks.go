package client

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

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

// migrateE2E adds the channel e2e fields and revoke seqs to older caches.
// Cached revokes from before seqs were stored are dropped, with the
// "full device list" mark, so the next e2e1 session lists them again with
// their seqs (an unknown seq would invalidate every bundle of the device).
func migrateE2E(db *sql.DB) error {
	// key_shares gained sharer/seq and a new key: rebuild it (shares are
	// recoverable through key.need).
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('key_shares') WHERE name = 'sharer'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`DROP TABLE key_shares; CREATE TABLE key_shares (
			chan INTEGER NOT NULL, epoch INTEGER NOT NULL, dev BLOB NOT NULL, sharer INTEGER NOT NULL,
			seq INTEGER NOT NULL, eph BLOB NOT NULL, ct BLOB NOT NULL,
			PRIMARY KEY (chan, epoch, dev, sharer))`); err != nil {
			return fmt.Errorf("migrating cache: %w", err)
		}
	}
	for _, c := range [][3]string{
		{"chans", "e2e", "INTEGER NOT NULL DEFAULT 0"},
		{"chans", "epoch", "INTEGER NOT NULL DEFAULT 0"},
		{"chans", "creator", "INTEGER NOT NULL DEFAULT 0"},
		{"chans", "legacy", "INTEGER NOT NULL DEFAULT 0"},
		{"device_revokes", "seq", "INTEGER NOT NULL DEFAULT 0"},
		{"msgs", "enc", "BLOB"},
		{"msgs", "sec", "INTEGER NOT NULL DEFAULT 0"},
		{"msgs", "mid", "BLOB"},
		{"msgs", "eenc", "BLOB"},
		{"msgs", "epend", "BLOB"},
		{"msgs", "ev", "INTEGER NOT NULL DEFAULT 0"},
		{"msgs", "ets", "INTEGER NOT NULL DEFAULT 0"},
		{"msgs", "denc", "BLOB"},
		{"msgs", "cmid", "BLOB"},
		{"msgs", "sts", "INTEGER NOT NULL DEFAULT 0"},
		{"msgs", "agent", "TEXT NOT NULL DEFAULT ''"},
		{"outbox", "enc", "BLOB"},
		{"outbox", "encnonce", "BLOB"},
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c[0], c[1]).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE ` + c[0] + ` ADD COLUMN ` + c[1] + ` ` + c[2]); err != nil {
			return fmt.Errorf("migrating cache: %w", err)
		}
		if c[0] == "device_revokes" {
			if _, err := db.Exec(`DELETE FROM device_revokes; DELETE FROM meta WHERE k = 'devices_full'`); err != nil {
				return fmt.Errorf("migrating cache: %w", err)
			}
		}
		if c[0] == "msgs" && c[1] == "denc" {
			// Step-7 caches deleted encrypted messages without knowing who
			// deleted them: none of those deletes can be shown as the author's.
			if _, err := db.Exec(`UPDATE msgs SET sec = ? WHERE deleted = 1 AND mid IS NOT NULL`, SecRemoved); err != nil {
				return fmt.Errorf("migrating cache: %w", err)
			}
		}
	}
	// Replays are found by mid (encmsg.go).
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS msgs_mid ON msgs (mid) WHERE mid IS NOT NULL`); err != nil {
		return fmt.Errorf("migrating cache: %w", err)
	}
	return nil
}

// NewHook is a created webhook. Curl is a ready-to-paste command that the
// client built from the validated URL and pin; it reads the URL from
// $TC_HOOK_URL so the secret stays out of shell history and ps.
type NewHook struct {
	ID   uint64
	Name string // the bot user's name
	URL  string // secret
	Pin  string // curl --pinnedpubkey form
	Curl string
}

var (
	hookURLRE = regexp.MustCompile(`^https://[A-Za-z0-9.:\[\]-]+/hooks/[a-z2-7]+$`)
	hookPinRE = regexp.MustCompile(`^sha256//[A-Za-z0-9+/]+={0,2}$`)
)

// hookCurl builds the curl command for a hook. url and pin come from the
// server, so they are checked to be exactly what they claim: the command
// ends up pasted into a shell.
func hookCurl(url, pin string) (string, error) {
	if !hookURLRE.MatchString(url) {
		return "", fmt.Errorf("server returned a malformed webhook URL")
	}
	b64, _ := strings.CutPrefix(pin, "sha256//")
	if raw, err := base64.StdEncoding.DecodeString(b64); !hookPinRE.MatchString(pin) || err != nil || len(raw) != sha256.Size {
		return "", fmt.Errorf("server returned a malformed webhook pin")
	}
	return "# export TC_HOOK_URL='" + url + "'\n" +
		`curl -fsS -k --pinnedpubkey '` + pin + `' -H 'Content-Type: application/json' -d '{"text":"hello from a webhook"}' "$TC_HOOK_URL"` + "\n", nil
}

// CreateHook creates an incoming webhook posting into ch as a bot user
// named after name (the server appends "-bot").
func (c *Client) CreateHook(ctx context.Context, ch uint64, name string) (NewHook, error) {
	var r proto.HookCreateRes
	if err := c.call(ctx, proto.OpHookCreate, proto.HookCreateArgs{Chan: ch, Name: name}, &r, nil); err != nil {
		return NewHook{}, err
	}
	if !proto.ValidName(r.Name) {
		return NewHook{}, fmt.Errorf("server returned a malformed bot name")
	}
	curl, err := hookCurl(r.URL, r.Pin)
	if err != nil {
		return NewHook{}, err
	}
	return NewHook{ID: r.ID, Name: r.Name, URL: r.URL, Pin: r.Pin, Curl: curl}, nil
}

// ListHooks lists webhooks in channels we belong to (all, for admins).
func (c *Client) ListHooks(ctx context.Context) ([]proto.Hook, error) {
	var r proto.HooksRes
	err := c.call(ctx, proto.OpHookList, proto.Empty{}, &r, nil)
	return r.Hooks, err
}

// DeleteHook deletes a webhook (any in a channel we're in; any at all, for admins).
func (c *Client) DeleteHook(ctx context.Context, id uint64) error {
	return c.call(ctx, proto.OpHookDelete, proto.HookArgs{ID: id}, nil, nil)
}
