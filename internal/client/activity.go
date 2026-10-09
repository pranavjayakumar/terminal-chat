package client

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/skailama/tc/internal/proto"
)

// ActivityKind says why an item concerns me.
type ActivityKind int

const (
	ActMention   ActivityKind = iota + 1 // @me
	ActHighlight                         // one of my highlight keywords
	ActReply                             // a reply in a thread I started or replied in
	ActReaction                          // someone reacted to my message
	ActDM                                // a message in a direct message
)

func (k ActivityKind) String() string {
	switch k {
	case ActMention:
		return "mention"
	case ActHighlight:
		return "highlight"
	case ActReply:
		return "reply"
	case ActReaction:
		return "reaction"
	case ActDM:
		return "dm"
	}
	return "?"
}

// ActivityItem is one thing in the activity view. Text fields are raw
// (untrusted): sanitize before showing them.
type ActivityItem struct {
	Kind ActivityKind
	Time int64  // when it happened (ms): the message's time, or the reaction's
	Chan uint64 // channel to open
	Msg  uint64 // message to select: the mention/reply/DM, or the message reacted to
	// Thread is the thread root that Msg belongs to, 0 for a top-level
	// message: open the thread to find it.
	Thread  uint64
	Author  uint64 // who wrote Msg; for ActReaction, who reacted
	Emoji   string // ActReaction only
	Preview string // first non-empty line of Msg's text, at most previewRunes runes
}

const (
	maxActivity  = 1000
	previewRunes = 200
)

// The mention, reply and DM scans only look at recent history: the newest
// scanMessages messages that are also within scanAge of the newest one. That
// bounds the work (and the rows decoded) whatever the cache holds, and an
// item older than that is no longer "activity" anyway. Variables so tests
// can use small bounds.
var (
	scanMessages       = 5000
	scanAge      int64 = 30 * 24 * 3600 * 1000 // ms
)

// maxReacts bounds the reactions table: only the newest are ever shown.
// A variable so a test can use a small bound.
var maxReacts = 1000

// Activity lists, newest first, up to limit (default 100) things in the
// channels I belong to that concern me: @mentions and highlight words,
// replies in threads I started or replied in, reactions to my messages, and
// messages in DMs. Never my own messages or bots', never deleted ones. A
// message that fits several kinds appears once, as the first of the order
// above.
//
// It reads only the local cache. The message streams walk the primary key
// backwards and stop as soon as they have limit items; reactions come from
// an index on time.
//
// Reactions are listed only if their react.add event reached this client
// (live or replayed): a snapshot carries who reacted but not when, so
// reactions that predate the cache can't be placed in time and are left out.
func (c *Client) Activity(limit int) []ActivityItem {
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, maxActivity)
	// One read transaction: a consistent view, and one lock round trip
	// instead of one per statement (each costs several syscalls).
	var out []ActivityItem
	_ = c.readTx(func(q dbtx) error {
		out = c.activity(q, c.Me(), limit)
		return nil
	})
	return out
}

// ActivitySeen is the time (ms) of the newest activity item that was on
// screen the last time the activity view was open; 0 if it never was. It
// lives in the cache, so it survives restarts.
func (c *Client) ActivitySeen() int64 {
	v, _ := metaUint(c.db, "activity_seen")
	return int64(v)
}

// SetActivitySeen records that items up to time ms were seen. It only ever
// moves forward.
func (c *Client) SetActivitySeen(ms int64) error {
	_, err := c.db.Exec(`INSERT INTO meta (k, v) VALUES ('activity_seen', ?)
		ON CONFLICT (k) DO UPDATE SET v = MAX(v, excluded.v)`, ms)
	if err != nil {
		return fmt.Errorf("saving activity time: %w", err)
	}
	return nil
}

