package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

const (
	maxEmojiPerMsg      = 32
	maxReactionsPerUser = 10
	maxSearchQuery      = 256
	maxSearchTerms      = 8
)

var (
	errForbidden = proto.Errf(proto.ErrForbidden, "")
	errNotFound  = proto.Errf(proto.ErrNotFound, "")
)

func badRequest(msg string) error { return proto.Errf(proto.ErrBadRequest, msg) }

func decodeArgs[T any](raw proto.Raw) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	if err := proto.Unmarshal(raw, &v); err != nil {
		return v, badRequest("bad arguments")
	}
	return v, nil
}

// writeOps are rate limited by allowWrite (send has its own limit).
var writeOps = map[string]bool{
	proto.OpEdit: true, proto.OpDelete: true, proto.OpReact: true, proto.OpUnreact: true, proto.OpPin: true, proto.OpUnpin: true,
	proto.OpGroupSet: true, proto.OpRemindCreate: true, proto.OpRemindDelete: true,
	proto.OpRead: true, proto.OpChanCreate: true, proto.OpChanJoin: true, proto.OpChanAdd: true,
	proto.OpChanLeave: true, proto.OpChanTopic: true, proto.OpDMOpen: true,
	proto.OpInviteCreate: true, proto.OpUserDisable: true, proto.OpUserUpdate: true,
	proto.OpHookCreate: true, proto.OpHookDelete: true, proto.OpHookList: true,
	proto.OpDeviceRegister: true, proto.OpDeviceRevoke: true, proto.OpKeyRotate: true,
	proto.OpKeyShare: true,
}

// e2eOps exist only in sessions that negotiated proto.CapE2E.
var e2eOps = map[string]bool{
	proto.OpDeviceRegister: true, proto.OpDeviceRevoke: true, proto.OpDevicesList: true,
	proto.OpKeyRotate: true, proto.OpKeyFetch: true, proto.OpKeyShare: true,
}

