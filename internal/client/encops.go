package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/trust"
)

// Encrypted edits, deletes and reactions (PROTOCOL.md §13.4; docs/E2E.md
// step 8). Each carries an enc whose Plain names the original message by
// its mid (ref) and is signed by a device of whoever acted:
//
//   - edit: the original author's, with a version v above the last applied
//     one, sealed with the channel's current epoch (like a send);
//   - del: the author's; a delete without it (an admin, the server) shows as
//     "removed (not by author)";
//   - react: the reacting user's, sealed under the message's own epoch,
//     whose key also makes the tag the server sees instead of the emoji.
//
// Anything that doesn't verify changes nothing; the reason goes to
// E2ERejects.

// plainEdit is Plain t = "edit".
type plainEdit struct {
	T    string `msgpack:"t"`
	TS   int64  `msgpack:"ts"`
	Body string `msgpack:"body"`
	Ref  []byte `msgpack:"ref"`
	V    uint32 `msgpack:"v"`
}

// plainDel is Plain t = "del".
type plainDel struct {
	T   string `msgpack:"t"`
	TS  int64  `msgpack:"ts"`
	Ref []byte `msgpack:"ref"`
}

// plainReact is Plain t = "react".
type plainReact struct {
	T     string `msgpack:"t"`
	TS    int64  `msgpack:"ts"`
	Ref   []byte `msgpack:"ref"`
	Emoji string `msgpack:"emoji"`
	Add   bool   `msgpack:"add"`
}

// msgRow is a cached message with its encryption state.
type msgRow struct {
	id, ch, author              uint64
	sec                         Sec
	body                        string
	mid, enc, eenc, epend, denc []byte
	ev                          uint32
	ets                         int64
	deleted                     bool
	edited                      int64
}

func getMsgRow(q dbtx, id uint64) (msgRow, bool) {
	var r msgRow
	err := q.QueryRow(`SELECT id, chan, author, sec, body, mid, enc, eenc, epend, denc, ev, ets, deleted, edited FROM msgs WHERE id = ?`, id).
		Scan(&r.id, &r.ch, &r.author, &r.sec, &r.body, &r.mid, &r.enc, &r.eenc, &r.epend, &r.denc, &r.ev, &r.ets, &r.deleted, &r.edited)
	return r, err == nil
}

// ---- judging ----

// openEnc checks and decrypts an enc that author sent in ch: it needs the
// verified K of its epoch and that epoch's eh, and must be signed by a
// verified device of author not revoked before seq (0: seq unknown; the
// caller checks revokedAt against the Plain's ts). SecWaiting: K or the
// device isn't here yet. why says what failed.
func (c *Client) openEnc(sid e2ecrypto.ServerID, ch, author, seq uint64, enc []byte, memo *keyMemo) (raw []byte, d *trust.Device, sec Sec, why string) {
	e, err := e2ecrypto.ParseEnc(enc)
	if err != nil {
		return nil, nil, SecInvalid, "malformed enc"
	}
	ep := memo.epoch(c.trust, sid, ch, e.Epoch)
	if ep == nil || ep.Key == nil {
		return nil, nil, SecWaiting, ""
	}
	if !bytes.Equal(e.EH[:], ep.Hash[:e2ecrypto.IDSize]) {
		return nil, nil, SecInvalid, "names another bundle of its epoch"
	}
	if d = memo.device(c.trust, sid, e.Dev); d == nil {
		return nil, nil, SecWaiting, ""
	}
	if d.User != author {
		return nil, nil, SecInvalid, fmt.Sprintf("signed by a device of user %d, not %d", d.User, author)
	}
	if d.Revoked && d.RevokedSeq != 0 && seq > d.RevokedSeq {
		return nil, nil, SecInvalid, "signed by a revoked device"
	}
	if raw, err = e2ecrypto.Decrypt(*ep.Key, d.Ed25519[:], sid, ch, enc); err != nil {
		return nil, nil, SecInvalid, "bad signature or ciphertext"
	}
	return raw, d, SecE2E, ""
}

