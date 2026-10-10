package server

import (
	"context"
	"database/sql"

	"github.com/skailama/tc/internal/proto"
)

// Membership changes (PROTOCOL.md §5, §13.5). In an encrypted channel with
// an epoch, adding or removing someone commits the next epoch bundle (whose
// roster is the new member set) in the same transaction as the membership
// change, so no message can be sealed in between: the events are
// chan.join/chan.leave, then key.epoch. A leave can't carry one (the
// leaver would know K): the channel's bundle is then stale and the next
// sender rotates (§13.4).

func checkBundleSize(b *proto.Signed) error {
	if b != nil && signedSize(*b) > proto.MaxBundle {
		return proto.Errf(proto.ErrTooLarge, "epoch bundles are limited to 16 KiB")
	}
	return nil
}

// errNeedBundle: a membership change in an encrypted channel without the
// next epoch's bundle (old clients can't make one).
var errNeedBundle = badRequest("this channel is end-to-end encrypted: changing its members needs a key bundle (upgrade tc)")

// opChanAdd implements chan.join (self, public only) and chan.add.
func (s *Server) opChanAdd(ctx context.Context, u proto.User, a proto.ChanAddArgs, join bool) (any, error) {
	if err := checkBundleSize(a.Bundle); err != nil {
		return nil, err
	}
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
		head, err := chanEpoch(ctx, tx, a.Chan)
		if err != nil {
			return nil, err
		}
		if !head.e2e && a.Bundle != nil {
			return nil, badRequest("channel is not encrypted")
		}
		if s.members[a.Chan][a.User] {
			return nil, nil
		}
		target, err := getUser(ctx, tx, a.User)
		if err != nil || target.Disabled {
			return nil, errNotFound
		}
		switch {
		case head.e2e && head.epoch > 0 && a.Bundle == nil:
			// At epoch 0 there is no key to keep from anyone: the first
			// bundle (the creator's) will include the new member.
			return nil, errNeedBundle
		}
		ev, err := addMember(ctx, tx, s.now(), u.ID, a.Chan, a.User)
		if err != nil || a.Bundle == nil {
			return []proto.Event{ev}, err
		}
		kev, err := s.rotateTx(ctx, tx, u, a.Chan, *a.Bundle)
		if err != nil {
			return nil, err
		}
		return []proto.Event{ev, kev}, nil
	})
	return proto.Empty{}, err
}

