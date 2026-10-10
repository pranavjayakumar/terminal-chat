package client

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// Membership changes (PROTOCOL.md §13.5; docs/E2E.md step 9). In an
// encrypted channel with a key, adding or removing someone sends the next
// epoch bundle (whose roster is the new member set, sealed to every
// verified device of it, linked back to the current key so a joiner can
// read the history) with the change itself; the server commits both at
// once. Leaving can't carry one: the next sender rotates (encmsg.go).

// ErrNotMember refuses a key change in an encrypted channel we're not in.
var ErrNotMember = errors.New("only members can change who is in an encrypted channel")

// e2eSession is the live session of a server that speaks e2e1, once this
// device is registered.
func (c *Client) e2eSession(ctx context.Context) (*session, e2ecrypto.ServerID, error) {
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return nil, e2ecrypto.ServerID{}, ErrOffline
	}
	sid, ok := s.serverID()
	if !ok || !s.e2e() {
		return nil, sid, errors.New("server doesn't support end-to-end encryption")
	}
	select {
	case <-s.devReady:
	case <-s.done:
		return nil, sid, ErrOffline
	case <-ctx.Done():
		return nil, sid, ctx.Err()
	}
	return s, sid, nil
}

func isStale(err error) bool {
	var pe *proto.Error
	return errors.As(err, &pe) && pe.Code == proto.ErrStaleEpoch
}

// AddMember adds user to ch. In an encrypted channel the add carries the
// next epoch (refused with *UnverifiedMembersError if user has devices but
// none this device verified).
func (c *Client) AddMember(ctx context.Context, ch, user uint64) error {
	if !c.needsEnc(ch) {
		return c.call(ctx, proto.OpChanAdd, proto.ChanAddArgs{Chan: ch, User: user}, nil, nil)
	}
	if cv, ok, err := getChan(c.db, ch); err == nil && ok && slices.Contains(cv.Members, user) {
		return nil
	}
	s, sid, err := c.e2eSession(ctx)
	if err != nil {
		return err
	}
	c.e2eMu.Lock()
	defer c.e2eMu.Unlock()
	// The new member may be someone whose devices we never looked at.
	c.verifyDevices(sid, s.sshPub, c.Me().ID)
	return c.changeMembers(ctx, s, sid, ch, proto.OpChanAdd, user, func(r []uint64) []uint64 { return append(r, user) })
}

// RemoveMember removes user from private channel ch (its creator or an
// admin may; removing yourself is Leave). In an encrypted channel the
// removal carries the next epoch without them, so only a member can do it.
func (c *Client) RemoveMember(ctx context.Context, ch, user uint64) error {
	me := c.Me().ID
	if user == me {
		return c.Leave(ctx, ch)
	}
	args := proto.ChanAddArgs{Chan: ch, User: user}
	if !c.needsEnc(ch) || c.User(user).Disabled { // a disabled user is in no roster already
		return c.call(ctx, proto.OpChanRemove, args, nil, nil)
	}
	cv, ok, err := getChan(c.db, ch)
	if err != nil {
		return err
	}
	if !ok || !slices.Contains(cv.Members, me) {
		return ErrNotMember
	}
	if !slices.Contains(cv.Members, user) {
		return nil
	}
	s, sid, err := c.e2eSession(ctx)
	if err != nil {
		return err
	}
	c.e2eMu.Lock()
	defer c.e2eMu.Unlock()
	return c.changeMembers(ctx, s, sid, ch, proto.OpChanRemove, user, func(r []uint64) []uint64 {
		return slices.DeleteFunc(r, func(u uint64) bool { return u == user })
	})
}

