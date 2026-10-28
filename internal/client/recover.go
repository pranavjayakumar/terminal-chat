package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/trust"
)

// Recovering an encrypted channel after its server was restored from a
// backup (docs/E2E.md "Server restored from a backup"). A restore loses the
// epochs committed after the backup, so every device that verified one sees
// a rollback, and once someone recovers and rotates, a fork. An honest
// restore and a server hiding a membership change look the same, so nothing
// here happens by itself: the user previews what accepting discards
// (ChainRecoveryPlan) and confirms it (RecoverChain), which is audited.
// Accepting is allowed only where the server's chain is exactly a chain
// this device verified, cut short (a strict prefix), or that prefix
// extended by valid epochs whose first rotator this device still counts as
// a member (another member's recovery); anything else stays an alarm.

var (
	// ErrRecoveryChanged means the server's chain (or this device's) isn't
	// what the confirmed preview showed any more.
	ErrRecoveryChanged = errors.New("the key chain changed since it was shown — preview again (/alarm-recover)")
	// ErrNotRecoverable means the server's chain can't be accepted: it isn't
	// a restore of the chain this device verified.
	ErrNotRecoverable = errors.New("can't accept the server's chain")
)

// ChainRecovery is what accepting the server's key chain for an encrypted
// channel in alarm would do. RecoverChain does it only if a fresh look
// finds exactly the same.
type ChainRecovery struct {
	Chan     uint64
	Head     uint32 // the newest epoch this device verified
	Ancestor uint32 // the newest epoch the server's chain shares with this device (the very same bundle)
	Top      uint32 // the server's newest epoch
	// Fork: the server has epochs Ancestor+1..Top that this device's chain
	// doesn't (a member already recovered and rotated); Rotator made the
	// first of them, and accepting follows them. Otherwise the server's
	// chain is a strict prefix (Top == Ancestor) and this device starts a
	// fresh key right after rewinding.
	Fork    bool
	Rotator uint64
	// Roster gets the key afterwards: the server's newest epoch's (fork)
	// or the members the fresh key goes to (prefix). Readded are in it but
	// weren't in this device's newest verified epoch (e.g. removed after
	// the backup was taken); Dropped the reverse.
	Roster, Readded, Dropped []uint64

	headHash, ancHash, topHash [32]byte
}

// Discarded is how many epochs this device verified that accepting drops.
func (r ChainRecovery) Discarded() uint32 { return r.Head - r.Ancestor }

func (r ChainRecovery) same(o ChainRecovery) bool {
	return r.Chan == o.Chan && r.Head == o.Head && r.Ancestor == o.Ancestor && r.Top == o.Top &&
		r.Fork == o.Fork && r.Rotator == o.Rotator && r.headHash == o.headHash && r.ancHash == o.ancHash &&
		r.topHash == o.topHash && slices.Equal(r.Roster, o.Roster) && slices.Equal(r.Readded, o.Readded) &&
		slices.Equal(r.Dropped, o.Dropped)
}

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrNotRecoverable, fmt.Sprintf(format, a...))
}

// ChainRecoveryPlan looks at the server's key chain for ch, in alarm, and
// says what accepting it would do, or (ErrNotRecoverable) why it can't be
// accepted. It changes nothing. Online only.
func (c *Client) ChainRecoveryPlan(ctx context.Context, ch uint64) (ChainRecovery, error) {
	s, sid, err := c.e2eSession(ctx)
	if err != nil {
		return ChainRecovery{}, err
	}
	c.e2eMu.Lock()
	defer c.e2eMu.Unlock()
	return c.chainRecovery(ctx, s, sid, ch)
}