func (s *Server) dispatch(ctx context.Context, sess *session, op string, args proto.Raw) (any, error) {
	u := sess.user
	if e2eOps[op] && !sess.e2e {
		return nil, proto.Errf(proto.ErrUnknownOp, "")
	}
	s.mu.Lock()
	disabled := s.disabled[u.ID]
	limited := writeOps[op] && !s.allowWrite(u.ID)
	s.mu.Unlock()
	if disabled {
		return nil, proto.Errf(proto.ErrUnauthorized, "account disabled")
	}
	if limited {
		return nil, proto.Errf(proto.ErrRateLimited, "slow down")
	}
	switch op {
	case proto.OpSend:
		return withArgs(args, func(a proto.SendArgs) (any, error) { return s.opSend(ctx, u, a) })
	case proto.OpEdit:
		return withArgs(args, func(a proto.EditArgs) (any, error) { return s.opEdit(ctx, u, a) })
	case proto.OpDelete:
		return withArgs(args, func(a proto.MsgArgs) (any, error) { return s.opDelete(ctx, u, a) })
	case proto.OpReact, proto.OpUnreact:
		return withArgs(args, func(a proto.ReactArgs) (any, error) { return s.opReact(ctx, u, a, op == proto.OpReact) })
	case proto.OpPin, proto.OpUnpin:
		return withArgs(args, func(a proto.MsgArgs) (any, error) { return s.opPin(ctx, u, a, op == proto.OpPin) })
	case proto.OpPinsList:
		return withArgs(args, func(a proto.ChanArgs) (any, error) { return s.opPinsList(ctx, u, a) })
	case proto.OpGroupSet:
		return withArgs(args, func(a proto.GroupSetArgs) (any, error) { return s.opGroupSet(ctx, u, a) })
	case proto.OpGroupsList:
		return s.opGroupsList(ctx)
	case proto.OpRemindCreate:
		return withArgs(args, func(a proto.RemindCreateArgs) (any, error) { return s.opRemindCreate(ctx, u, a) })
	case proto.OpRemindList:
		return s.opRemindList(ctx, u)
	case proto.OpRemindDelete:
		return withArgs(args, func(a proto.RemindDeleteArgs) (any, error) { return s.opRemindDelete(ctx, u, a) })
	case proto.OpHistory:
		return withArgs(args, func(a proto.HistoryArgs) (any, error) { return s.opHistory(ctx, u, a) })
	case proto.OpSearch:
		return withArgs(args, func(a proto.SearchArgs) (any, error) { return s.opSearch(ctx, u, a) })
	case proto.OpRead:
		return withArgs(args, func(a proto.ReadArgs) (any, error) { return s.opRead(ctx, u, a) })
	case proto.OpChanCreate:
		return withArgs(args, func(a proto.ChanCreateArgs) (any, error) { return s.opChanCreate(ctx, sess, a) })
	case proto.OpChanJoin:
		return withArgs(args, func(a proto.ChanArgs) (any, error) {
			return s.opChanAdd(ctx, u, proto.ChanAddArgs{Chan: a.Chan, User: u.ID}, true)
		})
	case proto.OpChanAdd:
		return withArgs(args, func(a proto.ChanAddArgs) (any, error) { return s.opChanAdd(ctx, u, a, false) })
	case proto.OpChanLeave:
		return withArgs(args, func(a proto.ChanArgs) (any, error) { return s.opChanLeave(ctx, u, a) })
	case proto.OpChanTopic:
		return withArgs(args, func(a proto.ChanTopicArgs) (any, error) { return s.opChanTopic(ctx, u, a) })
	case proto.OpDMOpen:
		return withArgs(args, func(a proto.DMOpenArgs) (any, error) { return s.opDMOpen(ctx, sess, a) })
	case proto.OpUsersList:
		users, err := listUsers(ctx, s.st.db)
		return proto.UsersRes{Users: users}, err
	case proto.OpInviteCreate:
		return withArgs(args, func(a proto.InviteCreateArgs) (any, error) { return s.opInviteCreate(ctx, u, a) })
	case proto.OpUserUpdate:
		return withArgs(args, func(a proto.UserUpdateArgs) (any, error) { return s.opUserUpdate(ctx, u, a) })
	case proto.OpUserDisable:
		return withArgs(args, func(a proto.UserArgs) (any, error) { return s.opUserDisable(ctx, u, a) })
	case proto.OpDeviceRegister:
		return withArgs(args, func(a proto.DeviceRegisterArgs) (any, error) { return s.opDeviceRegister(ctx, u, a) })
	case proto.OpDeviceRevoke:
		return withArgs(args, func(a proto.DeviceRevokeArgs) (any, error) { return s.opDeviceRevoke(ctx, u, a) })
	case proto.OpDevicesList:
		return withArgs(args, func(a proto.DevicesListArgs) (any, error) { return s.opDevicesList(ctx, a) })
	case proto.OpKeyRotate:
		return withArgs(args, func(a proto.KeyRotateArgs) (any, error) { return s.opKeyRotate(ctx, u, a) })
	case proto.OpKeyFetch:
		return withArgs(args, func(a proto.KeyFetchArgs) (any, error) { return s.opKeyFetch(ctx, u, a) })
	case proto.OpKeyShare:
		return withArgs(args, func(a proto.KeyShareArgs) (any, error) { return s.opKeyShare(ctx, u, a) })
	}
	if res, ok, err := s.hookOp(ctx, u, op, args); ok {
		return res, err
	}
	return nil, proto.Errf(proto.ErrUnknownOp, "")
}

func withArgs[T any](raw proto.Raw, fn func(T) (any, error)) (any, error) {
	a, err := decodeArgs[T](raw)
	if err != nil {
		return nil, err
	}
	return fn(a)
}

// ---- validation (PROTOCOL.md §9, §10) ----

func validBody(s string) error {
	if len(s) > proto.MaxBody {
		return proto.Errf(proto.ErrTooLarge, "message too long")
	}
	if strings.TrimSpace(s) == "" {
		return badRequest("empty message")
	}
	if !sanitize.Valid(s) {
		return badRequest("message contains control characters")
	}
	return nil
}

