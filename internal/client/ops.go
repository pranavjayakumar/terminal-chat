package client

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// ---- outbox ----

// Send queues a message. It is stored in the outbox (with a fresh nonce) and
// shown as pending at once; it is sent when online and retried with the same
// nonce until the server acknowledges it or rejects it for good.
func (c *Client) Send(ch uint64, body string, thread uint64) error {
	if ch == 0 || body == "" {
		return errors.New("send: empty channel or body")
	}
	if len(body) > proto.MaxBody {
		return proto.Errf(proto.ErrTooLarge, "message is over 64 KiB")
	}
	if err := c.sendAllowed(ch); err != nil {
		return err
	}
	e := outboxEntry{nonce: make([]byte, 16), chan_: ch, thread: thread, body: body, time: time.Now().UnixMilli()}
	rand.Read(e.nonce)
	if c.needsEnc(ch) {
		// The server can't check encrypted text (§10): receivers would
		// reject it as invalid, so refuse it here.
		if !validText(body) {
			return proto.Errf(proto.ErrBadRequest, "message is empty or contains control characters")
		}
		// Encrypted once, here if we can (works offline), else by the outbox
		// before the first send; retries send the same bytes.
		e.encNonce = newEncNonce()
		e.enc = c.sealOffline(e)
	}
	if _, err := c.db.Exec(`INSERT INTO outbox (nonce, chan, thread, body, time, enc, encnonce) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.nonce, ch, thread, body, e.time, e.enc, e.encNonce); err != nil {
		return fmt.Errorf("queueing message: %w", err)
	}
	c.outboxChanged(ch, nil)
	return nil
}

// RetryFailed requeues a failed outbox message (same nonce). A failed send
// stored nothing, so an encrypted one is encrypted afresh for the channel
// as it is now.
func (c *Client) RetryFailed(local int64) error {
	return c.editOutbox(`UPDATE outbox SET failed = '', msg = 0, enc = NULL, encnonce = NULL
		WHERE id = ? AND failed != '' RETURNING chan`, local)
}

// DiscardFailed drops a failed outbox message (the cache uses
// secure_delete, so its text doesn't linger in free pages).
func (c *Client) DiscardFailed(local int64) error {
	return c.editOutbox(`DELETE FROM outbox WHERE id = ? AND failed != '' RETURNING chan`, local)
}

func (c *Client) editOutbox(q string, local int64) error {
	var ch uint64
	if err := c.db.QueryRow(q, local).Scan(&ch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("no such failed message")
		}
		return err
	}
	c.outboxChanged(ch, nil)
	return nil
}

// outboxChanged notifies the UI, Flush waiters and the sender.
func (c *Client) outboxChanged(ch uint64, failed *Message) {
	u := Update{}
	u.msgs(ch)
	if failed != nil {
		u.Failed = []Message{*failed}
	}
	c.post(u)
	c.obMu.Lock()
	close(c.obChanged)
	c.obChanged = make(chan struct{})
	c.obMu.Unlock()
	select {
	case c.obKick <- struct{}{}:
	default:
	}
}

// Flush waits until nothing in the outbox is waiting to be sent. It returns
// an error if some messages were rejected (they stay listed as Failed).
func (c *Client) Flush(ctx context.Context) error {
	for {
		c.obMu.Lock()
		changed := c.obChanged
		c.obMu.Unlock()
		var pending, failed int
		var reason sql.NullString
		err := c.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE failed = ''), COUNT(*) FILTER (WHERE failed != ''),
			MAX(failed) FROM outbox`).Scan(&pending, &failed, &reason)
		if err != nil {
			return err
		}
		if pending == 0 {
			if failed > 0 {
				return fmt.Errorf("%d message(s) not sent: %s", failed, reason.String)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// permanent reports whether a send error won't go away by retrying.
func permanent(code string) bool {
	return code != proto.ErrRateLimited && code != proto.ErrInternal && code != proto.ErrStaleEpoch
}

// sendEntry sends one outbox entry: as is into a plaintext channel, or
// encrypted (encrypting it first if it isn't yet) into an encrypted one.
// A stale_epoch means nothing was stored: catch up, re-encrypt with the
// same nonces and try again, twice at most.
func (c *Client) sendEntry(ctx context.Context, s *session, e outboxEntry) error {
	if !c.needsEnc(e.chan_) {
		_, err := s.call(ctx, proto.OpSend,
			proto.SendArgs{Chan: e.chan_, Body: e.body, Nonce: e.nonce, Thread: e.thread}, c.sentHook(e))
		return err
	}
	if !s.e2e() {
		return failErr{"not sent: the server stopped offering end-to-end encryption"}
	}
	if st := c.E2EStatus(e.chan_); st.State == E2EAlarm && !unconfirmedAlarm(st) { // sealEntry judges that one
		return failErr{"not sent: encryption alarm: " + sanitize.Line(st.Detail)}
	}
	// An entry encrypted while queued but never handed to the server is
	// re-encrypted if the channel moved on meanwhile (new head, other
	// members). Once sent, only a stale_epoch may change its bytes.
	if e.enc != nil && e.sent == 0 && !c.encCurrent(e) {
		e.enc = nil
		if _, err := c.db.Exec(`UPDATE outbox SET enc = NULL WHERE id = ?`, e.id); err != nil {
			return err
		}
	}
	if len(e.encNonce) != e2ecrypto.NonceSize {
		e.encNonce, e.enc = newEncNonce(), nil
		if _, err := c.db.Exec(`UPDATE outbox SET encnonce = ?, enc = NULL WHERE id = ?`, e.encNonce, e.id); err != nil {
			return err
		}
	}
	var stale []byte
	for attempt := 0; ; attempt++ {
		if e.enc == nil {
			enc, err := c.sealEntry(ctx, s, e, stale)
			if err != nil {
				return err
			}
			// Stored before it goes out: a lost RES must find these bytes.
			if _, err := c.db.Exec(`UPDATE outbox SET enc = ? WHERE id = ?`, enc, e.id); err != nil {
				return err
			}
			e.enc = enc
		}
		_, err := s.call(ctx, proto.OpSend,
			proto.SendArgs{Chan: e.chan_, Nonce: e.nonce, Thread: e.thread, Enc: e.enc}, c.sentHook(e))
		var pe *proto.Error
		if attempt < 2 && errors.As(err, &pe) && pe.Code == proto.ErrStaleEpoch {
			stale, e.enc = e.enc, nil
			continue
		}
		return err
	}
}

// outboxLoop sends the outbox oldest first, one at a time, which keeps
// per-channel order. It stops when the session ends; the next session
// resends whatever is left with the same nonces.
func (c *Client) outboxLoop(ctx context.Context, s *session) {
	for {
		e, err := scanOutbox(c.db.QueryRow(`SELECT ` + outboxCols + ` FROM outbox WHERE failed = '' ORDER BY id LIMIT 1`))
		if errors.Is(err, sql.ErrNoRows) {
			select {
			case <-ctx.Done():
				return
			case <-c.obKick:
			}
			continue
		}
		if err == nil {
			// Record where the log stood at the first send; see matchOutbox.
			_, err = c.db.Exec(`UPDATE outbox SET sent = COALESCE((SELECT v FROM meta WHERE k = 'last_seq'), 0) + 1
				WHERE id = ? AND sent = 0`, e.id)
		}
		if err == nil {
			err = c.sendEntry(ctx, s, e)
		}
		var perr *proto.Error
		var ferr failErr
		reason := ""
		switch {
		case errors.As(err, &ferr):
			reason = ferr.reason
		case errors.As(err, &perr) && permanent(perr.Code):
			reason = sanitize.Line(perr.Error())
		}
		switch {
		case err == nil:
		case reason != "":
			if _, err := c.db.Exec(`UPDATE outbox SET failed = ?, msg = 0 WHERE id = ?`, reason, e.id); err == nil {
				e.failed = reason
				m := e.message(c.Me().ID)
				c.outboxChanged(e.chan_, &m)
			}
			if reason == noKeyReason {
				if sid, ok := s.serverID(); ok {
					c.sendKeyNeed(s, sid, e.chan_)
				}
			}
		case errors.Is(err, ErrOffline) || ctx.Err() != nil:
			return
		default: // rate limited, server hiccup, local DB error
			t := time.NewTimer(c.timing.retry)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

// sentHook settles an entry on the read goroutine when its RES arrives,
// right after its msg.new event was applied. Success deletes it, so the
// pending copy never coexists with the real one in a published state. A
// failed send created no message, so a tentative match (matchOutbox) was
// wrong and the entry shows again.
func (c *Client) sentHook(e outboxEntry) func(proto.Res) error {
	return func(r proto.Res) error {
		if !r.OK {
			res, err := c.db.Exec(`UPDATE outbox SET msg = 0 WHERE id = ? AND msg != 0`, e.id)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				u := Update{}
				u.msgs(e.chan_)
				c.post(u)
			}
			return nil
		}
		if _, err := c.db.Exec(`DELETE FROM outbox WHERE id = ?`, e.id); err != nil {
			return err
		}
		c.outboxChanged(e.chan_, nil)
		return nil
	}
}

// ---- read markers ----

// MarkRead moves ch's read marker to seq (a message id). It applies locally
// at once; the server is told when online (now, or after the next sync).
func (c *Client) MarkRead(ch, seq uint64) error {
	if err := setRead(c.db, ch, seq, true); err != nil {
		return err
	}
	c.post(Update{Channels: true})
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s != nil {
		_ = s.fire(proto.OpRead, proto.ReadArgs{Chan: ch, Seq: seq})
	}
	return nil
}

// pushReads sends read markers the server hasn't confirmed yet.
func (c *Client) pushReads(s *session) {
	for ch, seq := range c.uintMap(`SELECT chan, seq FROM reads WHERE dirty = 1`) {
		if s.fire(proto.OpRead, proto.ReadArgs{Chan: ch, Seq: seq}) != nil {
			return
		}
	}
}

// ---- online-only requests ----

func (c *Client) call(ctx context.Context, op string, args, out any, hook func(proto.Res) error) error {
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return ErrOffline
	}
	data, err := s.call(ctx, op, args, hook)
	if err != nil {
		return err
	}
	if out != nil && len(data) > 0 {
		return proto.Unmarshal(data, out)
	}
	return nil
}

// Edit changes the body of one of our messages.
func (c *Client) Edit(ctx context.Context, msg uint64, body string) error {
	// Never send new text in the clear without knowing where it goes: the
	// message must be cached, in a channel that isn't (and wasn't) encrypted.
	c.mu.Lock()
	online := c.sess != nil
	c.mu.Unlock()
	if !online {
		return ErrOffline
	}
	row, ok := getMsgRow(c.db, msg)
	if !ok {
		return errNotLoaded
	}
	if c.needsEnc(row.ch) {
		return c.editEnc(ctx, row, body) // encops.go
	}
	return c.call(ctx, proto.OpEdit, proto.EditArgs{Msg: msg, Body: body}, nil, nil)
}

// Delete deletes a message (ours, or any as admin). Deleting twice is ok.
// Our own encrypted messages get a signed delete (encops.go).
func (c *Client) Delete(ctx context.Context, msg uint64) error {
	a, err := c.deleteArgs(msg)
	if err != nil {
		return err
	}
	return c.call(ctx, proto.OpDelete, a, nil, nil)
}

// React adds our emoji reaction (in an encrypted channel: a tag the server
// can't read plus a signed enc with the emoji).
func (c *Client) React(ctx context.Context, msg uint64, emoji string) error {
	a, err := c.reactArgs(msg, emoji, true)
	if err != nil {
		return err
	}
	return c.call(ctx, proto.OpReact, a, nil, nil)
}

// Unreact removes our emoji reaction.
func (c *Client) Unreact(ctx context.Context, msg uint64, emoji string) error {
	a, err := c.reactArgs(msg, emoji, false)
	if err != nil {
		return err
	}
	return c.call(ctx, proto.OpUnreact, a, nil, nil)
}

// chanHook caches a channel returned in a RES if no event delivered it
// (dm.open of an existing DM emits nothing).
func (c *Client) chanHook(r proto.Res) error {
	var cr proto.ChanRes
	if !r.OK || len(r.Data) == 0 || proto.Unmarshal(r.Data, &cr) != nil || cr.Chan.ID == 0 {
		return nil
	}
	err := c.tx(func(tx *sql.Tx) error {
		if _, ok, err := getChan(tx, cr.Chan.ID); err != nil || ok {
			return err
		}
		return c.storeChanE2E(tx, cr.Chan)
	})
	if err == nil {
		c.post(Update{Channels: true})
	}
	return err
}

// CreateChannel creates a public or private channel.
func (c *Client) CreateChannel(ctx context.Context, name string, private bool, topic string) (proto.Channel, error) {
	var r proto.ChanRes
	err := c.call(ctx, proto.OpChanCreate, proto.ChanCreateArgs{Name: name, Private: private, Topic: topic}, &r, c.chanHook)
	return r.Chan, err
}

// CreateEncryptedChannel creates an end-to-end encrypted private channel
// (needs a server with e2e1). Its first epoch key is created in the
// background once this device is registered; see E2EStatus.
func (c *Client) CreateEncryptedChannel(ctx context.Context, name, topic string) (proto.Channel, error) {
	var r proto.ChanRes
	err := c.call(ctx, proto.OpChanCreate, proto.ChanCreateArgs{Name: name, Private: true, Topic: topic, E2E: true}, &r, c.chanHook)
	if err == nil && !r.Chan.E2E {
		return r.Chan, errors.New("the server created the channel without encryption")
	}
	if err == nil {
		if sid, ok := c.cacheServerID(); ok {
			_ = c.trust.MarkChannelE2E(sid, r.Chan.ID) // our choice: sticky from now on
		}
	}
	c.kickE2E()
	return r.Chan, err
}

// OpenDM returns (creating if needed) the DM with exactly these other users.
// A new DM is encrypted when the server speaks e2e1; its first epoch key is
// created in the background (see E2EStatus).
func (c *Client) OpenDM(ctx context.Context, users []uint64) (proto.Channel, error) {
	var r proto.ChanRes
	err := c.call(ctx, proto.OpDMOpen, proto.DMOpenArgs{Users: users}, &r, c.chanHook)
	c.kickE2E()
	return r.Chan, err
}

// Join joins a public channel.
func (c *Client) Join(ctx context.Context, ch uint64) error {
	return c.call(ctx, proto.OpChanJoin, proto.ChanArgs{Chan: ch}, nil, nil)
}

// SetTopic sets ch's topic.
func (c *Client) SetTopic(ctx context.Context, ch uint64, topic string) error {
	return c.call(ctx, proto.OpChanTopic, proto.ChanTopicArgs{Chan: ch, Topic: topic}, nil, nil)
}

// History fetches up to 100 messages of ch older than before (0 = latest)
// into the cache; Messages/Thread then include them. more reports whether
// older ones exist.
func (c *Client) History(ctx context.Context, ch, before uint64) (more bool, err error) {
	var r proto.HistoryRes
	hook := func(res proto.Res) error {
		var hr proto.HistoryRes
		if !res.OK || len(res.Data) == 0 || proto.Unmarshal(res.Data, &hr) != nil {
			return nil
		}
		// The RES is ordered after every event we've applied, so its copies
		// are at least as new as the cache.
		err := c.saveMsgs(hr.Msgs)
		if err == nil {
			c.post(Update{Msgs: map[uint64]bool{ch: true}, Channels: true})
		}
		return err
	}
	err = c.call(ctx, proto.OpHistory, proto.HistoryArgs{Chan: ch, Before: before, Limit: 100}, &r, hook)
	return r.More, err
}

// saveMsgs stores messages from a RES (history), judging encrypted ones.
// The RES is ordered after every event we've applied, so its copies are
// at least as new as the cache.
func (c *Client) saveMsgs(ms []proto.Message) error {
	memo := newKeyMemo()
	err := c.tx(func(tx *sql.Tx) error {
		for _, m := range ms {
			if _, err := c.saveMsg(tx, &m, true, memo); err != nil {
				return err
			}
		}
		return nil
	})
	c.kickE2E() // thread roots may have arrived for waiting replies
	return err
}

// Search searches messages (ch 0 = everywhere): on the server, plus the
// decrypted messages of encrypted channels in the local cache, which the
// server can't search. Newest first; results are not cached. Offline, only
// the local part runs (ErrOffline if it finds nothing).
func (c *Client) Search(ctx context.Context, q string, ch uint64) ([]proto.Message, error) {
	local := c.searchLocal(q, ch, proto.MaxSearch)
	if ch != 0 && c.needsEnc(ch) {
		return local, nil // the query of an encrypted channel never leaves this device
	}
	var r proto.MsgsRes
	err := c.call(ctx, proto.OpSearch, proto.SearchArgs{Q: q, Chan: ch, Limit: proto.MaxSearch}, &r, nil)
	if err != nil && (!errors.Is(err, ErrOffline) || len(local) == 0) {
		return nil, err
	}
	// The server shouldn't return encrypted channels' messages; if it does,
	// they're not ours to trust. Local rows come first so they win ids.
	out := local
	for _, m := range r.Msgs {
		if !c.needsEnc(m.Chan) {
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b proto.Message) int {
		switch {
		case a.ID > b.ID:
			return -1
		case a.ID < b.ID:
			return 1
		}
		return 0
	})
	out = slices.CompactFunc(out, func(a, b proto.Message) bool { return a.ID == b.ID })
	return out[:min(len(out), proto.MaxSearch)], nil
}

// CreateInvite (admin) returns an invite code: a new-user invite, or with
// user != 0 an add-key invite for that user.
func (c *Client) CreateInvite(ctx context.Context, user uint64) (string, error) {
	var r proto.InviteRes
	err := c.call(ctx, proto.OpInviteCreate, proto.InviteCreateArgs{User: user}, &r, nil)
	return r.Code, err
}

// ---- ephemeral ----

const typingEvery = 3 * time.Second

// SendTyping tells ch's members we're typing; call it on every keystroke,
// it rate-limits itself to once per 3 s per channel.
func (c *Client) SendTyping(ch uint64) error {
	c.mu.Lock()
	s := c.sess
	if s == nil {
		c.mu.Unlock()
		return ErrOffline
	}
	if time.Since(c.typed[ch]) < typingEvery {
		c.mu.Unlock()
		return nil
	}
	c.typed[ch] = time.Now()
	c.mu.Unlock()
	if err := s.send(proto.TEphemeral, proto.Ephemeral{Kind: proto.EphTyping, Chan: ch}); err != nil {
		return ErrOffline
	}
	return nil
}

// SetAway sets our presence to away (or back to online) with an optional
// status. It is remembered and re-sent after every reconnect.
func (c *Client) SetAway(away bool, status string) error {
	if len(status) > proto.MaxStatus {
		return proto.Errf(proto.ErrTooLarge, "status is over 128 bytes")
	}
	e := proto.Ephemeral{Kind: proto.EphPresence, State: proto.PresOnline, Status: status}
	if away {
		e.State = proto.PresAway
	}
	c.mu.Lock()
	c.away = &e
	if !away && status == "" {
		c.away = nil
	}
	s := c.sess
	c.presence[c.me.ID] = Presence{State: e.State, Status: status}
	c.mu.Unlock()
	c.post(Update{Presence: true})
	if s != nil {
		_ = s.send(proto.TEphemeral, e)
	}
	return nil
}

// SetDisplay sets my display name ("" clears it).
func (c *Client) SetDisplay(ctx context.Context, display string) error {
	return c.call(ctx, proto.OpUserUpdate, proto.UserUpdateArgs{Display: display}, nil, nil)
}

// ThreadHistory fetches older replies of thread root (before reply id, 0 =
// newest) into the cache.
func (c *Client) ThreadHistory(ctx context.Context, ch, root, before uint64) (more bool, err error) {
	var r proto.HistoryRes
	hook := func(res proto.Res) error {
		var hr proto.HistoryRes
		if !res.OK || len(res.Data) == 0 || proto.Unmarshal(res.Data, &hr) != nil {
			return nil
		}
		err := c.saveMsgs(hr.Msgs)
		if err == nil {
			c.post(Update{Msgs: map[uint64]bool{ch: true}})
		}
		return err
	}
	err = c.call(ctx, proto.OpHistory, proto.HistoryArgs{Chan: ch, Before: before, Limit: 100, Thread: root}, &r, hook)
	return r.More, err
}

// OpenNotes opens my private notes (a DM with only me).
func (c *Client) OpenNotes(ctx context.Context) (proto.Channel, error) {
	return c.OpenDM(ctx, nil)
}
