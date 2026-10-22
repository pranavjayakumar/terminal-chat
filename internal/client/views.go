package client

import (
	"slices"
	"strings"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// Message is a cached message, or an outbox entry not yet acknowledged
// (Pending, ID 0) or permanently rejected (Failed holds the reason). Local
// is the outbox id for RetryFailed/DiscardFailed.
type Message struct {
	proto.Message
	Replies int
	Pinned  bool // currently pinned in its channel
	Pending bool
	Failed  string
	Local   int64
	// Sec says whether an encrypted channel's message was decrypted and
	// verified, is waiting for a key, is invalid, or is plaintext (legacy
	// or inserted by the server). Body is "" unless the text can be shown.
	Sec Sec
	// Wait says why a SecWaiting message can't be read yet.
	Wait WaitReason
	// SignedTime is a verified encrypted message's own timestamp (its
	// author's clock, Plain ts), 0 otherwise. Time is the server's claim.
	SignedTime int64
}

// MaxTimeDrift is how far a verified message's signed time may be from
// the server's before the UI says so (clock skew; PROTOCOL §13.4).
const MaxTimeDrift = 5 * time.Minute

// TimeDrift reports whether m's signed time differs from the server's by
// more than MaxTimeDrift: the server may have rewritten when it was sent.
func (m Message) TimeDrift() bool {
	if m.SignedTime == 0 || m.Time == 0 {
		return false
	}
	d := m.SignedTime - m.Time
	return d > MaxTimeDrift.Milliseconds() || -d > MaxTimeDrift.Milliseconds()
}

// ShownTime is the time to show for m (Unix ms): the signed time when the
// server's differs by more than MaxTimeDrift, else the server's.
func (m Message) ShownTime() int64 {
	if m.TimeDrift() {
		return m.SignedTime
	}
	return m.Time
}

// ChannelView is a channel plus per-user state. Display is the name to show:
// the channel name, or for DMs the other members' names.
type ChannelView struct {
	proto.Channel
	Display string
	Member  bool
	// Removed: someone else removed us; the channel keeps the history we
	// had until Leave closes it.
	Removed bool
	Unread  int    // messages after the read marker not written by me
	Mention bool   // an unread message by a person @mentions me or has one of my highlight keywords, or any unread by a person in a DM
	Read    uint64 // read marker (message id)
	Last    uint64 // newest cached message id, for activity sorting
}

// Channels lists cached channels: named channels by name, then DMs.
func (c *Client) Channels() []ChannelView {
	me := c.Me()
	rows, err := c.db.Query(`SELECT ` + chanCols + ` FROM chans`)
	if err != nil {
		return nil
	}
	var out []ChannelView
	for rows.Next() {
		ch, err := scanChan(rows)
		if err != nil {
			rows.Close()
			return nil
		}
		out = append(out, ChannelView{Channel: ch, Member: slices.Contains(ch.Members, me.ID)})
	}
	rows.Close()

	removed := c.uintMap(`SELECT chan, actor FROM removed`)
	reads := c.uintMap(`SELECT chan, seq FROM reads`)
	last := c.uintMap(`SELECT chan, MAX(id) FROM msgs GROUP BY chan`)
	unread := c.uintMap(`SELECT m.chan, COUNT(*) FROM msgs m LEFT JOIN reads r ON r.chan = m.chan
		WHERE m.id > COALESCE(r.seq, 0) AND m.author != ? AND m.deleted = 0 GROUP BY m.chan`, me.ID)
	// Bot (webhook) messages count as unread but never as mentions: their
	// text comes from outside (e.g. a branch named "fix-@priya").
	human := c.uintMap(`SELECT m.chan, COUNT(*) FROM msgs m LEFT JOIN reads r ON r.chan = m.chan
		WHERE m.id > COALESCE(r.seq, 0) AND m.author != ? AND m.deleted = 0 AND m.sec IN (?, ?)
		AND m.author NOT IN (SELECT id FROM users WHERE bot = 1) GROUP BY m.chan`, me.ID, SecPlain, SecE2E)
	// A mention is @me or one of my highlight keywords. The SQL narrows the
	// rows; Go applies the exact whole-word rules.
	words := c.Highlights()
	names := c.MentionNames()
	mention := map[uint64]bool{}
	filter, fargs := highlightFilter("m.body", names, words)
	// Only authenticated text counts as a mention: never plaintext in an
	// encrypted channel (legacy or inserted by the server).
	if mrows, err := c.db.Query(`SELECT m.chan, m.body FROM msgs m LEFT JOIN reads r ON r.chan = m.chan
		WHERE m.id > COALESCE(r.seq, 0) AND m.author != ? AND m.deleted = 0 AND m.sec IN (?, ?) AND `+filter+`
		AND m.author NOT IN (SELECT id FROM users WHERE bot = 1)`,
		append([]any{me.ID, SecPlain, SecE2E}, fargs...)...); err == nil {
		for mrows.Next() {
			var ch uint64
			var body string
			if mrows.Scan(&ch, &body) == nil && !mention[ch] && (Mentioned(body, names) || Highlighted(body, words)) {
				mention[ch] = true
			}
		}
		mrows.Close()
	}

	pins := c.pinIDs()
	for i := range out {
		v := &out[i]
		v.Pins = pins[v.ID]
		v.Read, v.Last = reads[v.ID], last[v.ID]
		v.Display = c.displayName(v.Channel, me.ID)
		_, v.Removed = removed[v.ID]
		v.Removed = v.Removed && !v.Member
		if v.Member {
			v.Unread = int(unread[v.ID])
			v.Mention = mention[v.ID] || (v.Kind == proto.ChanDM && human[v.ID] > 0)
			// Reminders are my own messages, which never count as unread,
			// but they exist to be noticed.
			if v.Kind == proto.ChanDM && len(v.Members) == 1 {
				if n := c.unreadReminders(v.ID, me.ID); n > 0 {
					v.Unread += n
					v.Mention = true
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b ChannelView) int {
		if ad, bd := a.Kind == proto.ChanDM, b.Kind == proto.ChanDM; ad != bd {
			if ad {
				return 1
			}
			return -1
		}
		return strings.Compare(a.Display, b.Display)
	})
	return out
}

// Channel returns one channel's view.
func (c *Client) Channel(id uint64) (ChannelView, bool) {
	for _, v := range c.Channels() {
		if v.ID == id {
			return v, true
		}
	}
	return ChannelView{}, false
}

func (c *Client) uintMap(q string, args ...any) map[uint64]uint64 {
	m := map[uint64]uint64{}
	rows, err := c.db.Query(q, args...)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var k, v uint64
		if rows.Scan(&k, &v) == nil {
			m[k] = v
		}
	}
	return m
}

func (c *Client) displayName(ch proto.Channel, me uint64) string {
	if ch.Kind != proto.ChanDM {
		return ch.Name
	}
	var names []string
	for _, id := range ch.Members {
		if id != me {
			names = append(names, c.User(id).Name)
		}
	}
	if len(names) == 0 {
		return c.User(me).Name + " (notes)"
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// mentions reports whether body contains @name as a whole word
// (case-insensitive, not inside an email address or a longer name).
func mentions(body, name string) bool {
	if name == "" {
		return false
	}
	lb, pat := strings.ToLower(body), "@"+strings.ToLower(name)
	for i := 0; i < len(lb); {
		j := strings.Index(lb[i:], pat)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(pat)
		if (j == 0 || !isWordByte(lb[j-1])) && (end == len(lb) || !(isWordByte(lb[end]) || lb[end] == '-')) {
			return true
		}
		i = j + 1
	}
	return false
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}

// Messages returns the latest limit top-level messages of ch, oldest first,
// followed by our unsent/failed top-level messages for ch.
func (c *Client) Messages(ch uint64, limit int) []Message {
	if limit <= 0 {
		limit = 100
	}
	out := c.queryMsgs(`SELECT `+msgCols+`, sec, sts, (SELECT COUNT(*) FROM msgs r WHERE r.thread = m.id)
		FROM (SELECT * FROM msgs WHERE chan = ? AND thread = 0 ORDER BY id DESC LIMIT ?) m ORDER BY id`, ch, limit)
	c.markPinned(out)
	return append(out, c.outboxMsgs(`chan = ? AND thread = 0`, ch)...)
}

// Thread returns the root message, its replies, then our unsent replies.
func (c *Client) Thread(root uint64) []Message {
	out := c.queryMsgs(`SELECT `+msgCols+`, sec, sts, (SELECT COUNT(*) FROM msgs r WHERE r.thread = m.id)
		FROM msgs m WHERE id = ?`, root)
	out = append(out, c.queryMsgs(`SELECT `+msgCols+`, sec, sts, 0 FROM msgs WHERE thread = ? ORDER BY id`, root)...)
	c.markPinned(out)
	return append(out, c.outboxMsgs(`thread = ?`, root)...)
}

func (c *Client) queryMsgs(q string, args ...any) []Message {
	rows, err := c.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		pm, err := scanMsg(rows, &m.Sec, &m.SignedTime, &m.Replies)
		if err != nil {
			return out
		}
		m.Message = pm
		out = append(out, m)
	}
	rows.Close()
	c.setWaitReasons(out)
	return out
}

// outboxMsgs lists outbox entries as messages. An entry matched to a message
// that is in the cache is hidden: the real copy shows instead.
func (c *Client) outboxMsgs(where string, args ...any) []Message {
	rows, err := c.db.Query(`SELECT `+outboxCols+` FROM outbox WHERE `+where+
		` AND (failed != '' OR NOT EXISTS (SELECT 1 FROM msgs WHERE msgs.id = outbox.msg)) ORDER BY id`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	me := c.Me().ID
	var out []Message
	for rows.Next() {
		e, err := scanOutbox(rows)
		if err != nil {
			return out
		}
		out = append(out, e.message(me))
	}
	return out
}

func (e outboxEntry) message(me uint64) Message {
	return Message{
		Message: proto.Message{Chan: e.chan_, Author: me, Body: e.body, Thread: e.thread, Time: e.time},
		Pending: e.failed == "", Failed: e.failed, Local: e.id,
	}
}

// User returns a cached user (zero User with only ID set if unknown).
func (c *Client) User(id uint64) proto.User {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u, ok := c.users[id]; ok {
		return u
	}
	return proto.User{ID: id}
}

// Users lists cached users by name.
func (c *Client) Users() []proto.User {
	c.mu.Lock()
	out := make([]proto.User, 0, len(c.users))
	for _, u := range c.users {
		out = append(out, u)
	}
	c.mu.Unlock()
	slices.SortFunc(out, func(a, b proto.User) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// TypingUsers lists users typing in ch within the last few seconds.
func (c *Client) TypingUsers(ch uint64) []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []uint64
	for u, t := range c.typing[ch] {
		if time.Since(t) < c.timing.type_ {
			out = append(out, u)
		} else {
			delete(c.typing[ch], u)
		}
	}
	slices.Sort(out)
	return out
}

// Presence is a user's last known presence (State "" when unknown or offline
// from our side).
func (c *Client) Presence(user uint64) Presence {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.presence[user]
}
