package client

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/skailama/tc/internal/proto"
)

// Pinned messages (PROTOCOL.md §5, §6). The pins table holds a channel's
// current pins in pin order (id ascending = oldest pin first); data is the
// message as last fetched with pins.list, for pins whose message isn't in
// the msgs cache (older than the history we hold).

const pinsSchema = `
CREATE TABLE IF NOT EXISTS pins (
	id INTEGER PRIMARY KEY AUTOINCREMENT, chan INTEGER NOT NULL, msg INTEGER NOT NULL UNIQUE, data BLOB);
CREATE INDEX IF NOT EXISTS pins_chan ON pins (chan, id);
`

// storeChan caches ch and replaces its pin list with ch.Pins (newest first).
func storeChan(q dbtx, ch proto.Channel) error {
	if err := upsertChan(q, ch); err != nil {
		return err
	}
	return setPins(q, ch.ID, ch.Pins)
}

func setPins(q dbtx, ch uint64, newestFirst []uint64) error {
	rows, err := q.Query(`SELECT msg FROM pins WHERE chan = ? ORDER BY id DESC`, ch)
	if err != nil {
		return err
	}
	var cur []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		cur = append(cur, id)
	}
	rows.Close()
	if equalIDs(cur, newestFirst) {
		return nil
	}
	// Keep fetched message bodies for pins that stay.
	keep := map[uint64][]byte{}
	drows, err := q.Query(`SELECT msg, data FROM pins WHERE chan = ? AND data IS NOT NULL`, ch)
	if err != nil {
		return err
	}
	for drows.Next() {
		var id uint64
		var d []byte
		if err := drows.Scan(&id, &d); err != nil {
			drows.Close()
			return err
		}
		keep[id] = d
	}
	drows.Close()
	if _, err := q.Exec(`DELETE FROM pins WHERE chan = ?`, ch); err != nil {
		return err
	}
	for i := len(newestFirst) - 1; i >= 0; i-- {
		if _, err := q.Exec(`INSERT OR REPLACE INTO pins (chan, msg, data) VALUES (?, ?, ?)`, ch, newestFirst[i], keep[newestFirst[i]]); err != nil {
			return err
		}
	}
	return nil
}

func equalIDs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func addPin(q dbtx, ch, msg uint64) error {
	_, err := q.Exec(`INSERT OR IGNORE INTO pins (chan, msg) VALUES (?, ?)`, ch, msg)
	return err
}

func removePin(q dbtx, msg uint64) error {
	_, err := q.Exec(`DELETE FROM pins WHERE msg = ?`, msg)
	return err
}

// pinIDs maps every cached channel to its pinned message ids, newest pin first.
func (c *Client) pinIDs() map[uint64][]uint64 {
	out := map[uint64][]uint64{}
	rows, err := c.db.Query(`SELECT chan, msg FROM pins ORDER BY id DESC`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var ch, msg uint64
		if rows.Scan(&ch, &msg) == nil {
			out[ch] = append(out[ch], msg)
		}
	}
	return out
}

// markPinned sets Pinned on the messages that have a pin.
func (c *Client) markPinned(ms []Message) {
	if len(ms) == 0 {
		return
	}
	have := map[uint64]bool{}
	rows, err := c.db.Query(`SELECT msg FROM pins`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id uint64
		if rows.Scan(&id) == nil {
			have[id] = true
		}
	}
	for i := range ms {
		ms[i].Pinned = ms[i].ID != 0 && have[ms[i].ID]
	}
}

// Pins lists ch's pinned messages, newest pin first, from the local cache.
// A pinned message we never loaded shows only after FetchPins.
func (c *Client) Pins(ch uint64) []Message {
	cols := "m." + strings.ReplaceAll(msgCols, ", ", ", m.")
	rows, err := c.db.Query(`SELECT p.data, `+cols+`, m.sec FROM pins p LEFT JOIN msgs m ON m.id = p.msg
		WHERE p.chan = ? ORDER BY p.id DESC`, ch)
	if err != nil {
		return nil
	}
	var out []Message
	var unjudged []int // pins.list copies of encrypted messages we don't cache
	for rows.Next() {
		var data []byte
		var id, mch, author, thread, time, edited, sec sql.NullInt64
		var body, reactions sql.NullString
		var deleted sql.NullBool
		if err := rows.Scan(&data, &id, &mch, &author, &body, &thread, &time, &edited, &deleted, &reactions, &sec); err != nil {
			break
		}
		var m proto.Message
		switch {
		case id.Valid:
			m = proto.Message{ID: uint64(id.Int64), Chan: uint64(mch.Int64), Author: uint64(author.Int64), Body: body.String,
				Thread: uint64(thread.Int64), Time: time.Int64, Edited: edited.Int64, Deleted: deleted.Bool}
			if reactions.String != "" && reactions.String != "{}" {
				_ = json.Unmarshal([]byte(reactions.String), &m.Reactions)
			}
		case len(data) > 0 && proto.Unmarshal(data, &m) == nil:
			sec.Int64 = -1
		default:
			continue
		}
		if m.Deleted {
			continue
		}
		if sec.Int64 == -1 {
			unjudged = append(unjudged, len(out))
		}
		out = append(out, Message{Message: m, Sec: Sec(max(sec.Int64, 0))})
	}
	rows.Close()
	for _, i := range unjudged {
		m := &out[i]
		sec, _ := c.prepMsg(c.db, &m.Message, nil)
		if sec == SecWaiting || sec == SecInvalid {
			m.Body = ""
		}
		m.Sec, m.Enc = sec, nil
	}
	return out
}

// FetchPins asks the server for ch's pinned messages (pins.list) and keeps
// them so Pins can show those not in the message cache. It also resyncs the
// pin list itself, which fixes any drift while offline.
func (c *Client) FetchPins(ctx context.Context, ch uint64) error {
	var r proto.MsgsRes
	if err := c.call(ctx, proto.OpPinsList, proto.ChanArgs{Chan: ch}, &r, nil); err != nil {
		return err
	}
	err := c.tx(func(tx *sql.Tx) error {
		ids := make([]uint64, 0, len(r.Msgs))
		for _, m := range r.Msgs {
			ids = append(ids, m.ID)
		}
		if err := setPins(tx, ch, ids); err != nil {
			return err
		}
		for _, m := range r.Msgs {
			raw, err := proto.Marshal(m)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE pins SET data = ? WHERE msg = ?`, raw, m.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	c.post(Update{Channels: true})
	return nil
}

// Pin pins a message in its channel (no-op if already pinned).
func (c *Client) Pin(ctx context.Context, msg uint64) error {
	return c.call(ctx, proto.OpPin, proto.MsgArgs{Msg: msg}, nil, nil)
}

// Unpin removes a message's pin (no-op if not pinned).
func (c *Client) Unpin(ctx context.Context, msg uint64) error {
	return c.call(ctx, proto.OpUnpin, proto.MsgArgs{Msg: msg}, nil, nil)
}