func validLine(s string, max int) bool {
	if len(s) > max || !sanitize.Valid(s) || strings.ContainsAny(s, "\n\t") {
		return false
	}
	for _, r := range s {
		if (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return false
		}
	}
	return true
}

func validEmoji(s string) bool {
	return s != "" && validLine(s, proto.MaxEmoji) && !strings.ContainsFunc(s, unicode.IsSpace)
}

// canRead: public channels, or channels u currently belongs to.
func (s *Server) canRead(ch, u uint64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kind, ok := s.kinds[ch]
	if !ok {
		return "", false
	}
	return kind, kind == proto.ChanPublic || s.members[ch][u]
}

// ---- messages ----

func (s *Server) opSend(ctx context.Context, u proto.User, a proto.SendArgs) (any, error) {
	return s.send(ctx, u, a, nil)
}

// send implements the send op. For a webhook send, hook is the hook that
// was looked up; it is re-checked inside the write transaction so a hook
// deleted or revoked meanwhile never posts.
func (s *Server) send(ctx context.Context, u proto.User, a proto.SendArgs, hook *liveHook) (any, error) {
	if len(a.Nonce) != 16 {
		return nil, badRequest("nonce must be 16 bytes")
	}
	var encHdr e2ecrypto.Enc
	if a.Enc != nil {
		// Exactly one of body and enc (§13.4). Framing is checked before
		// anything else: a malformed enc was never stored, so it can't be
		// an idempotent retry.
		if a.Body != "" {
			return nil, badRequest("send has both body and enc")
		}
		if len(a.Enc) > proto.MaxEnc {
			return nil, proto.Errf(proto.ErrTooLarge, "encrypted message too long")
		}
		var err error
		if encHdr, err = e2ecrypto.ParseEnc(a.Enc); err != nil {
			return nil, badRequest("malformed enc")
		}
	} else if err := validBody(a.Body); err != nil {
		return nil, err
	}
	sid := s.sid()
	var res proto.SendRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		if hook != nil {
			if err := s.hookLiveLocked(ctx, tx, hook); err != nil {
				return nil, err
			}
		}
		// Idempotent retry: same (author, nonce) returns the original
		// message. This comes first, before any epoch check: a resend of an
		// enc that was stored under an older epoch must still find it.
		var id, ch, thread uint64
		var body string
		var enc []byte
		var edited int64
		var deleted bool
		err := tx.QueryRowContext(ctx, `SELECT id, chan, thread, body, enc, edited, deleted FROM messages WHERE author = ? AND nonce = ?`,
			u.ID, a.Nonce).Scan(&id, &ch, &thread, &body, &enc, &edited, &deleted)
		if err == nil {
			if ch != a.Chan || thread != a.Thread || (edited == 0 && !deleted && (body != a.Body || !bytes.Equal(enc, a.Enc))) {
				return nil, proto.Errf(proto.ErrConflict, "nonce reused for a different message")
			}
			res.Msg = id
			return nil, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if !s.members[a.Chan][u.ID] {
			return nil, errForbidden
		}
		head, err := chanEpoch(ctx, tx, a.Chan)
		if err != nil {
			return nil, err
		}
		switch {
		case head.e2e && hook != nil:
			return nil, errHookE2E
		case head.e2e && a.Enc == nil:
			return nil, badRequest("this channel is end-to-end encrypted — upgrade tc")
		case !head.e2e && a.Enc != nil:
			return nil, badRequest("channel is not encrypted")
		case a.Enc != nil:
			if err := checkEnc(ctx, tx, sid, u.ID, a, encHdr, head); err != nil {
				return nil, err
			}
		}
		var mid []byte
		if a.Enc != nil {
			// One message per enc and channel: a replay under a new nonce is
			// refused (a retry with the original nonce was found above).
			m := e2ecrypto.MID(a.Enc)
			mid = m[:]
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE chan = ? AND mid = ?`, a.Chan, mid).Scan(&n); err != nil {
				return nil, err
			}
			if n > 0 {
				return nil, proto.Errf(proto.ErrConflict, "this encrypted message was already sent")
			}
		}
		if a.Thread != 0 {
			var root uint64
			err := tx.QueryRowContext(ctx, `SELECT thread FROM messages WHERE id = ? AND chan = ?`, a.Thread, a.Chan).Scan(&root)
			if err != nil || root != 0 {
				return nil, badRequest("thread must be a top-level message in this channel")
			}
		}
		// A hook send also spends its creator's send budget, so making more
		// hooks doesn't multiply anyone's throughput.
		if hook != nil && !s.allowSend(hook.creator) {
			return nil, proto.Errf(proto.ErrRateLimited, "slow down")
		}
		if !s.allowSend(u.ID) {
			return nil, proto.Errf(proto.ErrRateLimited, "slow down")
		}
		now := s.now()
		m := proto.Message{Chan: a.Chan, Author: u.ID, Body: a.Body, Thread: a.Thread, Time: now, Enc: a.Enc}
		ev, err := appendEvent(ctx, tx, proto.KMsgNew, a.Chan, u.ID, 0, now, m)
		if err != nil {
			return nil, err
		}
		m.ID = ev.Seq
		if err := patchEventData(ctx, tx, &ev, m); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, chan, author, body, thread, time, nonce, enc, mid) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.Chan, m.Author, m.Body, m.Thread, m.Time, a.Nonce, a.Enc, mid); err != nil {
			return nil, err
		}
		res.Msg = m.ID
		return []proto.Event{ev}, nil
	})
	return res, err
}

func (s *Server) opEdit(ctx context.Context, u proto.User, a proto.EditArgs) (any, error) {
	if err := validBody(a.Body); err != nil {
		return nil, err
	}
	var res proto.SeqRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		m, err := getMessage(ctx, tx, a.Msg)
		if err != nil || m.Deleted || !s.members[m.Chan][u.ID] {
			return nil, errNotFound
		}
		if m.Author != u.ID {
			return nil, errForbidden
		}
		// Plaintext must never land in an encrypted channel; encrypted
		// edits are step 8 (docs/E2E.md).
		if head, err := chanEpoch(ctx, tx, m.Chan); err != nil {
			return nil, err
		} else if head.e2e || m.Enc != nil {
			return nil, badRequest("this channel is end-to-end encrypted — upgrade tc")
		}
		if m.Body == a.Body {
			return nil, nil
		}
		now := s.now()
		ev, err := appendEvent(ctx, tx, proto.KMsgEdit, m.Chan, u.ID, 0, now, proto.MsgEdit{Msg: m.ID, Body: a.Body, Edited: now})
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET body = ?, edited = ? WHERE id = ?`, a.Body, now, m.ID); err != nil {
			return nil, err
		}
		m.Body, m.Edited = a.Body, now
		if err := scrubMessageEvents(ctx, tx, m, ev.Seq); err != nil {
			return nil, err
		}
		res.Seq = ev.Seq
		return []proto.Event{ev}, nil
	})
	return res, err
}