// RecoverChain accepts the server's key chain for ch as want (from
// ChainRecoveryPlan, shown to the user) describes it: it rewinds the pinned
// head to want.Ancestor (trust.RewindChain, which moves the alarm to the
// audit log), then verifies the server's epochs above it (fork) or starts
// a fresh epoch for the current members (prefix). If the chain isn't
// exactly what want shows any more it does nothing (ErrRecoveryChanged).
func (c *Client) RecoverChain(ctx context.Context, want ChainRecovery) error {
	s, sid, err := c.e2eSession(ctx)
	if err != nil {
		return err
	}
	got, err := c.rewindTo(ctx, s, sid, want)
	if err != nil || got.Fork {
		return err
	}
	// The server caps rotations when nothing changed (§13.3): seconds, so
	// wait them out rather than leave the old key in use.
	for {
		err = c.RotateKey(ctx, want.Chan)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.ErrRateLimited {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
			continue
		}
		break
	}
	if err != nil {
		return fmt.Errorf("rewound to epoch %d (the channel works again on its key), but starting a fresh key failed: %w", got.Ancestor, err)
	}
	return nil
}

// rewindTo is RecoverChain up to (not including) the fresh key.
func (c *Client) rewindTo(ctx context.Context, s *session, sid e2ecrypto.ServerID, want ChainRecovery) (ChainRecovery, error) {
	c.e2eMu.Lock()
	defer c.e2eMu.Unlock()
	got, err := c.chainRecovery(ctx, s, sid, want.Chan)
	if err != nil {
		return got, err
	}
	if !got.same(want) {
		return got, ErrRecoveryChanged
	}
	detail := fmt.Sprintf("accepted the server's chain: epochs %d–%d (head %x) discarded, back to epoch %d (%x)",
		got.Ancestor+1, got.Head, got.headHash[:8], got.Ancestor, got.ancHash[:8])
	if got.Fork {
		detail += fmt.Sprintf("; following the server's epochs %d–%d (top %x, first by user %d)",
			got.Ancestor+1, got.Top, got.topHash[:8], got.Rotator)
	}
	if len(got.Readded) > 0 {
		detail += fmt.Sprintf("; readded users %v", got.Readded)
	}
	from := trust.Head{Epoch: got.Head, Hash: got.headHash}
	to := trust.Head{Epoch: got.Ancestor, Hash: got.ancHash}
	if err := c.trust.RewindChain(sid, want.Chan, from, to, c.Me().ID, detail); err != nil {
		if errors.Is(err, trust.ErrChainMoved) || errors.Is(err, trust.ErrNoAlarm) {
			return got, ErrRecoveryChanged
		}
		return got, err
	}
	// Cached bundles above the new head are either the discarded epochs or
	// unverified ones: fetch the server's afresh.
	c.db.Exec(`DELETE FROM epochs WHERE chan = ? AND epoch > ?`, want.Chan, got.Ancestor)
	// Messages sealed under the epochs now accepted were judged against
	// the discarded ones (another bundle of their epoch: invalid). Judge
	// them again; the enc names its bundle, so ones sealed under a
	// discarded epoch stay invalid.
	above := binary.BigEndian.AppendUint32(nil, got.Ancestor)
	c.db.Exec(`UPDATE msgs SET sec = ?, body = '' WHERE chan = ? AND sec = ? AND deleted = 0 AND enc IS NOT NULL
		AND substr(enc, 2, 4) > ?`, SecWaiting, want.Chan, SecInvalid, above)
	c.db.Exec(`UPDATE chans SET epoch = ? WHERE id = ? AND epoch > ?`, got.Top, want.Chan, got.Top)
	c.mu.Lock()
	delete(c.probed, want.Chan)
	c.mu.Unlock()
	defer func() {
		c.kickE2E()
		c.post(Update{Channels: true})
	}()
	c.fetchAhead(ctx, s, sid, want.Chan)
	if cv, ok, err := getChan(c.db, want.Chan); err == nil && ok {
		c.syncChannel(ctx, s, sid, cv)
	}
	if d, ok, _ := c.trust.Alarm(sid, want.Chan); ok {
		return got, fmt.Errorf("rewound to epoch %d, but the server's chain then raised an alarm: %s", got.Ancestor, sanitize.Line(d))
	}
	if got.Fork {
		if h, _, _ := c.trust.ChainHead(sid, want.Chan); h.Epoch < got.Top {
			return got, fmt.Errorf("rewound to epoch %d; the server's epochs up to %d aren't verified yet (they are retried)", got.Ancestor, got.Top)
		}
	}
	return got, nil
}

