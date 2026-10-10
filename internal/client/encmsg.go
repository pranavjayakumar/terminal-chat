package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/trust"
)

// Encrypted messages (PROTOCOL.md §13.4; docs/E2E.md step 7).
//
// The cache keeps a message's enc with the body it decrypted to and a Sec
// verdict. It is therefore as sensitive as the conversation itself (so is
// the outbox, which keeps what we typed until the server has it): protect
// TC_HOME like the messages, e.g. with disk encryption.

// Sec is how far a cached message can be trusted.
type Sec uint8

const (
	// SecPlain: a message in a channel that isn't encrypted.
	SecPlain Sec = iota
	// SecE2E: decrypted and verified: signed by a verified device of its
	// author, for its thread, valid text.
	SecE2E
	// SecWaiting: ciphertext we can't judge yet (no key for its epoch, its
	// device or thread root unknown); decrypted when they arrive.
	SecWaiting
	// SecInvalid: failed a check (forged, replayed, moved, control
	// characters). Never shown as text.
	SecInvalid
	// SecLegacy: plaintext in an encrypted channel that this client had
	// cached before it first saw the channel encrypted (trust store legacy
	// cutoff). Unauthenticated.
	SecLegacy
	// SecInserted: any other plaintext in an encrypted channel: only the
	// server can have put it there. Untrusted.
	SecInserted
	// SecRemoved: a deleted message of an encrypted channel whose delete
	// its author didn't sign (an admin, or the server): "removed (not by
	// author)". A delete the author signed keeps SecE2E.
	SecRemoved
	// SecLegacyClaimed: legacy by the server's Channel.legacy (this device
	// cached none of the channel's plaintext when it first saw it
	// encrypted), not by what it witnessed. Unauthenticated.
	SecLegacyClaimed
)

func (s Sec) String() string {
	return [...]string{"plain", "e2e", "waiting", "invalid", "legacy", "inserted", "removed", "legacy-claimed"}[s]
}

// trustedSecs is Trusted() as an SQL list: (SecPlain, SecE2E).
var trustedSecs = fmt.Sprintf("(%d, %d)", SecPlain, SecE2E)

// Trusted reports whether the message's text may be treated as its
// author's (notifications, mentions). Plaintext in an encrypted channel,
// legacy or not, is unauthenticated.
func (s Sec) Trusted() bool { return s == SecPlain || s == SecE2E }

// noKeyReason is the outbox failure when we can't get the channel's key;
// such messages are requeued when the key arrives.
const noKeyReason = "no key yet — waiting for a member"

// rotateReason is the outbox failure when the channel needs a new key and
// we couldn't make one (retry by hand).
const rotateReason = "couldn't start a new key for this channel — retry"

// ErrE2EAlarm refuses sends into an encrypted channel in alarm.
var ErrE2EAlarm = errors.New("sending is blocked: this channel's encryption is in alarm")

// plainMsg is Plain (§13.4) for t = "msg".
type plainMsg struct {
	T         string `msgpack:"t"`
	TS        int64  `msgpack:"ts"`
	Body      string `msgpack:"body"`
	ThreadRef []byte `msgpack:"thread_ref,omitempty"`
}

var zeroRef = make([]byte, e2ecrypto.IDSize)

// validText is §10 for a decrypted body.
func validText(s string) bool {
	return len(s) <= proto.MaxBody && strings.TrimSpace(s) != "" && sanitize.Valid(s)
}

// ---- receiving ----

// keyMemo caches trust lookups for a batch of messages.
type keyMemo struct {
	epochs map[[2]uint64]*trust.Epoch
	devs   map[e2ecrypto.DevID]*trust.Device
}

func (k *keyMemo) epoch(ts *trust.Store, sid e2ecrypto.ServerID, ch uint64, ep uint32) *trust.Epoch {
	key := [2]uint64{ch, uint64(ep)}
	if k != nil {
		if e, ok := k.epochs[key]; ok {
			return e
		}
	}
	var out *trust.Epoch
	if e, ok, err := ts.Epoch(sid, ch, ep); err == nil && ok {
		out = &e
	}
	if k != nil {
		k.epochs[key] = out
	}
	return out
}