// checkSealer applies to what is sealed under the current epoch (messages
// and edits; not deletes or reactions, sealed under their message's epoch):
// the author must be in the roster of the epoch it names, and with a known
// seq the event must not come after the next epoch's key.epoch. An honest
// server refuses both (stale_epoch, not a member), so a removed member
// can't go on posting under the last epoch it held, even with the server's
// help (step 12 security pass). Messages judged before the next epoch
// verified are re-checked then (invalidateSuperseded).
func (c *Client) checkSealer(sid e2ecrypto.ServerID, ch, author, seq uint64, enc []byte, memo *keyMemo) (Sec, string) {
	e, err := e2ecrypto.ParseEnc(enc)
	if err != nil {
		return SecInvalid, "malformed enc"
	}
	ep := memo.epoch(c.trust, sid, ch, e.Epoch)
	if ep == nil {
		return SecWaiting, ""
	}
	if !slices.Contains(ep.Roster, author) {
		return SecInvalid, fmt.Sprintf("author not in the roster of epoch %d", e.Epoch)
	}
	if seq == 0 {
		return SecE2E, ""
	}
	if next := memo.epoch(c.trust, sid, ch, e.Epoch+1); next != nil {
		if next.Seq != 0 && seq > next.Seq {
			return SecInvalid, fmt.Sprintf("sealed under epoch %d after epoch %d", e.Epoch, e.Epoch+1)
		}
	}
	return SecE2E, ""
}

// invalidateSuperseded is checkSealer for messages judged before epoch
// (now verified, its key.epoch at seq) was: those sealed under the epoch
// before it with a later id become invalid.
func (c *Client) invalidateSuperseded(ch uint64, epoch uint32, seq uint64) bool {
	if epoch < 2 || seq == 0 {
		return false
	}
	prev := binary.BigEndian.AppendUint32([]byte{e2ecrypto.EncVersion}, epoch-1)
	r, err := c.db.Exec(`UPDATE msgs SET sec = ?, body = '' WHERE chan = ? AND sec = ? AND id > ? AND substr(enc, 1, 5) = ?`,
		SecInvalid, ch, SecE2E, seq, prev)
	if err != nil {
		return false
	}
	n, _ := r.RowsAffected()
	return n > 0
}

// revokedAt: a revoke whose seq we don't know is judged by its signed
// time, with the same clock skew as bundles (§13.3).
func revokedAt(d *trust.Device, ts int64) bool {
	return d.Revoked && d.RevokedTime != 0 && ts > d.RevokedTime+revokeSkew.Milliseconds()
}

const maxRejects = 64

// reject records why an encrypted edit, delete or reaction was ignored.
func (c *Client) reject(msg uint64, format string, args ...any) {
	s := fmt.Sprintf("message %d: ", msg) + fmt.Sprintf(format, args...)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.rejects) == maxRejects {
		c.rejects = c.rejects[1:]
	}
	c.rejects = append(c.rejects, s)
}

// E2ERejects lists why recent encrypted edits, deletes and reactions were
// ignored (oldest first, bounded): a log for debugging and tests.
func (c *Client) E2ERejects() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.rejects...)
}