func (s *Server) opDelete(ctx context.Context, u proto.User, a proto.MsgArgs) (any, error) {
	var res proto.SeqRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		m, err := getMessage(ctx, tx, a.Msg)
		if err != nil {
			return nil, errNotFound
		}
		if m.Author != u.ID && !u.Admin {
			if !s.members[m.Chan][u.ID] {
				return nil, errNotFound
			}
			return nil, errForbidden
		}
		if m.Deleted {
			return nil, nil
		}
		ev, err := appendEvent(ctx, tx, proto.KMsgDelete, m.Chan, u.ID, 0, s.now(), proto.MsgRef{Msg: m.ID})
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET body = '', enc = NULL, deleted = 1 WHERE id = ?`, m.ID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM reactions WHERE msg = ?`, m.ID); err != nil {
			return nil, err
		}
		evs := []proto.Event{ev}
		unpin, err := s.unpinInTx(ctx, tx, m, u.ID)
		if err != nil {
			return nil, err
		}
		evs = append(evs, unpin...)
		m.Body, m.Enc, m.Deleted = "", nil, true
		if err := scrubMessageEvents(ctx, tx, m, 0); err != nil {
			return nil, err
		}
		res.Seq = ev.Seq
		return evs, nil
	})
	return res, err
}

func (s *Server) opReact(ctx context.Context, u proto.User, a proto.ReactArgs, add bool) (any, error) {
	if !validEmoji(a.Emoji) {
		return nil, badRequest("invalid emoji")
	}
	var res proto.SeqRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		m, err := getMessage(ctx, tx, a.Msg)
		if err != nil || m.Deleted || !s.members[m.Chan][u.ID] {
			return nil, errNotFound
		}
		if add {
			if len(m.Reactions) >= maxEmojiPerMsg && m.Reactions[a.Emoji] == nil {
				return nil, badRequest("too many different reactions on this message")
			}
			mine := 0
			for _, users := range m.Reactions {
				if slices.Contains(users, u.ID) {
					mine++
				}
			}
			if mine >= maxReactionsPerUser {
				return nil, badRequest("too many reactions from you on this message")
			}
		}
		var r sql.Result
		kind := proto.KReactAdd
		if add {
			r, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO reactions (msg, user, emoji) VALUES (?, ?, ?)`, m.ID, u.ID, a.Emoji)
		} else {
			kind = proto.KReactRemove
			r, err = tx.ExecContext(ctx, `DELETE FROM reactions WHERE msg = ? AND user = ? AND emoji = ?`, m.ID, u.ID, a.Emoji)
		}
		if err != nil {
			return nil, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return nil, nil
		}
		ev, err := appendEvent(ctx, tx, kind, m.Chan, u.ID, 0, s.now(), proto.Reaction{Msg: m.ID, Emoji: a.Emoji})
		if err != nil {
			return nil, err
		}
		res.Seq = ev.Seq
		return []proto.Event{ev}, nil
	})
	return res, err
}

func (s *Server) opHistory(ctx context.Context, u proto.User, a proto.HistoryArgs) (any, error) {
	if _, ok := s.canRead(a.Chan, u.ID); !ok {
		return nil, errForbidden
	}
	limit := a.Limit
	if limit <= 0 || limit > proto.MaxHistory {
		limit = proto.MaxHistory
	}
	before := a.Before
	if before == 0 || before > math.MaxInt64 {
		before = math.MaxInt64
	}
	query := `SELECT ` + msgCols + ` FROM messages WHERE chan = ? AND id < ? ORDER BY id DESC LIMIT ?`
	qargs := []any{a.Chan, before, limit + 1}
	if a.Thread != 0 {
		query = `SELECT ` + msgCols + ` FROM messages WHERE chan = ? AND thread = ? AND id < ? ORDER BY id DESC LIMIT ?`
		qargs = []any{a.Chan, a.Thread, before, limit + 1}
	}
	msgs, err := scanMessages(ctx, s.st.db, query, qargs...)
	if err != nil {
		return nil, err
	}
	more := len(msgs) > limit
	msgs = trimBytes(msgs[:min(len(msgs), limit)], &more)
	slices.Reverse(msgs)
	return proto.HistoryRes{Msgs: msgs, More: more}, nil
}

// trimBytes keeps messages until ~MaxResultBytes so a RES fits in a frame.
func trimBytes(msgs []proto.Message, more *bool) []proto.Message {
	size := 0
	for i, m := range msgs {
		size += encodedSize(m)
		if size > proto.MaxResultBytes {
			*more = true
			return msgs[:i]
		}
	}
	return msgs
}

func (s *Server) opSearch(ctx context.Context, u proto.User, a proto.SearchArgs) (any, error) {
	if len(a.Q) > maxSearchQuery {
		return nil, badRequest("search query too long")
	}
	q := ftsQuery(a.Q)
	if q == "" {
		return proto.MsgsRes{}, nil
	}
	limit := a.Limit
	if limit <= 0 || limit > proto.MaxSearch {
		limit = proto.MaxSearch
	}
	query := `SELECT ` + prefixCols("m.", msgCols) + ` FROM msg_fts f JOIN messages m ON m.id = f.rowid
		JOIN channels c ON c.id = m.chan
		WHERE msg_fts MATCH ? AND m.deleted = 0 AND c.e2e = 0
		AND (c.kind = 'public' OR EXISTS (SELECT 1 FROM membership x WHERE x.chan = m.chan AND x.user = ? AND x.leave_seq IS NULL))`
	args := []any{q, u.ID}
	if a.Chan != 0 {
		query += ` AND m.chan = ?`
		args = append(args, a.Chan)
	}
	query += ` ORDER BY m.id DESC LIMIT ?`
	args = append(args, limit)
	msgs, err := scanMessages(ctx, s.st.db, query, args...)
	if err != nil {
		if strings.Contains(err.Error(), "fts5") {
			return proto.MsgsRes{}, nil // query had no searchable tokens
		}
		return nil, err
	}
	var more bool
	return proto.MsgsRes{Msgs: trimBytes(msgs, &more)}, nil
}

func (s *Server) opRead(ctx context.Context, u proto.User, a proto.ReadArgs) (any, error) {
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		if !s.members[a.Chan][u.ID] {
			return nil, errForbidden
		}
		var cur uint64
		tx.QueryRowContext(ctx, `SELECT seq FROM reads WHERE user = ? AND chan = ?`, u.ID, a.Chan).Scan(&cur)
		// Clamp to the channel's newest message so repeated reads can't
		// keep minting read.update events.
		var last uint64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM messages WHERE chan = ?`, a.Chan).Scan(&last); err != nil {
			return nil, err
		}
		seq := min(a.Seq, last)
		if seq <= cur {
			return nil, nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO reads (user, chan, seq) VALUES (?, ?, ?)
			ON CONFLICT (user, chan) DO UPDATE SET seq = excluded.seq`, u.ID, a.Chan, seq); err != nil {
			return nil, err
		}
		ev, err := appendEvent(ctx, tx, proto.KReadUpdate, a.Chan, u.ID, 0, s.now(), proto.ReadUpdate{Chan: a.Chan, Seq: seq})
		return []proto.Event{ev}, err
	})
	return proto.Empty{}, err
}

// ---- channels ----

// createChannel inserts a channel whose members all join at the chan.new seq.
func createChannel(ctx context.Context, tx *sql.Tx, now int64, actor uint64, name, kind, topic string, members []uint64, e2e bool) (proto.Channel, proto.Event, error) {
	var dmkey any
	if kind == proto.ChanDM {
		dmkey = dmKey(members)
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO channels (name, kind, topic, created, dmkey, e2e, creator) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, kind, topic, now, dmkey, e2e, actor)
	if err != nil {
		return proto.Channel{}, proto.Event{}, err
	}
	id, _ := r.LastInsertId()
	c := proto.Channel{ID: uint64(id), Name: name, Kind: kind, Topic: topic, Members: members, Created: now, E2E: e2e, Creator: actor}
	if c.Members == nil {
		c.Members = []uint64{}
	}
	ev, err := appendEvent(ctx, tx, proto.KChanNew, c.ID, actor, 0, now, c)
	if err != nil {
		return c, ev, err
	}
	for _, m := range members {
		if _, err := tx.ExecContext(ctx, `INSERT INTO membership (chan, user, join_seq) VALUES (?, ?, ?)`, c.ID, m, ev.Seq); err != nil {
			return c, ev, err
		}
	}
	return c, ev, nil
}

func dmKey(members []uint64) string {
	ids := slices.Clone(members)
	slices.Sort(ids)
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(id, 10)
	}
	return strings.Join(parts, ",")
}

// addMember appends a chan.join event and opens a membership interval.
func addMember(ctx context.Context, tx *sql.Tx, now int64, actor, ch, user uint64) (proto.Event, error) {
	c, err := getChannel(ctx, tx, ch)
	if err != nil {
		return proto.Event{}, err
	}
	c.Members = append(c.Members, user)
	slices.Sort(c.Members)
	ev, err := appendEvent(ctx, tx, proto.KChanJoin, ch, actor, user, now, proto.ChanJoin{User: user, Chan: c})
	if err != nil {
		return ev, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO membership (chan, user, join_seq) VALUES (?, ?, ?)`, ch, user, ev.Seq)
	return ev, err
}