// leaveTx appends a chan.leave event for user (actor: who did it) and
// closes their membership interval.
func leaveTx(ctx context.Context, tx *sql.Tx, now int64, actor, ch, user uint64) (proto.Event, error) {
	ev, err := appendEvent(ctx, tx, proto.KChanLeave, ch, actor, user, now, proto.UserRef{User: user})
	if err != nil {
		return ev, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE membership SET leave_seq = ? WHERE chan = ? AND user = ? AND leave_seq IS NULL`,
		ev.Seq, ch, user)
	return ev, err
}

func (s *Server) opChanLeave(ctx context.Context, u proto.User, a proto.ChanArgs) (any, error) {
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		if s.kinds[a.Chan] == proto.ChanDM {
			return nil, errForbidden
		}
		if !s.members[a.Chan][u.ID] {
			return nil, nil
		}
		ev, err := leaveTx(ctx, tx, s.now(), u.ID, a.Chan, u.ID)
		return []proto.Event{ev}, err
	})
	return proto.Empty{}, err
}

// opChanRemove removes someone else from a private channel: its creator
// (while a member) or an admin may. Removing yourself is chan.leave. In an
// encrypted channel with an epoch, a member must send the bundle without
// the removed user, unless the target is disabled and so in no roster
// already. An admin who isn't a member can't make one: their removal goes
// without, like a leave, and the next sender rotates.
func (s *Server) opChanRemove(ctx context.Context, u proto.User, a proto.ChanAddArgs) (any, error) {
	if err := checkBundleSize(a.Bundle); err != nil {
		return nil, err
	}
	if a.User == u.ID {
		if a.Bundle != nil {
			return nil, badRequest("leaving needs no key bundle")
		}
		return s.opChanLeave(ctx, u, proto.ChanArgs{Chan: a.Chan})
	}
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		kind, ok := s.kinds[a.Chan]
		switch {
		case !ok:
			return nil, errNotFound
		case kind == proto.ChanDM:
			return nil, errForbidden
		case kind == proto.ChanPublic:
			return nil, badRequest("nobody can be removed from a public channel")
		}
		var creator uint64
		if err := tx.QueryRowContext(ctx, `SELECT creator FROM channels WHERE id = ?`, a.Chan).Scan(&creator); err != nil {
			return nil, err
		}
		if !u.Admin && (creator != u.ID || !s.members[a.Chan][u.ID]) {
			return nil, proto.Errf(proto.ErrForbidden, "only the channel's creator or an admin can remove members")
		}
		head, err := chanEpoch(ctx, tx, a.Chan)
		if err != nil {
			return nil, err
		}
		if !head.e2e && a.Bundle != nil {
			return nil, badRequest("channel is not encrypted")
		}
		if !s.members[a.Chan][a.User] {
			return nil, nil
		}
		target, err := getUser(ctx, tx, a.User)
		if err != nil {
			return nil, err
		}
		switch {
		case head.e2e && head.epoch > 0 && a.Bundle == nil && !target.Disabled && s.members[a.Chan][u.ID]:
			return nil, errNeedBundle
		case a.Bundle != nil && !s.members[a.Chan][u.ID]:
			return nil, proto.Errf(proto.ErrForbidden, "only members can rotate the channel's key")
		}
		ev, err := leaveTx(ctx, tx, s.now(), u.ID, a.Chan, a.User)
		if err != nil || a.Bundle == nil {
			return []proto.Event{ev}, err
		}
		kev, err := s.rotateTx(ctx, tx, u, a.Chan, *a.Bundle)
		if err != nil {
			return nil, err
		}
		return []proto.Event{ev, kev}, nil
	})
	return proto.Empty{}, err
}

// opChanEncrypt converts a plaintext private channel (by its creator) or a
// DM with others (by any member) to end-to-end encryption, with epoch 1 in
// the same transaction. Its plaintext history stays as it is: legacy, up to
// Channel.legacy (the newest message id now). Events: chan.update (e2e,
// legacy), then key.epoch.
func (s *Server) opChanEncrypt(ctx context.Context, u proto.User, a proto.KeyRotateArgs) (any, error) {
	if err := checkBundleSize(&a.Bundle); err != nil {
		return nil, err
	}
	var res proto.SeqRes
	var hooks []uint64
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		hooks = nil
		kind, ok := s.kinds[a.Chan]
		switch {
		case !ok:
			return nil, errNotFound
		case !s.members[a.Chan][u.ID]:
			return nil, errForbidden
		case kind == proto.ChanPublic:
			return nil, badRequest("only private channels and DMs can be encrypted")
		}
		c, err := getChannel(ctx, tx, a.Chan)
		if err != nil {
			return nil, err
		}
		switch {
		case c.E2E:
			return nil, proto.Errf(proto.ErrConflict, "channel is already encrypted")
		case kind == proto.ChanDM && len(c.Members) < 2:
			// The server posts reminders into private notes (§13.3).
			return nil, badRequest("private notes can't be encrypted")
		case kind != proto.ChanDM && c.Creator != u.ID:
			return nil, proto.Errf(proto.ErrForbidden, "only the channel's creator can encrypt it")
		}
		// Webhooks can't post into an encrypted channel: they go with the
		// conversion, and bots left without a hook retire (as hook.delete).
		hrows, err := tx.QueryContext(ctx, `SELECT id FROM hooks WHERE chan = ?`, a.Chan)
		if err != nil {
			return nil, err
		}
		for hrows.Next() {
			var id uint64
			if err := hrows.Scan(&id); err != nil {
				hrows.Close()
				return nil, err
			}
			hooks = append(hooks, id)
		}
		hrows.Close()
		if _, err := tx.ExecContext(ctx, `DELETE FROM hooks WHERE chan = ?`, a.Chan); err != nil {
			return nil, err
		}
		if err := retireOrphanBots(tx); err != nil {
			return nil, err
		}
		var legacy uint64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM messages WHERE chan = ?`, a.Chan).Scan(&legacy); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE channels SET e2e = 1, legacy = ? WHERE id = ?`, legacy, a.Chan); err != nil {
			return nil, err
		}
		if c, err = getChannel(ctx, tx, a.Chan); err != nil {
			return nil, err
		}
		ev, err := appendEvent(ctx, tx, proto.KChanUpdate, a.Chan, u.ID, 0, s.now(), c)
		if err != nil {
			return nil, err
		}
		kev, err := s.rotateTx(ctx, tx, u, a.Chan, a.Bundle)
		if err != nil {
			return nil, err
		}
		res.Seq = kev.Seq
		return []proto.Event{ev, kev}, nil
	})
	if err != nil {
		return nil, err
	}
	s.hk.mu.Lock()
	for _, id := range hooks {
		delete(s.hk.limits, id)
	}
	s.hk.mu.Unlock()
	return res, nil
}