// applyEdit applies an encrypted edit of row if it verifies: signed by a
// verified device of the original author, t = "edit", ref = the
// original's mid, valid text and newer than the applied one (newerEdit).
// Otherwise nothing changes. One it can't judge yet is kept (epend) and
// tried again by the e2e worker. Its edited time is the signed Plain ts,
// not the server's claim.
func (c *Client) applyEdit(q dbtx, row msgRow, enc []byte, seq uint64, memo *keyMemo) bool {
	if row.deleted || row.mid == nil || row.sec == SecInvalid {
		c.reject(row.id, "edit of a message that can't be edited")
		return false
	}
	if bytes.Equal(enc, row.eenc) {
		return false
	}
	sid, ok := qServerID(q)
	if row.sec == SecWaiting || !ok {
		q.Exec(`UPDATE msgs SET epend = ? WHERE id = ?`, enc, row.id)
		c.trimPending(q, row.ch)
		return false
	}
	if row.sec != SecE2E {
		c.reject(row.id, "edit of an unauthenticated message")
		return false
	}
	raw, d, sec, why := c.openEnc(sid, row.ch, row.author, seq, enc, memo)
	if sec == SecE2E {
		sec, why = c.checkSealer(sid, row.ch, row.author, seq, enc, memo)
	}
	if sec == SecWaiting {
		q.Exec(`UPDATE msgs SET epend = ? WHERE id = ?`, enc, row.id)
		c.trimPending(q, row.ch)
		return false
	}
	// Judged: whatever was pending for this message is settled.
	q.Exec(`UPDATE msgs SET epend = NULL WHERE id = ? AND epend = ?`, row.id, enc)
	if sec != SecE2E {
		c.reject(row.id, "edit: %s", why)
		return false
	}
	var p plainEdit
	switch {
	case proto.Unmarshal(raw, &p) != nil || p.T != "edit":
		c.reject(row.id, "edit: not an edit")
	case !bytes.Equal(p.Ref, row.mid):
		c.reject(row.id, "edit: made for another message")
	case !validText(p.Body):
		c.reject(row.id, "edit: invalid text")
	case revokedAt(d, p.TS):
		c.reject(row.id, "edit: signed after its device was revoked")
	case !newerEdit(p.V, p.TS, enc, row):
		c.reject(row.id, "edit: (v %d, ts %d) is not newer than the applied (v %d, ts %d)", p.V, p.TS, row.ev, row.ets)
	default:
		_, err := q.Exec(`UPDATE msgs SET body = ?, edited = ?, ev = ?, ets = ?, eenc = ?, epend = NULL WHERE id = ?`,
			p.Body, max(p.TS, 1), p.V, p.TS, enc, row.id)
		return err == nil
	}
	return false
}

// newerEdit orders edits the same way on every client, whatever the
// arrival order: the higher (v, Plain ts, mid of the edit's enc) wins. Two
// devices editing at once may pick the same v; the tie still has one
// winner. An equal tuple is the same edit (equal mid = equal bytes).
func newerEdit(v uint32, ts int64, enc []byte, row msgRow) bool {
	switch {
	case v == 0: // versions start at 1
		return false
	case v != row.ev:
		return v > row.ev
	case ts != row.ets:
		return ts > row.ets
	}
	mid, cur := e2ecrypto.MID(enc), e2ecrypto.MID(row.eenc)
	return bytes.Compare(mid[:], cur[:]) > 0
}

// deleteVerdict is what a deleted message of an encrypted channel shows
// as: SecE2E when its author signed the delete (enc; ref must be mid when
// we know it), SecWaiting when that can't be judged yet, SecLegacy for
// plaintext from before encryption, else SecRemoved.
func (c *Client) deleteVerdict(q dbtx, sid e2ecrypto.ServerID, hasSID bool, ch, id, author uint64, mid, enc []byte, seq uint64, memo *keyMemo) Sec {
	if enc == nil {
		if s := c.plainSec(q, ch, id); mid == nil && (s == SecLegacy || s == SecLegacyClaimed) {
			return s
		}
		return SecRemoved
	}
	if !hasSID {
		return SecWaiting
	}
	raw, d, sec, why := c.openEnc(sid, ch, author, seq, enc, memo)
	switch sec {
	case SecWaiting:
		return SecWaiting
	case SecE2E:
	default:
		c.reject(id, "delete: %s", why)
		return SecRemoved
	}
	var p plainDel
	switch {
	case proto.Unmarshal(raw, &p) != nil || p.T != "del":
		c.reject(id, "delete: not a delete")
	case mid == nil || !bytes.Equal(p.Ref, mid):
		c.reject(id, "delete: made for another message")
	case revokedAt(d, p.TS):
		c.reject(id, "delete: signed after its device was revoked")
	default:
		return SecE2E
	}
	return SecRemoved
}