func joinGeneral(ctx context.Context, tx *sql.Tx, now int64, user uint64) (proto.Event, bool, error) {
	var ch uint64
	err := tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE name = 'general' AND kind = 'public'`).Scan(&ch)
	if errors.Is(err, sql.ErrNoRows) {
		return proto.Event{}, false, nil
	}
	if err != nil {
		return proto.Event{}, false, err
	}
	ev, err := addMember(ctx, tx, now, user, ch, user)
	return ev, err == nil, err
}

func (s *Server) opChanCreate(ctx context.Context, sess *session, a proto.ChanCreateArgs) (any, error) {
	u := sess.user
	switch {
	case a.E2E && !sess.e2e:
		// Silently making a plaintext channel would mislead the client.
		return nil, badRequest("encrypted channels need cap e2e1")
	case a.E2E && !a.Private:
		return nil, badRequest("only private channels can be encrypted")
	}
	if !proto.ValidName(a.Name) {
		return nil, badRequest("channel name must match ^[a-z0-9][a-z0-9_-]{0,31}$")
	}
	if !validLine(a.Topic, proto.MaxTopic) {
		return nil, badRequest("invalid topic")
	}
	kind := proto.ChanPublic
	if a.Private {
		kind = proto.ChanPrivate
	}
	var res proto.ChanRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		var n int
		tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels WHERE name = ? AND kind != 'dm'`, a.Name).Scan(&n)
		if n > 0 {
			return nil, proto.Errf(proto.ErrConflict, "channel exists")
		}
		c, ev, err := createChannel(ctx, tx, s.now(), u.ID, a.Name, kind, a.Topic, []uint64{u.ID}, a.E2E)
		res.Chan = c
		return []proto.Event{ev}, err
	})
	return res, err
}