func (c *Client) activity(q dbtx, me proto.User, limit int) []ActivityItem {
	chanKind := memberChans(q, me.ID)
	if len(chanKind) == 0 {
		return nil
	}
	bots := c.botSet()
	ok := func(m proto.Message) bool {
		_, in := chanKind[m.Chan]
		return in && !bots[m.Author]
	}
	words := highlightsOf(q)
	floorID, floorTime := scanFloor(q)
	seen := map[uint64]bool{}
	var out []ActivityItem
	add := func(kind ActivityKind, m proto.Message) {
		seen[m.ID] = true
		out = append(out, ActivityItem{Kind: kind, Time: m.Time, Chan: m.Chan, Msg: m.ID, Thread: m.Thread,
			Author: m.Author, Preview: previewOf(m.Body)})
	}

	// Mentions and highlights.
	filter, fargs := highlightFilter("m.body", me.Name, words)
	scanMsgs(q, `SELECT `+msgCols+` FROM msgs m WHERE m.id >= ? AND m.time >= ? AND m.author != ? AND m.deleted = 0 AND `+filter+` ORDER BY m.id DESC`,
		append([]any{floorID, floorTime, me.ID}, fargs...), limit, func(m proto.Message) bool {
			switch {
			case !ok(m):
			case mentions(m.Body, me.Name):
				add(ActMention, m)
				return true
			case Highlighted(m.Body, words):
				add(ActHighlight, m)
				return true
			}
			return false
		})

	// Replies in threads I started or took part in (before the reply). Each
	// thread is checked once, by the (thread, id) index and by primary key.
	firstReply := map[uint64]uint64{} // thread → my first reply, 0 if none
	tookPart := func(thread, before uint64) bool {
		f, known := firstReply[thread]
		if !known {
			_ = q.QueryRow(`SELECT COALESCE(MIN(id), 0) FROM msgs WHERE thread = ? AND author = ?`, thread, me.ID).Scan(&f)
			firstReply[thread] = f
		}
		return f != 0 && f < before
	}
	myRoot := map[uint64]bool{}
	isMyRoot := func(thread uint64) bool {
		v, known := myRoot[thread]
		if !known {
			var author uint64
			v = q.QueryRow(`SELECT author FROM msgs WHERE id = ?`, thread).Scan(&author) == nil && author == me.ID
			myRoot[thread] = v
		}
		return v
	}
	scanMsgs(q, `SELECT `+msgCols+` FROM msgs r WHERE r.id >= ? AND r.time >= ? AND r.thread != 0 AND r.author != ? AND r.deleted = 0 ORDER BY r.id DESC`,
		[]any{floorID, floorTime, me.ID}, limit, func(m proto.Message) bool {
			if seen[m.ID] || !ok(m) || !(tookPart(m.Thread, m.ID) || isMyRoot(m.Thread)) {
				return false
			}
			add(ActReply, m)
			return true
		})

	// Direct messages.
	scanMsgs(q, `SELECT `+msgCols+` FROM msgs m WHERE m.id >= ? AND m.time >= ? AND m.author != ? AND m.deleted = 0
		AND m.chan IN (SELECT id FROM chans WHERE kind = ?) ORDER BY m.id DESC`,
		[]any{floorID, floorTime, me.ID, proto.ChanDM}, limit, func(m proto.Message) bool {
			if seen[m.ID] || !ok(m) || chanKind[m.Chan] != proto.ChanDM {
				return false
			}
			add(ActDM, m)
			return true
		})

	// Reactions to my messages.
	if rows, err := q.Query(`SELECT r.emoji, r.actor, r.time, m.id, m.chan, m.thread, m.body
		FROM reacts r JOIN msgs m ON m.id = r.msg WHERE m.author = ? AND m.deleted = 0
		ORDER BY r.time DESC, r.msg DESC`, me.ID); err == nil {
		n := 0
		for rows.Next() && n < limit {
			var it ActivityItem
			var body string
			if rows.Scan(&it.Emoji, &it.Author, &it.Time, &it.Msg, &it.Chan, &it.Thread, &body) != nil {
				break
			}
			if _, in := chanKind[it.Chan]; !in || bots[it.Author] {
				continue
			}
			it.Kind, it.Preview = ActReaction, previewOf(body)
			out = append(out, it)
			n++
		}
		rows.Close()
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Time != b.Time {
			return a.Time > b.Time
		}
		if a.Msg != b.Msg {
			return a.Msg > b.Msg
		}
		return a.Kind < b.Kind
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// scanFloor is the oldest message id and time the scans may look at: the
// scanMessages-th newest id (a walk down the primary key) and scanAge before
// the newest message's time. Relative to the newest message, not the clock, so
// an idle cache still shows its last activity.
func scanFloor(q dbtx) (id uint64, ms int64) {
	var newest int64
	if q.QueryRow(`SELECT time FROM msgs ORDER BY id DESC LIMIT 1`).Scan(&newest) != nil {
		return 0, 0
	}
	_ = q.QueryRow(`SELECT id FROM msgs ORDER BY id DESC LIMIT 1 OFFSET ?`, scanMessages-1).Scan(&id)
	return id, newest - scanAge
}

// scanMsgs streams a message query and stops once take has returned true
// limit times.
func scanMsgs(db dbtx, q string, args []any, limit int, take func(proto.Message) bool) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return
	}
	defer rows.Close()
	for n := 0; n < limit && rows.Next(); {
		m, err := scanMsg(rows)
		if err != nil {
			return
		}
		if take(m) {
			n++
		}
	}
}

// memberChans maps the channels I'm a member of to their kind.
func memberChans(db dbtx, me uint64) map[uint64]string {
	rows, err := db.Query(`SELECT ` + chanCols + ` FROM chans`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[uint64]string{}
	for rows.Next() {
		ch, err := scanChan(rows)
		if err != nil {
			return nil
		}
		if slices.Contains(ch.Members, me) {
			out[ch.ID] = ch.Kind
		}
	}
	return out
}

func (c *Client) botSet() map[uint64]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[uint64]bool{}
	for id, u := range c.users {
		if u.Bot {
			out[id] = true
		}
	}
	return out
}

// previewOf is the first non-empty line of body, preferring text over a
// code fence line, cut to previewRunes runes.
func previewOf(body string) string {
	first := ""
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if first == "" {
			first = l
		}
		if !strings.HasPrefix(l, "```") {
			first = l
			break
		}
	}
	if utf8.RuneCountInString(first) > previewRunes {
		first = string([]rune(first)[:previewRunes])
	}
	return first
}

// noteReaction records a reaction someone else added to my message, or
// forgets it when they take it back.
func noteReaction(q dbtx, d proto.Reaction, ev proto.Event, me uint64) error {
	if ev.Actor == me || ev.Actor == 0 {
		return nil
	}
	if ev.Kind == proto.KReactRemove {
		_, err := q.Exec(`DELETE FROM reacts WHERE msg = ? AND actor = ? AND emoji = ?`, d.Msg, ev.Actor, d.Emoji)
		return err
	}
	var author uint64
	err := q.QueryRow(`SELECT author FROM msgs WHERE id = ?`, d.Msg).Scan(&author)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && author != me) {
		return nil // unknown message, or not mine
	}
	if err != nil {
		return err
	}
	if _, err := q.Exec(`INSERT OR REPLACE INTO reacts (msg, actor, emoji, time) VALUES (?, ?, ?, ?)`,
		d.Msg, ev.Actor, d.Emoji, ev.Time); err != nil {
		return err
	}
	_, err = q.Exec(`DELETE FROM reacts WHERE rowid IN (SELECT rowid FROM reacts ORDER BY time DESC, msg DESC LIMIT -1 OFFSET ?)`, maxReacts)
	return err
}