// saveDeleted stores a deleted message of an encrypted channel from the
// server (snapshot, history, msg.new replay): nothing of it stays but its
// mid (replies name it) and who deleted it.
func (c *Client) saveDeleted(q dbtx, m *proto.Message, old msgRow, hadOld bool, memo *keyMemo) (Sec, error) {
	if hadOld && old.sec == SecE2E && !old.deleted && (old.ch != m.Chan || old.author != m.Author) {
		return old.sec, nil // a copy can't move a verified message
	}
	if hadOld && old.deleted && old.sec != SecWaiting && bytes.Equal(old.denc, m.DelEnc) {
		return old.sec, nil
	}
	sid, hasSID := qServerID(q)
	// A delete's ref is checked against the mid we computed from the enc;
	// a device that never saw the enc has only the server's claim (cmid),
	// which must not be another message's. With neither the delete can't be
	// shown as the author's. The claim is never used for threads.
	ref, cmid := old.mid, []byte(nil)
	if ref == nil && len(m.Mid) == e2ecrypto.IDSize {
		var other uint64
		if q.QueryRow(`SELECT id FROM msgs WHERE mid = ? AND id != ? LIMIT 1`, m.Mid, m.ID).Scan(&other) != nil {
			ref, cmid = m.Mid, m.Mid
		}
	}
	sec := c.deleteVerdict(q, sid, hasSID, m.Chan, m.ID, m.Author, ref, m.DelEnc, 0, memo)
	m.Body, m.Enc, m.Reactions, m.Edited = "", nil, nil, 0
	if err := storeMsg(q, *m, sec, old.mid, true); err != nil {
		return sec, err
	}
	if _, err := q.Exec(`UPDATE msgs SET denc = ?, cmid = ? WHERE id = ?`, m.DelEnc, cmid, m.ID); err != nil {
		return sec, err
	}
	if _, err := q.Exec(`DELETE FROM rencs WHERE msg = ?`, m.ID); err != nil {
		return sec, err
	}
	return sec, nil
}

// applyDelete applies a msg.delete event (seq) to a cached message: its
// text goes in any case; whether its author signed it decides how it shows.
func (c *Client) applyDelete(q dbtx, d proto.MsgDelete, seq uint64) error {
	row, ok := getMsgRow(q, d.Msg)
	if !ok {
		return nil
	}
	sid, hasSID := qServerID(q)
	sec := SecPlain
	if row.mid != nil || c.chanIsE2E(q, sid, hasSID, row.ch) {
		sec = c.deleteVerdict(q, sid, hasSID, row.ch, row.id, row.author, row.mid, d.Enc, seq, nil)
	}
	// mid stays: replies keep naming it as their thread root.
	if _, err := q.Exec(`UPDATE msgs SET body = '', enc = NULL, eenc = NULL, epend = NULL, denc = ?, deleted = 1,
		sec = ?, reactions = '{}' WHERE id = ?`, d.Enc, sec, d.Msg); err != nil {
		return err
	}
	if _, err := q.Exec(`DELETE FROM rencs WHERE msg = ?`, d.Msg); err != nil {
		return err
	}
	return nil
}

// ---- reactions ----

// validReactEmoji mirrors the server's emoji rules (§9) for the emoji
// inside an encrypted reaction, which the server can't see.
func validReactEmoji(s string) bool {
	if s == "" || len(s) > proto.MaxEmoji || !sanitize.Valid(s) || strings.ContainsFunc(s, unicode.IsSpace) {
		return false
	}
	for _, r := range s {
		if (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return false
		}
	}
	return true
}

const (
	reactPending = 0
	reactOK      = 1
	reactBad     = 2
)