// fetchServerBundles asks the server for ch's bundles from..to without
// caching them: what it holds for each epoch (more than one: a dishonest
// server) and the key.epoch seqs.
func (c *Client) fetchServerBundles(ctx context.Context, s *session, sid e2ecrypto.ServerID, ch uint64, from, to uint32, out map[uint32][]cachedBundle) error {
	for from <= to {
		hi := min(to, from+proto.MaxKeyFetch-1)
		raw, err := s.call(ctx, proto.OpKeyFetch, proto.KeyFetchArgs{Chan: ch, From: from, To: hi}, nil)
		if err != nil {
			return err
		}
		var res proto.KeyFetchRes
		if err := proto.Unmarshal(raw, &res); err != nil || len(res.Seqs) != len(res.Bundles) {
			return errors.New("bad key.fetch reply")
		}
		last := from - 1
		for i, b := range res.Bundles {
			eb, err := identity.PeekEpochBundle(identity.Signed(b))
			if err != nil || eb.ServerID != sid || eb.Chan != ch || eb.Epoch < from || eb.Epoch > hi {
				continue
			}
			cb := cachedBundle{b: identity.Signed(b), seq: res.Seqs[i]}
			if !slices.ContainsFunc(out[eb.Epoch], func(o cachedBundle) bool { return bytes.Equal(o.b.B, cb.b.B) }) {
				out[eb.Epoch] = append(out[eb.Epoch], cb)
			}
			last = max(last, eb.Epoch)
		}
		if len(res.Bundles) == 0 || last >= hi || last < from {
			from = hi + 1 // nothing (more) there, or a full answer
		} else {
			from = last + 1 // stopped early at the size cap
		}
	}
	return nil
}

