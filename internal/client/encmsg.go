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
	// SecLegacy: plaintext in an encrypted channel from before its first
	// epoch (sent before encrypted sends existed).
	SecLegacy
	// SecInserted: plaintext in an encrypted channel after its first epoch:
	// only the server can have put it there. Untrusted.
	SecInserted
)

func (s Sec) String() string {
	return [...]string{"plain", "e2e", "waiting", "invalid", "legacy", "inserted"}[s]
}

// Trusted reports whether the message's text may be treated as its author's.
func (s Sec) Trusted() bool { return s == SecPlain || s == SecE2E || s == SecLegacy }

// noKeyReason is the outbox failure when we can't get the channel's key;
// such messages are requeued when the key arrives.
const noKeyReason = "no key yet — waiting for a member"

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

// plainSec classifies plaintext message id in encrypted channel ch: before
// the seq of its verified epoch 1 (or with none known) it is legacy,
// otherwise the server inserted it.
func (c *Client) plainSec(sid e2ecrypto.ServerID, ch, id uint64, memo *keyMemo) Sec {
	if e := memo.epoch(c.trust, sid, ch, 1); e != nil && e.Seq != 0 && id >= e.Seq {
		return SecInserted
	}
	return SecLegacy
}

// prepMsg judges a message from the server (§13.4 "Receiving"): it
// decrypts m.Enc into m.Body and returns the verdict and the enc's mid (nil
// without enc). It only reads q.
func (c *Client) prepMsg(q dbtx, m *proto.Message, memo *keyMemo) (Sec, []byte) {
	sid, hasSID := qServerID(q)
	if m.Enc == nil {
		if m.Deleted || !c.chanIsE2E(q, sid, hasSID, m.Chan) {
			return SecPlain, nil
		}
		return c.plainSec(sid, m.Chan, m.ID, memo), nil
	}
	body := m.Body
	m.Body = ""
	e, err := e2ecrypto.ParseEnc(m.Enc)
	if err != nil || body != "" {
		return SecInvalid, nil
	}
	mid := e2ecrypto.MID(m.Enc)
	// The same bytes judged before keep their verdict (history refetches).
	var oldEnc []byte
	var oldSec Sec
	var oldBody string
	if q.QueryRow(`SELECT enc, sec, body FROM msgs WHERE id = ?`, m.ID).Scan(&oldEnc, &oldSec, &oldBody) == nil &&
		bytes.Equal(oldEnc, m.Enc) && oldSec != SecWaiting {
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
	ep := memo.epoch(c.trust, sid, m.Chan, e.Epoch)
	if ep == nil || ep.Key == nil {
		return SecWaiting, mid[:]
	}
	if !bytes.Equal(e.EH[:], ep.Hash[:e2ecrypto.IDSize]) {
		return SecInvalid, mid[:]
	}
	d := memo.device(c.trust, sid, e.Dev)
	if d == nil {
		return SecWaiting, mid[:]
	}
	if d.User != m.Author || (d.Revoked && d.RevokedSeq != 0 && m.ID > d.RevokedSeq) {
		return SecInvalid, mid[:]
	}
	raw, err := e2ecrypto.Decrypt(*ep.Key, d.Ed25519[:], sid, m.Chan, m.Enc)
	if err != nil {
		return SecInvalid, mid[:]
	}
	var p plainMsg
	if proto.Unmarshal(raw, &p) != nil || p.T != "msg" || !validText(p.Body) {
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
	sec, mid := c.prepMsg(q, m, memo)
	if sec == SecInvalid || sec == SecWaiting {
		m.Body = ""
	}
	if err := storeMsg(q, *m, sec, mid, replace); err != nil {
		return sec, err
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
		// Rows cached before step 7, or before the channel was known as
		// encrypted, are plain: classify them.
		res1, _ := c.db.Exec(`UPDATE msgs SET sec = ? WHERE chan = ? AND sec = ? AND enc IS NULL AND deleted = 0`,
			SecLegacy, ch.ID, SecPlain)
		var n2 int64
		if e := memo.epoch(c.trust, sid, ch.ID, 1); e != nil && e.Seq != 0 {
			res2, _ := c.db.Exec(`UPDATE msgs SET sec = ? WHERE chan = ? AND sec = ? AND id >= ?`, SecInserted, ch.ID, SecLegacy, e.Seq)
			if res2 != nil {
				n2, _ = res2.RowsAffected()
			}
		}
		if n1, _ := rowsAffected(res1); n1+n2 > 0 {
			u.msgs(ch.ID)
		}
	}
	rows, err := c.db.Query(`SELECT `+msgCols+`, enc FROM msgs WHERE sec = ? ORDER BY id LIMIT 1000`, SecWaiting)
	if err != nil {
		return
	}
	var waiting []proto.Message
	for rows.Next() {
		var enc []byte
		m, err := scanMsg(rows, &enc)
		if err == nil {
			m.Enc = enc
			waiting = append(waiting, m)
		}
	}
	rows.Close()
	reads := c.uintMap(`SELECT chan, seq FROM reads`)
	me := c.Me().ID
	for _, m := range waiting {
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

// sendAllowed refuses sends into an encrypted channel in alarm.
func (c *Client) sendAllowed(ch uint64) error {
	if !c.needsEnc(ch) {
		return nil
	}
	if st := c.E2EStatus(ch); st.State == E2EAlarm {
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

// rosterNow is ch's current non-disabled members, sorted (what a fresh
// bundle's roster must be).
func (c *Client) rosterNow(ch proto.Channel) []uint64 {
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
	if err != nil || !ok || ep.Key == nil || !slices.Equal(ep.Roster, c.rosterNow(ch)) {
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

// sealEntry encrypts an outbox entry for sending (§13.4 "Sending"): it
// catches up on the channel's chain, rotates first when the bundle is
// stale (none yet, the roster changed, or after a stale_epoch the server
// still calls the epoch we used stale), and encrypts with the head's key.
// prev is the enc a stale_epoch answered (nil the first time).
func (c *Client) sealEntry(ctx context.Context, s *session, e outboxEntry, prev []byte) ([]byte, error) {
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
	c.verifyDevices(sid, s.sshPub, me)
	for attempt := 0; ; attempt++ {
		ch, ok, err := getChan(c.db, e.chan_)
		if err != nil {
			return nil, err
		}
		if !ok || !slices.Contains(ch.Members, me) {
			return nil, failErr{"you're not a member of this channel"}
		}
		c.syncChannel(ctx, s, sid, ch)
		if ch, _, err = getChan(c.db, e.chan_); err != nil {
			return nil, err
		}
		st := c.e2eStatus(sid, ch)
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
		case !hasHead && latest > 0:
			return nil, failErr{noKeyReason} // a chain we can't verify (yet)
		case !hasHead:
			rotate = true
		case latest > head.Epoch:
			return nil, failErr{noKeyReason}
		default:
			cur, _, _ := c.trust.Epoch(sid, ch.ID, head.Epoch)
			rotate = !slices.Equal(cur.Roster, c.rosterNow(ch)) || (prev != nil && head.Epoch == prevEpoch)
		}
		if rotate && attempt < 2 {
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
			case attempt == 0:
				// Typically someone's bundle arrived meanwhile ("chain isn't
				// verified yet"): catch up and look again.
				continue
			default:
				return nil, failErr{noKeyReason}
			}
		}
		if !hasHead {
			return nil, failErr{noKeyReason}
		}
		ep, ok, err := c.trust.Epoch(sid, ch.ID, head.Epoch)
		if err != nil || !ok {
			return nil, failErr{noKeyReason}
		}
		return c.seal(sid, dev, ep, e)
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