// judgeReact judges one encrypted reaction by user on row: sealed under
// the message's epoch, signed by a verified device of user, t = "react",
// add, ref = the message's mid, and the tag the server stored is the
// tag of its emoji.
func (c *Client) judgeReact(q dbtx, row msgRow, user uint64, tag string, enc []byte, seq uint64, memo *keyMemo) (int, string) {
	switch {
	case row.sec == SecWaiting:
		return reactPending, ""
	case row.sec != SecE2E || row.mid == nil || row.deleted:
		return reactBad, ""
	}
	sid, ok := qServerID(q)
	if !ok {
		return reactPending, ""
	}
	orig, err1 := e2ecrypto.ParseEnc(row.enc)
	re, err2 := e2ecrypto.ParseEnc(enc)
	if err1 != nil || err2 != nil || re.Epoch != orig.Epoch || re.EH != orig.EH {
		c.reject(row.id, "reaction: not sealed under the message's epoch")
		return reactBad, ""
	}
	raw, d, sec, why := c.openEnc(sid, row.ch, user, seq, enc, memo)
	switch sec {
	case SecWaiting:
		return reactPending, ""
	case SecE2E:
	default:
		c.reject(row.id, "reaction: %s", why)
		return reactBad, ""
	}
	var p plainReact
	if proto.Unmarshal(raw, &p) != nil || p.T != "react" || !p.Add || !validReactEmoji(p.Emoji) {
		c.reject(row.id, "reaction: not a valid reaction")
		return reactBad, ""
	}
	if !bytes.Equal(p.Ref, row.mid) {
		c.reject(row.id, "reaction: made for another message")
		return reactBad, ""
	}
	if revokedAt(d, p.TS) {
		c.reject(row.id, "reaction: signed after its device was revoked")
		return reactBad, ""
	}
	ep := memo.epoch(c.trust, sid, row.ch, orig.Epoch)
	want, err := e2ecrypto.ReactTag(*ep.Key, sid, row.ch, orig.Epoch, e2ecrypto.MsgID(row.mid), p.Emoji)
	if err != nil || want != tag {
		c.reject(row.id, "reaction: tag doesn't match its emoji")
		return reactBad, ""
	}
	return reactOK, p.Emoji
}

// syncRencs makes the cached encrypted reactions of msg the server's list
// (snapshot, history), keeping verdicts of unchanged ones.
func syncRencs(q dbtx, msg uint64, renc []proto.ReactEnc) error {
	keep := map[[2]string]bool{}
	if len(renc) > maxReactsPerMsg {
		renc = renc[:maxReactsPerMsg] // the server caps them too
	}
	for _, r := range renc {
		if r.Enc == nil {
			continue
		}
		keep[[2]string{fmt.Sprint(r.User), r.Emoji}] = true
		if _, err := q.Exec(`INSERT INTO rencs (msg, user, tag, enc) VALUES (?, ?, ?, ?)
			ON CONFLICT (msg, user, tag) DO UPDATE SET enc = excluded.enc,
				st = CASE WHEN enc = excluded.enc THEN st ELSE 0 END,
				emoji = CASE WHEN enc = excluded.enc THEN emoji ELSE '' END`, msg, r.User, r.Emoji, r.Enc); err != nil {
			return err
		}
	}
	rows, err := q.Query(`SELECT user, tag FROM rencs WHERE msg = ?`, msg)
	if err != nil {
		return err
	}
	var gone [][2]any
	for rows.Next() {
		var u uint64
		var t string
		if rows.Scan(&u, &t) == nil && !keep[[2]string{fmt.Sprint(u), t}] {
			gone = append(gone, [2]any{u, t})
		}
	}
	rows.Close()
	for _, g := range gone {
		if _, err := q.Exec(`DELETE FROM rencs WHERE msg = ? AND user = ? AND tag = ?`, msg, g[0], g[1]); err != nil {
			return err
		}
	}
	return nil
}

// refreshReacts judges msg's pending encrypted reactions and rebuilds its
// shown reactions from the verified ones. It reports whether they changed.
func (c *Client) refreshReacts(q dbtx, msg uint64, memo *keyMemo) bool {
	row, ok := getMsgRow(q, msg)
	if !ok {
		return false
	}
	if row.deleted || row.sec == SecInvalid {
		// Nothing to show on them: don't keep (or retry) their reactions.
		q.Exec(`DELETE FROM rencs WHERE msg = ?`, msg)
		return c.rebuildReacts(q, row)
	}
	type pend struct {
		user uint64
		tag  string
		enc  []byte
	}
	var todo []pend
	if rows, err := q.Query(`SELECT user, tag, enc FROM rencs WHERE msg = ? AND st = ?`, msg, reactPending); err == nil {
		for rows.Next() {
			var p pend
			if rows.Scan(&p.user, &p.tag, &p.enc) == nil {
				todo = append(todo, p)
			}
		}
		rows.Close()
	}
	for _, p := range todo {
		if st, emoji := c.judgeReact(q, row, p.user, p.tag, p.enc, 0, memo); st != reactPending {
			q.Exec(`UPDATE rencs SET st = ?, emoji = ? WHERE msg = ? AND user = ? AND tag = ?`, st, emoji, msg, p.user, p.tag)
		}
	}
	return c.rebuildReacts(q, row)
}