// changeMembers sends op (chan.add / chan.remove) for user with the next
// epoch bundle, retrying once on stale_epoch (someone rotated or the
// members changed meanwhile). Before the channel has any key there is
// nothing to protect: the change goes without one, and epoch 1 (the
// creator's) will have the new roster. Caller holds e2eMu.
func (c *Client) changeMembers(ctx context.Context, s *session, sid e2ecrypto.ServerID, chID uint64, op string, user uint64,
	roster func([]uint64) []uint64) error {
	for attempt := 0; ; attempt++ {
		ch, ok, err := getChan(c.db, chID)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("unknown channel")
		}
		if !slices.Contains(ch.Members, c.Me().ID) {
			return ErrNotMember
		}
		c.syncChannel(ctx, s, sid, ch)
		_, hasHead, err := c.trust.ChainHead(sid, chID)
		if err != nil {
			return err
		}
		if !hasHead && ch.Epoch == 0 && c.cachedMaxEpoch(chID) == 0 {
			_, err := s.call(ctx, op, proto.ChanAddArgs{Chan: chID, User: user}, nil)
			var pe *proto.Error
			if attempt == 0 && errors.As(err, &pe) && pe.Code == proto.ErrBadRequest {
				continue // epoch 1 arrived meanwhile: now it needs a bundle
			}
			return err
		}
		err = c.commitBundle(ctx, s, sid, chID, roster, func(_ proto.Channel, b identity.Signed) error {
			sb := proto.Signed(b)
			_, err := s.call(ctx, op, proto.ChanAddArgs{Chan: chID, User: user, Bundle: &sb}, nil)
			return err
		})
		if attempt == 0 && isStale(err) {
			continue
		}
		return err
	}
}

// EncryptChannel converts plaintext private channel ch (we created it) or
// DM to end-to-end encryption, with epoch 1 sealed to every current
// member. Its existing messages stay readable, marked legacy.
func (c *Client) EncryptChannel(ctx context.Context, ch uint64) error {
	cv, ok, err := getChan(c.db, ch)
	if err != nil {
		return err
	}
	switch {
	case !ok:
		return errors.New("unknown channel")
	case cv.E2E:
		return errors.New("already end-to-end encrypted")
	case cv.Kind == proto.ChanPublic:
		return errors.New("public channels can't be encrypted")
	}
	s, sid, err := c.e2eSession(ctx)
	if err != nil {
		return err
	}
	c.e2eMu.Lock()
	defer c.e2eMu.Unlock()
	c.verifyDevices(sid, s.sshPub, c.Me().ID)
	err = c.commitBundle(ctx, s, sid, ch, nil, func(_ proto.Channel, b identity.Signed) error {
		_, err := s.call(ctx, proto.OpChanEncrypt, proto.KeyRotateArgs{Chan: ch, Bundle: proto.Signed(b)}, nil)
		return err
	})
	if err == nil {
		_ = c.trust.MarkChannelE2E(sid, ch) // our choice: sticky from now on
	}
	return err
}

// DisableUser (admin) disables a user and closes their sessions, then
// rotates every encrypted channel of ours they were in (best effort; any
// other goes stale and its next sender rotates).
func (c *Client) DisableUser(ctx context.Context, user uint64) error {
	if err := c.call(ctx, proto.OpUserDisable, proto.UserArgs{User: user}, nil, nil); err != nil {
		return err
	}
	chans, err := c.memberChannels(c.Me().ID)
	if err != nil {
		return nil
	}
	for _, ch := range chans {
		if ch.E2E && slices.Contains(ch.Members, user) {
			_ = c.RotateKey(ctx, ch.ID)
		}
	}
	return nil
}

// markRemoved applies our removal from private channel ch by someone
// else: unlike a leave, the channel and the history we had stay (until
// Leave closes it) so the UI can say what happened. Its cached bundles and
// shares go; the trust store keeps the keys we had.
func (c *Client) markRemoved(tx *sql.Tx, ch, me, by uint64, at int64) error {
	for _, q := range []string{`DELETE FROM epochs WHERE chan = ?`, `DELETE FROM key_shares WHERE chan = ?`} {
		if _, err := tx.Exec(q, ch); err != nil {
			return err
		}
	}
	if err := setMember(tx, ch, me, false); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT OR REPLACE INTO removed (chan, actor, time) VALUES (?, ?, ?)`, ch, by, at)
	return err
}

func isRemoved(q dbtx, ch uint64) bool {
	var n int
	return q.QueryRow(`SELECT COUNT(*) FROM removed WHERE chan = ?`, ch).Scan(&n) == nil && n > 0
}

// Leave leaves a channel. A channel we were removed from is only closed
// locally (forgetting its history here).
func (c *Client) Leave(ctx context.Context, ch uint64) error {
	if isRemoved(c.db, ch) {
		err := c.tx(func(tx *sql.Tx) error {
			if _, err := tx.Exec(`DELETE FROM removed WHERE chan = ?`, ch); err != nil {
				return err
			}
			return c.forgetChan(tx, ch, c.Me().ID)
		})
		if err == nil {
			u := Update{Channels: true}
			u.msgs(ch)
			c.post(u)
		}
		return err
	}
	return c.call(ctx, proto.OpChanLeave, proto.ChanArgs{Chan: ch}, nil, nil)
}