// chainRecovery is ChainRecoveryPlan. Caller holds e2eMu.
func (c *Client) chainRecovery(ctx context.Context, s *session, sid e2ecrypto.ServerID, chID uint64) (ChainRecovery, error) {
	r := ChainRecovery{Chan: chID}
	ch, ok, err := getChan(c.db, chID)
	if err != nil {
		return r, err
	}
	if !ok || !ch.E2E {
		return r, errors.New("not an encrypted channel")
	}
	me := c.Me().ID
	if !slices.Contains(ch.Members, me) {
		return r, ErrNotMember
	}
	if _, ok, err := c.trust.Alarm(sid, chID); err != nil {
		return r, err
	} else if !ok {
		return r, ErrNoAlarm
	}
	head, ok, err := c.trust.ChainHead(sid, chID)
	if err != nil {
		return r, err
	}
	if !ok {
		return r, refuse("this device hasn't verified any epoch here, so there is nothing to rewind")
	}
	headEp, ok, err := c.trust.Epoch(sid, chID, head.Epoch)
	if err != nil || !ok {
		return r, fmt.Errorf("epoch %d isn't recorded: %v", head.Epoch, err)
	}
	if !slices.Contains(headEp.Roster, me) {
		return r, refuse("epoch %d, the newest this device verified, doesn't include you: another member has to recover the channel", head.Epoch)
	}
	r.Head, r.headHash = head.Epoch, head.Hash

	// Walk the server's chain down from above our head to the newest epoch
	// whose bundle is the one we verified. Matching hashes there means
	// matching chains below (each bundle commits to the previous).
	server := map[uint32][]cachedBundle{}
	const window = 32 // bundles ≤ 16 KiB: one answer never hits the size cap
	for hi := head.Epoch + maxEpochAhead; hi >= 1 && r.Ancestor == 0; {
		lo := uint32(1)
		if hi > window {
			lo = hi - window + 1
		}
		if err := c.fetchServerBundles(ctx, s, sid, chID, lo, hi, server); err != nil {
			return r, err
		}
		for e := hi; e >= lo; e-- {
			bs := server[e]
			if len(bs) > 1 {
				return r, refuse("the server holds %d different bundles for epoch %d", len(bs), e)
			}
			if len(bs) == 1 && r.Top == 0 {
				r.Top = e
			}
			if len(bs) == 1 && e <= head.Epoch {
				ours, ok, err := c.trust.Epoch(sid, chID, e)
				if err != nil {
					return r, err
				}
				if ok && e2ecrypto.ChainHash(bs[0].b.B) == ours.Hash {
					r.Ancestor, r.ancHash = e, ours.Hash
					break
				}
			}
			if e == 1 {
				break
			}
		}
		if lo == 1 {
			break
		}
		hi = lo - 1
	}
	switch {
	case r.Top == 0:
		return r, refuse("the server has no key epoch for this channel")
	case r.Ancestor == 0:
		return r, refuse("the server's chain shares no epoch with the one this device verified")
	case r.Ancestor == head.Epoch:
		return r, refuse("the server's chain still includes epoch %d, the newest this device verified: this alarm isn't a restore (/alarm explains it)", head.Epoch)
	}
	r.topHash = e2ecrypto.ChainHash(server[r.Top][0].b.B)
	anc, ok, err := c.trust.Epoch(sid, chID, r.Ancestor)
	if err != nil || !ok {
		return r, fmt.Errorf("epoch %d isn't recorded: %v", r.Ancestor, err)
	}

	if r.Top == r.Ancestor {
		// A strict prefix: the fresh key goes to the members rosterNow
		// will pick once the head is the ancestor.
		r.Roster = slices.DeleteFunc(c.liveMembers(ch), func(u uint64) bool { return !slices.Contains(anc.Roster, u) })
	} else {
		// The server's chain goes on past the ancestor with other epochs:
		// they must verify exactly as if this device had never seen its
		// own, and the first must come from someone this device's newest
		// epoch still counts as a member — not a member removed in an
		// epoch accepting would discard.
		r.Fork = true
		prev := anc
		for e := r.Ancestor + 1; e <= r.Top; e++ {
			bs := server[e]
			if len(bs) == 0 {
				return r, refuse("the server's chain has no epoch %d", e)
			}
			eb, err := c.validateBundle(sid, ch, e, &prev, bs[0])
			var soft errSoft
			if errors.As(err, &soft) {
				return r, fmt.Errorf("can't judge the server's epoch %d yet (%s): try again shortly", e, soft.msg)
			}
			if err != nil {
				return r, refuse("the server's epoch %d doesn't verify: %v", e, err)
			}
			if e == r.Ancestor+1 {
				by, _, _ := c.trust.Device(sid, eb.By)
				r.Rotator = by.User
				if !slices.Contains(headEp.Roster, by.User) {
					return r, refuse("the server's epoch %d was made by someone epoch %d (the newest this device verified) doesn't include", e, head.Epoch)
				}
			}
			prev = trust.Epoch{Epoch: e, Hash: e2ecrypto.ChainHash(bs[0].b.B), Roster: eb.Roster}
		}
		r.Roster = slices.Clone(prev.Roster)
	}
	slices.Sort(r.Roster)
	for _, u := range r.Roster {
		if !slices.Contains(headEp.Roster, u) {
			r.Readded = append(r.Readded, u)
		}
	}
	for _, u := range headEp.Roster {
		if !slices.Contains(r.Roster, u) {
			r.Dropped = append(r.Dropped, u)
		}
	}
	return r, nil
}