// Bounds on what waits for a key or device (a server can send any amount
// of it): pending reactions per message and per channel, pending edits per
// channel (one per message by construction), and reactions kept per
// message. Variables so tests can lower them.
var (
	maxPendingPerMsg  = 16
	maxPendingPerChan = 1000
	maxReactsPerMsg   = 256
)

// trimPending drops the newest pending reactions beyond the per-message
// and per-channel caps in ch, and the newest pending edits beyond the
// per-channel cap. Dropped ones are as if never received.
func (c *Client) trimPending(q dbtx, ch uint64) {
	q.Exec(`DELETE FROM rencs WHERE st = ?1 AND rowid IN (
		SELECT r.rowid FROM rencs r JOIN msgs m ON m.id = r.msg WHERE r.st = ?1 AND m.chan = ?2
		AND (SELECT COUNT(*) FROM rencs o WHERE o.msg = r.msg AND o.st = ?1 AND o.rowid < r.rowid) >= ?3)`,
		reactPending, ch, maxPendingPerMsg)
	q.Exec(`DELETE FROM rencs WHERE rowid IN (
		SELECT r.rowid FROM rencs r JOIN msgs m ON m.id = r.msg WHERE r.st = ? AND m.chan = ?
		ORDER BY r.rowid LIMIT -1 OFFSET ?)`, reactPending, ch, maxPendingPerChan)
	q.Exec(`UPDATE msgs SET epend = NULL WHERE id IN (
		SELECT id FROM msgs WHERE chan = ? AND epend IS NOT NULL ORDER BY id LIMIT -1 OFFSET ?)`, ch, maxPendingPerChan)
}

// rebuildReacts sets row's shown reactions to its verified ones.
func (c *Client) rebuildReacts(q dbtx, row msgRow) bool {
	r := map[string][]uint64{}
	if rows, err := q.Query(`SELECT emoji, user FROM rencs WHERE msg = ? AND st = ? ORDER BY rowid`, row.id, reactOK); err == nil {
		for rows.Next() {
			var e string
			var u uint64
			if rows.Scan(&e, &u) == nil {
				r[e] = append(r[e], u)
			}
		}
		rows.Close()
	}
	res, err := q.Exec(`UPDATE msgs SET reactions = ? WHERE id = ? AND reactions != ?`, encodeReactions(r), row.id, encodeReactions(r))
	n, _ := rowsAffected(res)
	return err == nil && n > 0
}

// applyReactEvent applies a react.add/react.remove event on a message of
// an encrypted channel. Adds show only once their enc verifies; tags
// without a verified enc are ignored. A remove needs no proof: the server
// can always withhold a reaction anyway.
func (c *Client) applyReactEvent(q dbtx, row msgRow, ev proto.Event, d proto.Reaction, me uint64) error {
	if ev.Kind == proto.KReactRemove {
		var emoji string
		var st int
		q.QueryRow(`SELECT emoji, st FROM rencs WHERE msg = ? AND user = ? AND tag = ?`, row.id, ev.Actor, d.Emoji).Scan(&emoji, &st)
		if _, err := q.Exec(`DELETE FROM rencs WHERE msg = ? AND user = ? AND tag = ?`, row.id, ev.Actor, d.Emoji); err != nil {
			return err
		}
		c.rebuildReacts(q, row)
		if st == reactOK {
			return noteReaction(q, proto.Reaction{Msg: row.id, Emoji: emoji}, ev, me)
		}
		return nil
	}
	if d.Enc == nil {
		c.reject(row.id, "reaction without enc")
		return nil
	}
	st, emoji := c.judgeReact(q, row, ev.Actor, d.Emoji, d.Enc, ev.Seq, nil)
	if _, err := q.Exec(`INSERT INTO rencs (msg, user, tag, enc, emoji, st) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (msg, user, tag) DO UPDATE SET enc = excluded.enc, emoji = excluded.emoji, st = excluded.st`,
		row.id, ev.Actor, d.Emoji, d.Enc, emoji, st); err != nil {
		return err
	}
	if st == reactPending {
		c.trimPending(q, row.ch)
	}
	c.rebuildReacts(q, row)
	if st == reactOK {
		return noteReaction(q, proto.Reaction{Msg: row.id, Emoji: emoji}, ev, me)
	}
	return nil
}