// opChanAdd implements chan.join (self, public only) and chan.add.
func (s *Server) opChanAdd(ctx context.Context, u proto.User, a proto.ChanAddArgs, join bool) (any, error) {
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		kind, ok := s.kinds[a.Chan]
		if !ok {
			return nil, errNotFound
		}
		switch {
		case kind == proto.ChanDM:
			return nil, errForbidden
		case join && kind != proto.ChanPublic:
			return nil, errForbidden
		case !join && !s.members[a.Chan][u.ID]:
			return nil, errForbidden
		}
		if s.members[a.Chan][a.User] {
			return nil, nil
		}
		target, err := getUser(ctx, tx, a.User)
		if err != nil || target.Disabled {
			return nil, errNotFound
		}
		ev, err := addMember(ctx, tx, s.now(), u.ID, a.Chan, a.User)
		return []proto.Event{ev}, err
	})
	return proto.Empty{}, err
}

func (s *Server) opChanLeave(ctx context.Context, u proto.User, a proto.ChanArgs) (any, error) {
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		if s.kinds[a.Chan] == proto.ChanDM {
			return nil, errForbidden
		}
		if !s.members[a.Chan][u.ID] {
			return nil, nil
		}
		ev, err := appendEvent(ctx, tx, proto.KChanLeave, a.Chan, u.ID, u.ID, s.now(), proto.UserRef{User: u.ID})
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE membership SET leave_seq = ? WHERE chan = ? AND user = ? AND leave_seq IS NULL`,
			ev.Seq, a.Chan, u.ID)
		return []proto.Event{ev}, err
	})
	return proto.Empty{}, err
}

func (s *Server) opChanTopic(ctx context.Context, u proto.User, a proto.ChanTopicArgs) (any, error) {
	if !validLine(a.Topic, proto.MaxTopic) {
		return nil, badRequest("invalid topic")
	}
	var res proto.SeqRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		if !s.members[a.Chan][u.ID] || s.kinds[a.Chan] == proto.ChanDM {
			return nil, errForbidden
		}
		if _, err := tx.ExecContext(ctx, `UPDATE channels SET topic = ? WHERE id = ?`, a.Topic, a.Chan); err != nil {
			return nil, err
		}
		c, err := getChannel(ctx, tx, a.Chan)
		if err != nil {
			return nil, err
		}
		ev, err := appendEvent(ctx, tx, proto.KChanUpdate, a.Chan, u.ID, 0, s.now(), c)
		res.Seq = ev.Seq
		return []proto.Event{ev}, err
	})
	return res, err
}

// opDMOpen creates DMs encrypted (epoch 0 until the first key.rotate) when
// the caller speaks e2e1; an existing DM is returned as it is.
func (s *Server) opDMOpen(ctx context.Context, sess *session, a proto.DMOpenArgs) (any, error) {
	u := sess.user
	if len(a.Users) > proto.MaxDMOthers+1 {
		return nil, badRequest("a DM needs 1 to 7 other people")
	}
	members := []uint64{u.ID}
	for _, id := range a.Users {
		if id != u.ID && !slices.Contains(members, id) {
			members = append(members, id)
		}
	}
	// A DM with nobody else is the caller's private notes.
	if len(members) > proto.MaxDMOthers+1 {
		return nil, badRequest("a DM can have at most 7 other people")
	}
	slices.Sort(members)
	var res proto.ChanRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		var id uint64
		err := tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE dmkey = ?`, dmKey(members)).Scan(&id)
		if err == nil {
			res.Chan, err = getChannel(ctx, tx, id)
			return nil, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		for _, m := range members {
			if usr, err := getUser(ctx, tx, m); err != nil || usr.Disabled {
				return nil, errNotFound
			}
		}
		// Private notes (a DM with only yourself) stay unencrypted for now:
		// the server writes reminders into them (docs/E2E.md step 7).
		c, ev, err := createChannel(ctx, tx, s.now(), u.ID, "", proto.ChanDM, "", members, sess.e2e && len(members) > 1)
		res.Chan = c
		return []proto.Event{ev}, err
	})
	return res, err
}

