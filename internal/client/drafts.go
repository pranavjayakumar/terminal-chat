package client

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// Draft is unsent input kept for one place to type: a channel (Thread 0) or
// one thread of it. Drafts are local only: they live in the cache, survive
// restarts and snapshot resets, and are never sent anywhere.
type Draft struct {
	Chan, Thread uint64
	Body         string
}

// Drafts lists every saved draft, most recently saved first.
func (c *Client) Drafts() []Draft {
	rows, err := c.db.Query(`SELECT chan, thread, body FROM drafts ORDER BY time DESC, chan, thread`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Draft
	for rows.Next() {
		var d Draft
		if rows.Scan(&d.Chan, &d.Thread, &d.Body) == nil {
			out = append(out, d)
		}
	}
	return out
}

// Draft returns the saved draft for a channel or thread ("" if none).
func (c *Client) Draft(ch, thread uint64) string {
	var body string
	if err := c.db.QueryRow(`SELECT body FROM drafts WHERE chan = ? AND thread = ?`, ch, thread).Scan(&body); err != nil {
		return ""
	}
	return body
}

// SetDraft saves the unsent input for a channel (thread 0) or a thread. An
// empty body (or only whitespace) removes the draft.
func (c *Client) SetDraft(ch, thread uint64, body string) error {
	if ch == 0 {
		return errors.New("draft: no channel")
	}
	if strings.TrimSpace(body) == "" {
		_, err := c.db.Exec(`DELETE FROM drafts WHERE chan = ? AND thread = ?`, ch, thread)
		return wrapDraft(err)
	}
	if len(body) > proto.MaxBody {
		// A longer message could not be sent anyway.
		body = strings.ToValidUTF8(body[:proto.MaxBody], "")
	}
	_, err := c.db.Exec(`INSERT INTO drafts (chan, thread, body, time) VALUES (?, ?, ?, ?)
		ON CONFLICT (chan, thread) DO UPDATE SET body = excluded.body, time = excluded.time`,
		ch, thread, body, time.Now().UnixMilli())
	return wrapDraft(err)
}

func wrapDraft(err error) error {
	if err != nil {
		return fmt.Errorf("saving draft: %w", err)
	}
	return nil
}