func (k *keyMemo) device(ts *trust.Store, sid e2ecrypto.ServerID, dev e2ecrypto.DevID) *trust.Device {
	if k != nil {
		if d, ok := k.devs[dev]; ok {
			return d
		}
	}
	var out *trust.Device
	if d, ok, err := ts.Device(sid, dev); err == nil && ok {
		out = &d
	}
	if k != nil {
		k.devs[dev] = out
	}
	return out
}

func newKeyMemo() *keyMemo {
	return &keyMemo{epochs: map[[2]uint64]*trust.Epoch{}, devs: map[e2ecrypto.DevID]*trust.Device{}}
}

// qServerID is the cached server id as seen by q (inside a reset
// transaction it is already the new one).
func qServerID(q dbtx) (e2ecrypto.ServerID, bool) {
	var sid e2ecrypto.ServerID
	b, err := metaBytes(q, "server_id")
	if err != nil || len(b) != e2ecrypto.IDSize {
		return sid, false
	}
	copy(sid[:], b)
	return sid, true
}

// chanIsE2E: the cached channel is encrypted, or the trust store saw it so.
func (c *Client) chanIsE2E(q dbtx, sid e2ecrypto.ServerID, hasSID bool, ch uint64) bool {
	if cv, ok, err := getChan(q, ch); err == nil && ok && cv.E2E {
		return true
	}
	if !hasSID {
		return false
	}
	was, _ := c.trust.ChannelE2E(sid, ch)
	return was
}

// legacyCutoff is ch's legacy cutoff (trust store, set once: the newest
// message id cached when we first saw ch encrypted; with nothing cached,
// the server's Channel.legacy, its claim of where chan.encrypt converted
// it). ok is false without a server id.
func (c *Client) legacyCutoff(q dbtx, ch uint64) (uint64, bool) {
	cut, _, ok := c.legacyInfo(q, ch)
	return cut, ok
}