// ---- admin ----

func (s *Server) opInviteCreate(ctx context.Context, u proto.User, a proto.InviteCreateArgs) (any, error) {
	if !u.Admin {
		return nil, errForbidden
	}
	if a.User != 0 {
		target, err := getUser(ctx, s.st.db, a.User)
		if err != nil {
			return nil, errNotFound
		}
		if target.Bot {
			return nil, badRequest("bots can't log in")
		}
	}
	code, err := s.CreateInvite(ctx, false, a.User)
	return proto.InviteRes{Code: code}, err
}

func (s *Server) opUserDisable(ctx context.Context, u proto.User, a proto.UserArgs) (any, error) {
	if !u.Admin {
		return nil, errForbidden
	}
	if a.User == u.ID {
		return nil, badRequest("can't disable yourself")
	}
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		target, err := getUser(ctx, tx, a.User)
		if err != nil {
			return nil, errNotFound
		}
		if target.Disabled {
			return nil, nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled = 1 WHERE id = ?`, a.User); err != nil {
			return nil, err
		}
		// Their webhooks (made by them, or posting as them) die with them.
		if _, err := tx.ExecContext(ctx, `DELETE FROM hooks WHERE created_by = ? OR bot = ?`, a.User, a.User); err != nil {
			return nil, err
		}
		if err := retireOrphanBots(tx); err != nil {
			return nil, err
		}
		target.Disabled = true
		ev, err := appendEvent(ctx, tx, proto.KUserUpdate, 0, u.ID, a.User, s.now(), target)
		if err != nil {
			return nil, err
		}
		gevs, err := s.pruneFromGroups(ctx, tx, u, a.User)
		if err != nil {
			return nil, err
		}
		// Still under s.mu: no new session can register for this user and
		// existing ones stop receiving before the lock is released.
		s.disabled[a.User] = true
		for sess := range s.sessions {
			if sess.user.ID == a.User {
				sess.revoke()
			}
		}
		return append([]proto.Event{ev}, gevs...), nil
	})
	return proto.Empty{}, err
}
