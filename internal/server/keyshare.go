package server

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// Key requests (PROTOCOL.md §13.7). The server only relays: it can't check
// that a key.share holds the right K (only kc can), so clients verify.

// keyNeedEvery is the least time between two key.need of one session for
// one channel.
const keyNeedEvery = 30 * time.Second

// maxKeyNeedChans bounds a session's per-channel key.need timestamps (the
// rate limit is charged before the channel is checked, so ids are arbitrary).
const maxKeyNeedChans = 1024

// keyNeed relays an ephemeral key.need from sess to the e2e1 sessions of
// the channel's members (the sender's other devices included). Dropped
// silently unless dev is an active device of the sender, the channel is
// encrypted, epoch ≤ its current epoch and the sender is a member. The
// per-session rate limit is checked (and charged) before any database
// query, so a flood of key.need costs the server no queries.
func (s *Server) keyNeed(sess *session, e proto.Ephemeral) {
	if !sess.e2e || len(e.Dev) != e2ecrypto.IDSize || e.Epoch == 0 || e.Chan == 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	if now.Sub(sess.lastKeyNeed[e.Chan]) < keyNeedEvery {
		s.mu.Unlock()
		return
	}
	if len(sess.lastKeyNeed) >= maxKeyNeedChans {
		for ch, t := range sess.lastKeyNeed {
			if now.Sub(t) >= keyNeedEvery {
				delete(sess.lastKeyNeed, ch)
			}
		}
		if len(sess.lastKeyNeed) >= maxKeyNeedChans {
			s.mu.Unlock()
			return
		}
	}
	sess.lastKeyNeed[e.Chan] = now
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Reads first, outside the hub lock.
	var owner uint64
	var revoked []byte
	if err := s.st.db.QueryRowContext(ctx, `SELECT user, revoked FROM devices WHERE dev = ?`, e.Dev).
		Scan(&owner, &revoked); err != nil || owner != sess.user.ID || revoked != nil {
		return
	}
	head, err := chanEpoch(ctx, s.st.db, e.Chan)
	if err != nil || !head.e2e || e.Epoch > head.epoch {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.members[e.Chan][e.User] {
		return
	}
	out := proto.Ephemeral{Kind: proto.EphKeyNeed, Chan: e.Chan, User: e.User, Epoch: e.Epoch, Dev: e.Dev}
	payload, err := proto.Marshal(out)
	if err != nil {
		return
	}
	for to := range s.sessions {
		if to != sess && to.e2e && s.members[e.Chan][to.user.ID] {
			to.pushLive(outFrame{t: proto.TEphemeral, payload: payload})
		}
	}
}

// opKeyShare relays K sealed to one device as a key.share event, visible to
// the channel's e2e1 members.
func (s *Server) opKeyShare(ctx context.Context, u proto.User, a proto.KeyShareArgs) (any, error) {
	if len(a.Dev) != e2ecrypto.IDSize || len(a.Eph) != 32 || len(a.CT) != e2ecrypto.BoxCTSize || a.Epoch == 0 {
		return nil, badRequest("malformed key share")
	}
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		if _, ok := s.kinds[a.Chan]; !ok {
			return nil, errNotFound
		}
		if !s.members[a.Chan][u.ID] {
			return nil, errForbidden
		}
		head, err := chanEpoch(ctx, tx, a.Chan)
		if err != nil {
			return nil, err
		}
		if !head.e2e {
			return nil, badRequest("channel is not encrypted")
		}
		if a.Epoch > head.epoch {
			return nil, badRequest("no such epoch")
		}
		var owner uint64
		var revoked []byte
		err = tx.QueryRowContext(ctx, `SELECT user, revoked FROM devices WHERE dev = ?`, a.Dev).Scan(&owner, &revoked)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, errNotFound
		case err != nil:
			return nil, err
		case revoked != nil:
			return nil, badRequest("device is revoked")
		case !s.members[a.Chan][owner]:
			return nil, badRequest("device of a non-member")
		}
		ev, err := appendEvent(ctx, tx, proto.KKeyShare, a.Chan, u.ID, 0, s.now(), a)
		if err != nil {
			return nil, err
		}
		return []proto.Event{ev}, nil
	})
	return proto.Empty{}, err
}