// legacyInfo is legacyCutoff and whether the cutoff is the server's claim.
// A claim counts only up to the seq of the channel's verified epoch 1
// (honest: the conversion's last message precedes its epoch 1); above it,
// or before epoch 1 is verified, it counts as 0.
func (c *Client) legacyInfo(q dbtx, ch uint64) (cut uint64, claimed, ok bool) {
	sid, ok := qServerID(q)
	if !ok {
		return 0, false, false
	}
	var newest uint64
	if err := q.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM msgs WHERE chan = ?`, ch).Scan(&newest); err != nil {
		return 0, false, false
	}
	claim := false
	if newest == 0 {
		// Only a device that saw none of the plaintext takes the server's
		// word: one that watched the conversion knows the newest message
		// itself, because events (msg.new before chan.update) apply in order.
		_ = q.QueryRow(`SELECT legacy FROM chans WHERE id = ?`, ch).Scan(&newest)
		claim = newest > 0
	}
	cut, claimed, err := c.trust.LegacyCutoffAs(sid, ch, newest, claim)
	if err != nil {
		return 0, false, false
	}
	if claimed {
		ep1, ok, err := c.trust.Epoch(sid, ch, 1)
		if err != nil || !ok || ep1.Seq == 0 || cut > ep1.Seq {
			return 0, false, true
		}
	}
	return cut, claimed, true
}

// legacySec is the Sec of plaintext at or below a cutoff.
func legacySec(claimed bool) Sec {
	if claimed {
		return SecLegacyClaimed
	}
	return SecLegacy
}

// noteE2EChan records the legacy cutoff of an encrypted channel the moment
// the client first sees it so (no-op afterwards).
func (c *Client) noteE2EChan(q dbtx, ch proto.Channel) {
	if ch.E2E {
		c.legacyCutoff(q, ch.ID)
	}
}

// storeChanE2E stores a channel from the server and, the first time it is
// seen encrypted, records its legacy cutoff.
func (c *Client) storeChanE2E(q dbtx, ch proto.Channel) error {
	old, had, err := getChan(q, ch.ID)
	if err != nil {
		return err
	}
	if err := storeChan(q, ch); err != nil {
		return err
	}
	c.noteE2EChan(q, ch)
	if ch.E2E && had && !old.E2E {
		// Converted (chan.encrypt): the plaintext we cached is legacy from
		// this event on, not only once the e2e worker reclassifies it.
		if cut, claimed, ok := c.legacyInfo(q, ch.ID); ok {
			if _, err := q.Exec(`UPDATE msgs SET sec = ? WHERE chan = ? AND enc IS NULL AND deleted = 0 AND sec = ? AND id <= ?`,
				legacySec(claimed), ch.ID, SecPlain, cut); err != nil {
				return err
			}
		}
	}
	return nil
}

// plainSec classifies plaintext message id in encrypted channel ch: at or
// below the legacy cutoff it predates encryption, above it only the server
// can have put it there. Neither is authenticated.
func (c *Client) plainSec(q dbtx, ch, id uint64) Sec {
	if cut, claimed, ok := c.legacyInfo(q, ch); ok && id <= cut {
		return legacySec(claimed)
	}
	return SecInserted
}

// prepMsg judges a message from the server (§13.4 "Receiving"): it
// decrypts m.Enc into m.Body and returns the verdict and the enc's mid (nil
// without enc). It only reads q (and sets a channel's legacy cutoff).
func (c *Client) prepMsg(q dbtx, m *proto.Message, memo *keyMemo) (Sec, []byte) {
	sec, mid, _ := c.judge(q, m, memo)
	return sec, mid
}

// judge is prepMsg; keep reports that the cached row must stay as it is: a
// copy that tries to change a verified message's channel, author, thread or
// bytes is invalid, not an update.
func (c *Client) judge(q dbtx, m *proto.Message, memo *keyMemo) (sec Sec, mid []byte, keep bool) {
	sid, hasSID := qServerID(q)
	if m.Enc == nil {
		if m.Deleted || !c.chanIsE2E(q, sid, hasSID, m.Chan) {
			return SecPlain, nil, false
		}
		return c.plainSec(q, m.Chan, m.ID), nil, false
	}
	sec, mid = c.judgeEnc(q, sid, hasSID, m, memo)
	if sec == SecInvalid {
		m.Body = ""
	}
	var old struct {
		enc              []byte
		sec              Sec
		ch, author, thrd uint64
	}
	if q.QueryRow(`SELECT enc, sec, chan, author, thread FROM msgs WHERE id = ?`, m.ID).
		Scan(&old.enc, &old.sec, &old.ch, &old.author, &old.thrd) == nil && old.sec == SecE2E &&
		(old.ch != m.Chan || old.author != m.Author || old.thrd != m.Thread || !bytes.Equal(old.enc, m.Enc)) {
		m.Body = ""
		return SecInvalid, mid, true
	}
	return sec, mid, false
}

func (c *Client) judgeEnc(q dbtx, sid e2ecrypto.ServerID, hasSID bool, m *proto.Message, memo *keyMemo) (Sec, []byte) {
	body := m.Body
	m.Body = ""
	if _, err := e2ecrypto.ParseEnc(m.Enc); err != nil || body != "" {
		return SecInvalid, nil
	}
	mid := e2ecrypto.MID(m.Enc)
	// The same bytes judged before, for the same message, keep their
	// verdict (history refetches).
	var oldEnc []byte
	var oldSec Sec
	var oldBody string
	var oldCh, oldAuthor, oldThread uint64
	if q.QueryRow(`SELECT enc, sec, body, chan, author, thread FROM msgs WHERE id = ?`, m.ID).
		Scan(&oldEnc, &oldSec, &oldBody, &oldCh, &oldAuthor, &oldThread) == nil &&
		bytes.Equal(oldEnc, m.Enc) && oldSec != SecWaiting &&
		oldCh == m.Chan && oldAuthor == m.Author && oldThread == m.Thread {
		m.Body = oldBody
		return oldSec, mid[:]
	}
	// First mid wins: the server can't replay an enc as a second message.
	var other uint64
	if q.QueryRow(`SELECT id FROM msgs WHERE mid = ? AND id < ? LIMIT 1`, mid[:], m.ID).Scan(&other) == nil {
		return SecInvalid, mid[:]
	}
	if !hasSID {
		return SecWaiting, mid[:]
	}
	raw, d, sec, _ := c.openEnc(sid, m.Chan, m.Author, m.ID, m.Enc, memo)
	if sec != SecE2E {
		return sec, mid[:]
	}
	var p plainMsg
	if proto.Unmarshal(raw, &p) != nil || p.T != "msg" || !validText(p.Body) || revokedAt(d, p.TS) {
		return SecInvalid, mid[:]
	}
	if m.Thread == 0 {
		if p.ThreadRef != nil {
			return SecInvalid, mid[:]
		}
	} else {
		var rootMid []byte
		err := q.QueryRow(`SELECT mid FROM msgs WHERE id = ? AND chan = ?`, m.Thread, m.Chan).Scan(&rootMid)
		if errors.Is(err, sql.ErrNoRows) {
			return SecWaiting, mid[:] // judged once the root is cached
		}
		if rootMid == nil {
			rootMid = zeroRef
		}
		if err != nil || !bytes.Equal(p.ThreadRef, rootMid) {
			return SecInvalid, mid[:]
		}
	}
	m.Body = p.Body
	return SecE2E, mid[:]
}

// saveMsg stores a message from the server after judging it; m.Body is the
// text to show (decrypted, or "" when it can't be shown).
func (c *Client) saveMsg(q dbtx, m *proto.Message, replace bool, memo *keyMemo) (Sec, error) {
	old, hadOld := getMsgRow(q, m.ID)
	if sid, ok := qServerID(q); m.Deleted && ((hadOld && old.mid != nil) || c.chanIsE2E(q, sid, ok, m.Chan)) {
		return c.saveDeleted(q, m, old, hadOld, memo)
	}
	if hadOld && old.enc != nil && m.Enc == nil {
		// A copy without the enc can't replace an encrypted message (it
		// would turn verified text into the server's): keep ours.
		return old.sec, nil
	}
	editEnc, renc := m.EditEnc, m.Renc
	sec, mid, keep := c.judge(q, m, memo)
	if sec == SecInvalid || sec == SecWaiting {
		m.Body = ""
	}
	if keep {
		return sec, nil // the verified copy stays
	}
	encrypted := m.Enc != nil
	same := encrypted && hadOld && bytes.Equal(old.enc, m.Enc)
	if encrypted {
		// Only verified edits and reactions show (encops.go): not the
		// server's edited time, not its reaction tags.
		m.Reactions, m.Edited = nil, 0
		if same && old.ev > 0 {
			m.Edited = old.edited
		}
	}
	if err := storeMsg(q, *m, sec, mid, replace); err != nil {
		return sec, err
	}
	if mid != nil {
		// Lowest id wins whatever the arrival order: a copy of this enc
		// under a later id is a replay.
		if _, err := q.Exec(`UPDATE msgs SET sec = ?, body = '' WHERE mid = ? AND id > ? AND sec != ?`,
			SecInvalid, mid, m.ID, SecInvalid); err != nil {
			return sec, err
		}
	}
	if encrypted {
		if same {
			// The edit state of the stored copy survives the refetch.
			if _, err := q.Exec(`UPDATE msgs SET ev = ?, ets = ?, eenc = ?, epend = ? WHERE id = ?`,
				old.ev, old.ets, old.eenc, old.epend, m.ID); err != nil {
				return sec, err
			}
		}
		if row, ok := getMsgRow(q, m.ID); ok {
			if editEnc != nil && !bytes.Equal(editEnc, row.eenc) {
				c.applyEdit(q, row, editEnc, 0, memo)
			}
			// An edit that waited for this message to open.
			if row, ok := getMsgRow(q, m.ID); ok && row.epend != nil && row.sec == SecE2E {
				c.applyEdit(q, row, row.epend, 0, memo)
			}
			if replace {
				if err := syncRencs(q, m.ID, renc); err != nil {
					return sec, err
				}
			}
			c.refreshReacts(q, m.ID, memo)
			if sec == SecWaiting {
				c.trimPending(q, m.Chan)
			}
			if row, ok := getMsgRow(q, m.ID); ok && sec == SecE2E {
				m.Body, m.Edited = row.body, row.edited
			}
		}
	}
	if sec == SecWaiting {
		c.kickE2E() // the key may be arriving right now
	}
	return sec, nil
}

// reopenMsgs is the lazy decrypt queue: it re-judges cached messages that
// were waiting (for a key, a device or a thread root) and reclassifies
// plaintext in encrypted channels. Messages that became readable go to
// Update.New if they are unread. Caller holds e2eMu.
func (c *Client) reopenMsgs(sid e2ecrypto.ServerID, chans []proto.Channel) {
	memo := newKeyMemo()
	var u Update
	for _, ch := range chans {
		// Plaintext rows cached before the channel was known as encrypted
		// are plain: classify them against the legacy cutoff.
		cut, claimed, ok := c.legacyInfo(c.db, ch.ID)
		if !ok {
			continue
		}
		res, _ := c.db.Exec(`UPDATE msgs SET sec = CASE WHEN id <= ?1 THEN ?2 ELSE ?3 END
			WHERE chan = ?4 AND enc IS NULL AND deleted = 0 AND sec IN (?5, ?3, ?6, ?7)
			AND sec != CASE WHEN id <= ?1 THEN ?2 ELSE ?3 END`, cut, legacySec(claimed), SecInserted, ch.ID, SecPlain, SecLegacy, SecLegacyClaimed)
		if n, _ := rowsAffected(res); n > 0 {
			u.msgs(ch.ID)
		}
	}
	rows, err := c.db.Query(`SELECT `+msgCols+`, enc, denc, COALESCE(mid, cmid) FROM msgs WHERE sec = ? ORDER BY id LIMIT 1000`, SecWaiting)
	if err != nil {
		return
	}
	type waitRow struct {
		m         proto.Message
		denc, mid []byte
	}
	var waiting []waitRow
	for rows.Next() {
		var w waitRow
		m, err := scanMsg(rows, &w.m.Enc, &w.denc, &w.mid)
		if err == nil {
			m.Enc = w.m.Enc
			w.m = m
			waiting = append(waiting, w)
		}
	}
	rows.Close()
	reads := c.uintMap(`SELECT chan, seq FROM reads`)
	me := c.Me().ID
	for _, w := range waiting {
		m := w.m
		if m.Deleted {
			// A delete we couldn't judge yet: by its author or not.
			sec := c.deleteVerdict(c.db, sid, true, m.Chan, m.ID, m.Author, w.mid, w.denc, 0, memo)
			if sec == SecWaiting {
				continue
			}
			if _, err := c.db.Exec(`UPDATE msgs SET sec = ? WHERE id = ? AND sec = ?`, sec, m.ID, SecWaiting); err == nil {
				u.msgs(m.Chan)
			}
			continue
		}
		m.Body = ""
		sec, _ := c.prepMsg(c.db, &m, memo)
		if sec == SecWaiting {
			continue
		}
		body := m.Body
		if sec != SecE2E {
			body = ""
		}
		res, err := c.db.Exec(`UPDATE msgs SET body = ?, sec = ? WHERE id = ? AND sec = ?`, body, sec, m.ID, SecWaiting)
		if n, _ := rowsAffected(res); err != nil || n == 0 {
			continue
		}
		u.msgs(m.Chan)
		u.Channels = true
		if m.ID > reads[m.Chan] && m.Author != me {
			m.Enc, m.Body = nil, body
			u.New = append(u.New, Message{Message: m, Sec: sec})
		}
	}
	c.reopenOps(memo, &u)
	c.post(u)
}

func rowsAffected(r sql.Result) (int64, error) {
	if r == nil {
		return 0, nil
	}
	return r.RowsAffected()
}

// ---- sending ----

// needsEnc reports whether sends into ch must be encrypted: the cache says
// it's encrypted or the trust store once saw it so.
func (c *Client) needsEnc(ch uint64) bool {
	sid, ok := c.cacheServerID()
	return c.chanIsE2E(c.db, sid, ok, ch)
}

// sendAllowed refuses sends into an encrypted channel in alarm or with a
// member's unacknowledged key change.
func (c *Client) sendAllowed(ch uint64) error {
	if !c.needsEnc(ch) {
		return nil
	}
	if err := c.alarmAllowed(ch); err != nil {
		return err
	}
	return c.keyChangeAllowed(ch)
}

// alarmAllowed refuses anything into an encrypted channel in alarm. Deletes
// check only this: they never seal to a changed key (docs/E2E.md step 10).
func (c *Client) alarmAllowed(ch uint64) error {
	if st := c.E2EStatus(ch); st.State == E2EAlarm && !unconfirmedAlarm(st) {
		return fmt.Errorf("%w (%s)", ErrE2EAlarm, sanitize.Line(st.Detail))
	}
	return nil
}

// failErr marks an outbox entry failed with reason (no server involved).
type failErr struct{ reason string }

func (e failErr) Error() string { return e.reason }

// threadRef is thread_ref for a reply to root: the root's mid, or zeros for
// a root without enc. The root must be cached.
func (c *Client) threadRef(ch, root uint64) ([]byte, error) {
	var mid []byte
	err := c.db.QueryRow(`SELECT mid FROM msgs WHERE id = ? AND chan = ?`, root, ch).Scan(&mid)
	if err != nil {
		return nil, failErr{"the thread's first message isn't loaded"}
	}
	if mid == nil {
		return zeroRef, nil
	}
	return mid, nil
}

// seal encrypts an outbox entry with epoch ep's key. encNonce makes it
// reproducible: the same entry, epoch and nonce give the same bytes.
func (c *Client) seal(sid e2ecrypto.ServerID, dev *e2ecrypto.DeviceKeys, ep trust.Epoch, e outboxEntry) ([]byte, error) {
	if ep.Key == nil {
		return nil, failErr{noKeyReason}
	}
	p := plainMsg{T: "msg", TS: e.time, Body: e.body}
	if e.thread != 0 {
		ref, err := c.threadRef(e.chan_, e.thread)
		if err != nil {
			return nil, err
		}
		p.ThreadRef = ref
	}
	raw, err := proto.Marshal(p)
	if err != nil {
		return nil, err
	}
	return e2ecrypto.Encrypt(bytes.NewReader(e.encNonce), *ep.Key, dev.Ed25519, sid, e.chan_, ep.Epoch,
		e2ecrypto.EpochHash(ep.Hash[:e2ecrypto.IDSize]), dev.ID, raw)
}

// rosterNow is who a fresh bundle of ch seals to: its current
// non-disabled members, sorted, and — once the channel has a verified
// epoch — only those that epoch includes. The member list is the
// server's word; a verified epoch is a member's (§13.3: the rotator was in
// the previous roster), so a user the server alone added (an unsigned
// chan.join; honest adds carry the next epoch) is unconfirmed and never
// sealed to. Before epoch 1 the member list is all there is: its creator
// builds epoch 1 from it.
func (c *Client) rosterNow(ch proto.Channel) []uint64 {
	r := c.liveMembers(ch)
	if head, ok := c.headRoster(ch.ID); ok {
		r = slices.DeleteFunc(r, func(u uint64) bool { return !slices.Contains(head, u) })
	}
	return r
}

// liveMembers is ch's non-disabled members, sorted.
func (c *Client) liveMembers(ch proto.Channel) []uint64 {
	c.mu.Lock()
	var r []uint64
	for _, u := range ch.Members {
		if !c.users[u].Disabled {
			r = append(r, u)
		}
	}
	c.mu.Unlock()
	slices.Sort(r)
	return slices.Compact(r)
}

// headRoster is the roster of ch's verified head; ok is false before the
// channel has one.
func (c *Client) headRoster(ch uint64) ([]uint64, bool) {
	sid, ok := c.cacheServerID()
	if !ok {
		return nil, false
	}
	head, ok, err := c.trust.ChainHead(sid, ch)
	if err != nil || !ok {
		return nil, false
	}
	ep, ok, err := c.trust.Epoch(sid, ch, head.Epoch)
	if err != nil || !ok {
		return nil, false
	}
	return ep.Roster, true
}

// unconfirmedPrefix starts the E2EStatus alarm for members no verified
// epoch includes.
const unconfirmedPrefix = "membership not confirmed by a member: "

// unconfirmed lists ch's live members that its verified head doesn't
// include (none before epoch 1).
func (c *Client) unconfirmed(sid e2ecrypto.ServerID, ch proto.Channel) []uint64 {
	head, ok := c.headRoster(ch.ID)
	if !ok {
		return nil
	}
	return slices.DeleteFunc(c.liveMembers(ch), func(u uint64) bool { return slices.Contains(head, u) })
}

// unconfirmedAlarm: st is the alarm for unconfirmed members, which also
// shows for a moment between an honest add's chan.join and its key.epoch,
// so senders wait a little before failing on it.
func unconfirmedAlarm(st E2EStatus) bool {
	return st.State == E2EAlarm && strings.HasPrefix(st.Detail, unconfirmedPrefix)
}

// sealOffline encrypts a new outbox entry right away when this device holds
// the key of the channel's verified head and the head isn't visibly stale;
// otherwise the outbox encrypts it before the first send. No network.
func (c *Client) sealOffline(e outboxEntry) []byte {
	sid, ok := c.cacheServerID()
	if !ok {
		return nil
	}
	ch, ok, err := getChan(c.db, e.chan_)
	if err != nil || !ok {
		return nil
	}
	head, ok, err := c.trust.ChainHead(sid, e.chan_)
	if err != nil || !ok || head.Epoch < ch.Epoch {
		return nil
	}
	ep, ok, err := c.trust.Epoch(sid, e.chan_, head.Epoch)
	if err != nil || !ok || ep.Key == nil || !slices.Equal(ep.Roster, c.liveMembers(ch)) {
		return nil
	}
	dev, err := c.trust.DeviceKeys(sid, c.Me().ID)
	if err != nil {
		return nil
	}
	enc, err := c.seal(sid, dev, ep, e)
	if err != nil {
		return nil
	}
	return enc
}

// encCurrent reports whether a queued entry's enc is still for the
// channel's verified head, whose roster is still the current members.
func (c *Client) encCurrent(e outboxEntry) bool {
	h, err := e2ecrypto.ParseEnc(e.enc)
	if err != nil {
		return false
	}
	sid, ok := c.cacheServerID()
	if !ok {
		return false
	}
	ch, ok, err := getChan(c.db, e.chan_)
	if err != nil || !ok {
		return false
	}
	head, ok, err := c.trust.ChainHead(sid, e.chan_)
	if err != nil || !ok || head.Epoch != h.Epoch || head.EH != h.EH {
		return false
	}
	ep, ok, err := c.trust.Epoch(sid, e.chan_, head.Epoch)
	return err == nil && ok && ep.Key != nil && slices.Equal(ep.Roster, c.liveMembers(ch)) // not rosterNow: an unconfirmed member must reach sealWith
}

// sealEntry encrypts an outbox entry for sending (§13.4 "Sending"): it
// catches up on the channel's chain, rotates first when the bundle is
// stale (none yet, the roster changed, or after a stale_epoch the server
// still calls the epoch we used stale), and encrypts with the head's key.
// prev is the enc a stale_epoch answered (nil the first time).
func (c *Client) sealEntry(ctx context.Context, s *session, e outboxEntry, prev []byte) ([]byte, error) {
	return c.sealWith(ctx, s, e.chan_, prev, func(sid e2ecrypto.ServerID, dev *e2ecrypto.DeviceKeys, ep trust.Epoch) ([]byte, error) {
		return c.seal(sid, dev, ep, e)
	})
}

// sealWith is sealEntry for anything sealed with channel chID's current
// key (messages, edits): seal encrypts with the verified head's epoch ep.
func (c *Client) sealWith(ctx context.Context, s *session, chID uint64, prev []byte,
	seal func(e2ecrypto.ServerID, *e2ecrypto.DeviceKeys, trust.Epoch) ([]byte, error)) ([]byte, error) {
	sid, ok := s.serverID()
	if !ok {
		return nil, failErr{"the server has no valid id"}
	}
	wait := time.NewTimer(c.timing.devWait)
	defer wait.Stop()
	select {
	case <-s.devReady:
	case <-s.done:
		return nil, ErrOffline
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-wait.C:
		return nil, failErr{"this device isn't registered for encryption yet"}
	}
	dev := s.myDevice()
	if dev == nil {
		return nil, failErr{"this device isn't registered for encryption yet"}
	}
	c.e2eMu.Lock()
	defer c.e2eMu.Unlock()
	me := c.Me().ID
	prevEpoch := uint32(0)
	if prev != nil {
		if h, err := e2ecrypto.ParseEnc(prev); err == nil {
			prevEpoch = h.Epoch
		}
	}
	// Events keep arriving while we look (a rival's first bundle, a device
	// it was signed by): when the chain is behind what the cache shows,
	// catch up again a few times before calling it "no key".
	const attempts = 4
	behind := func(attempt int) error {
		if attempt == attempts-1 {
			return failErr{noKeyReason}
		}
		t := time.NewTimer(time.Duration(attempt+1) * 25 * time.Millisecond)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-s.done:
			return ErrOffline
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for attempt := 0; ; attempt++ {
		c.verifyDevices(sid, s.sshPub, me)
		ch, ok, err := getChan(c.db, chID)
		if err != nil {
			return nil, err
		}
		if !ok || !slices.Contains(ch.Members, me) {
			return nil, failErr{"you're not a member of this channel"}
		}
		c.syncChannel(ctx, s, sid, ch)
		if ch, _, err = getChan(c.db, chID); err != nil {
			return nil, err
		}
		st := c.e2eStatus(sid, ch)
		if unconfirmedAlarm(st) && attempt < attempts-1 {
			if err := behind(attempt); err != nil {
				return nil, err
			}
			continue
		}
		if st.State == E2EAlarm {
			return nil, failErr{"encryption alarm: " + sanitize.Line(st.Detail)}
		}
		head, hasHead, err := c.trust.ChainHead(sid, ch.ID)
		if err != nil {
			return nil, err
		}
		latest := max(ch.Epoch, c.cachedMaxEpoch(ch.ID))
		rotate := false
		switch {
		case latest > head.Epoch: // a chain we can't verify (yet)
			if err := behind(attempt); err != nil {
				return nil, err
			}
			continue
		case !hasHead:
			rotate = true
		default:
			cur, _, _ := c.trust.Epoch(sid, ch.ID, head.Epoch)
			rotate = !slices.Equal(cur.Roster, c.rosterNow(ch)) || (prev != nil && head.Epoch == prevEpoch)
		}
		if rotate && attempt < attempts-1 {
			err := c.rotateOnce(ctx, s, sid, ch.ID)
			var pe *proto.Error
			var unv *UnverifiedMembersError
			switch {
			case err == nil:
				prev = nil // the new epoch is fresh
				continue
			case errors.As(err, &pe) && pe.Code == proto.ErrStaleEpoch:
				continue // someone else rotated: look again
			case errors.As(err, &pe) && pe.Code == proto.ErrRateLimited:
				return nil, err
			case errors.As(err, &unv):
				return nil, failErr{sanitize.Line(err.Error())}
			case errors.Is(err, ErrOffline) || ctx.Err() != nil:
				return nil, ErrOffline
			default:
				// Typically someone's bundle arrived meanwhile ("chain isn't
				// verified yet"), or we may not make epoch 1: look again.
				if err := behind(attempt); err != nil {
					return nil, err
				}
				continue
			}
		}
		if !hasHead {
			return nil, failErr{noKeyReason}
		}
		if rotate {
			// Still stale after every try: never seal to the old key.
			return nil, failErr{rotateReason}
		}
		ep, ok, err := c.trust.Epoch(sid, ch.ID, head.Epoch)
		if err != nil || !ok {
			return nil, failErr{noKeyReason}
		}
		return seal(sid, dev, ep)
	}
}

// requeueNoKey puts messages that failed for want of a key back in the
// outbox once ch's key is here.
func (c *Client) requeueNoKey(ch uint64) {
	res, err := c.db.Exec(`UPDATE outbox SET failed = '' WHERE chan = ? AND failed = ?`, ch, noKeyReason)
	if n, _ := rowsAffected(res); err == nil && n > 0 {
		c.outboxChanged(ch, nil)
	}
}

// newEncNonce is a fresh enc nonce for an outbox entry.
func newEncNonce() []byte {
	n := make([]byte, e2ecrypto.NonceSize)
	rand.Read(n)
	return n
}

// ---- local search ----

// searchLocal finds decrypted messages of encrypted channels in the cache
// (the server can't search them): every term, case-insensitively for
// ASCII, newest first.
func (c *Client) searchLocal(q string, ch uint64, limit int) []proto.Message {
	terms := strings.Fields(q)
	if len(terms) == 0 {
		return nil
	}
	if len(terms) > 8 {
		terms = terms[:8]
	}
	where := `sec = ? AND deleted = 0`
	args := []any{SecE2E}
	for _, t := range terms {
		where += ` AND body LIKE ? ESCAPE '\'`
		args = append(args, "%"+likeEscape(t)+"%")
	}
	if ch != 0 {
		where += ` AND chan = ?`
		args = append(args, ch)
	}
	args = append(args, limit)
	rows, err := c.db.Query(`SELECT `+msgCols+` FROM msgs WHERE `+where+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []proto.Message
	for rows.Next() {
		if m, err := scanMsg(rows); err == nil {
			out = append(out, m)
		}
	}
	return out
}