// reopenOps retries what waited for a key or a device: pending edits and
// reactions of messages that are verified now. Caller holds e2eMu.
func (c *Client) reopenOps(memo *keyMemo, u *Update) {
	var ids []uint64
	if rows, err := c.db.Query(`SELECT id FROM msgs WHERE epend IS NOT NULL AND sec = ? ORDER BY id LIMIT 1000`, SecE2E); err == nil {
		for rows.Next() {
			var id uint64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
	}
	for _, id := range ids {
		if row, ok := getMsgRow(c.db, id); ok && c.applyEdit(c.db, row, row.epend, 0, memo) {
			u.msgs(row.ch)
			u.Channels = true
		}
	}
	ids = ids[:0]
	if rows, err := c.db.Query(`SELECT DISTINCT r.msg FROM rencs r JOIN msgs m ON m.id = r.msg
		WHERE r.st = ? AND m.sec != ? ORDER BY r.msg LIMIT 1000`, reactPending, SecWaiting); err == nil {
		for rows.Next() {
			var id uint64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
	}
	for _, id := range ids {
		if c.refreshReacts(c.db, id, memo) {
			if row, ok := getMsgRow(c.db, id); ok {
				u.msgs(row.ch)
			}
		}
	}
}

// ---- sending ----

// ErrNotEditable: only our own verified encrypted messages can get an
// encrypted edit, delete or reaction (the rest have no mid to refer to, or
// we can't vouch for them).
var ErrNotEditable = errors.New("this encrypted message can't be changed from here")

// errNotLoaded: the message isn't in the cache, so neither is what we must
// know about its channel (encrypted? in alarm?) before acting on it.
var errNotLoaded = proto.Errf(proto.ErrNotFound, "message not loaded")

// editEnc edits our message row with an encrypted edit (§13.4): sealed
// with the channel's current key like a send, retried after a stale_epoch.
func (c *Client) editEnc(ctx context.Context, row msgRow, body string) error {
	if !validText(body) {
		return proto.Errf(proto.ErrBadRequest, "message is empty or contains control characters")
	}
	if row.author != c.Me().ID || row.deleted || row.sec != SecE2E || row.mid == nil {
		return ErrNotEditable
	}
	if err := c.sendAllowed(row.ch); err != nil {
		return err
	}
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return ErrOffline
	}
	if !s.e2e() {
		return errors.New("the server stopped offering end-to-end encryption")
	}
	raw, err := proto.Marshal(plainEdit{T: "edit", TS: time.Now().UnixMilli(), Body: body, Ref: row.mid, V: row.ev + 1})
	if err != nil {
		return err
	}
	var prev []byte
	for attempt := 0; ; attempt++ {
		enc, err := c.sealWith(ctx, s, row.ch, prev, func(sid e2ecrypto.ServerID, dev *e2ecrypto.DeviceKeys, ep trust.Epoch) ([]byte, error) {
			if ep.Key == nil {
				return nil, failErr{noKeyReason}
			}
			return e2ecrypto.Encrypt(rand.Reader, *ep.Key, dev.Ed25519, sid, row.ch, ep.Epoch,
				e2ecrypto.EpochHash(ep.Hash[:e2ecrypto.IDSize]), dev.ID, raw)
		})
		if err != nil {
			return err
		}
		_, err = s.call(ctx, proto.OpEdit, proto.EditArgs{Msg: row.id, Enc: enc}, nil)
		var pe *proto.Error
		if attempt < 2 && errors.As(err, &pe) && pe.Code == proto.ErrStaleEpoch {
			prev = enc
			continue
		}
		return err
	}
}

// sealForMsg seals Plain p under the epoch of row's own enc with this
// device's key (deletes and reactions, §13.4).
func (c *Client) sealForMsg(row msgRow, p any) ([]byte, e2ecrypto.Key, error) {
	var k e2ecrypto.Key
	sid, ok := c.cacheServerID()
	if !ok {
		return nil, k, ErrNotEditable
	}
	orig, err := e2ecrypto.ParseEnc(row.enc)
	if err != nil {
		return nil, k, ErrNotEditable
	}
	ep, ok, err := c.trust.Epoch(sid, row.ch, orig.Epoch)
	if err != nil || !ok || ep.Key == nil {
		return nil, k, errors.New(noKeyReason)
	}
	dev, err := c.trust.DeviceKeys(sid, c.Me().ID)
	if err != nil {
		return nil, k, fmt.Errorf("this device has no encryption keys: %w", err)
	}
	raw, err := proto.Marshal(p)
	if err != nil {
		return nil, k, err
	}
	enc, err := e2ecrypto.Encrypt(rand.Reader, *ep.Key, dev.Ed25519, sid, row.ch, orig.Epoch, orig.EH, dev.ID, raw)
	return enc, *ep.Key, err
}

// deleteArgs is the delete op for msg: signed when it's our verified
// encrypted message.
func (c *Client) deleteArgs(msg uint64) (proto.DeleteArgs, error) {
	a := proto.DeleteArgs{Msg: msg}
	row, ok := getMsgRow(c.db, msg)
	if !ok {
		return a, errNotLoaded // its channel (alarm, encryption) is unknown
	}
	if c.needsEnc(row.ch) {
		// No deletes in alarm, like sends; but allowed during a pending key
		// change: a delete only removes, and seals to nobody new.
		if err := c.alarmAllowed(row.ch); err != nil {
			return a, err
		}
	}
	if row.deleted || row.author != c.Me().ID || row.mid == nil || !c.needsEnc(row.ch) {
		return a, nil // someone else's (admin), or plaintext: unsigned
	}
	if row.sec != SecE2E {
		return a, ErrNotEditable
	}
	enc, _, err := c.sealForMsg(row, plainDel{T: "del", TS: time.Now().UnixMilli(), Ref: row.mid})
	a.Enc = enc
	return a, err
}

// reactArgs is the react/unreact op for emoji on msg: in an encrypted
// channel, the tag and a signed enc carrying the emoji.
func (c *Client) reactArgs(msg uint64, emoji string, add bool) (proto.ReactArgs, error) {
	a := proto.ReactArgs{Msg: msg, Emoji: emoji}
	row, ok := getMsgRow(c.db, msg)
	if !ok {
		// We can't tell which channel it is in, so whether the emoji may go
		// in the clear: refuse, like edits of uncached messages.
		return a, errNotLoaded
	}
	if !c.needsEnc(row.ch) {
		return a, nil
	}
	if err := c.sendAllowed(row.ch); err != nil {
		return a, err // no reactions in alarm, like sends
	}
	if row.sec != SecE2E || row.mid == nil || row.deleted {
		return a, ErrNotEditable
	}
	if !validReactEmoji(emoji) {
		return a, proto.Errf(proto.ErrBadRequest, "invalid emoji")
	}
	enc, k, err := c.sealForMsg(row, plainReact{T: "react", TS: time.Now().UnixMilli(), Ref: row.mid, Emoji: emoji, Add: add})
	if err != nil {
		return a, err
	}
	sid, _ := c.cacheServerID()
	orig, _ := e2ecrypto.ParseEnc(row.enc)
	if a.Emoji, err = e2ecrypto.ReactTag(k, sid, row.ch, orig.Epoch, e2ecrypto.MsgID(row.mid), emoji); err != nil {
		return a, err
	}
	a.Enc = enc
	return a, nil
}
