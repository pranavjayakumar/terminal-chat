package client

import (
	"bytes"
	"context"
	"crypto/rand"
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
//
// The pinned head never rests on the shared epoch: its roster may hold
// members the discarded epochs removed, who could then rotate or post as
// members. The rewind commits, in one trust transaction, together with
// exactly the epochs the user was shown (fork) or with this device's own
// fresh epoch for exactly the members shown, already accepted by the server
// (prefix).

var (
	// ErrRecoveryChanged means the server's chain (or this device's, or the
	// members) isn't what the confirmed preview showed any more.
	ErrRecoveryChanged = errors.New("the key chain changed since it was shown — preview again (/alarm-recover)")
	// ErrNotRecoverable means the server's chain can't be accepted: it isn't
	// a restore of the chain this device verified.
	ErrNotRecoverable = errors.New("can't accept the server's chain")
)

// maxRewind bounds how far below the head the shared epoch is looked for.
const maxRewind = 1024

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
	// first of them, and accepting follows exactly them. Otherwise the
	// server's chain is a strict prefix (Top == Ancestor) and this device
	// commits a fresh epoch Ancestor+1 for Roster.
	Fork    bool
	Rotator uint64
	// Roster gets the key afterwards: the server's newest epoch's (fork)
	// or the members the fresh key goes to (prefix). Readded held a key of
	// an accepted epoch (any of them, for a fork) but weren't in this
	// device's newest verified epoch (e.g. removed after the backup was
	// taken); Dropped were in it and aren't in Roster.
	Roster, Readded, Dropped []uint64

	headHash, ancHash, topHash [32]byte
	branch                     []trust.Epoch // fork: the verified epochs Ancestor+1..Top
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

// ChainRecoveryPlan looks at the server's key chain for ch, in a rollback
// or fork alarm, and says what accepting it would do, or (ErrNotRecoverable)
// why it can't be accepted. It changes nothing. Online only.
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
// ChainRecoveryPlan, shown to the user) describes it. Fork: the pinned head
// moves to exactly the previewed epochs. Prefix: this device first commits
// a fresh epoch on the shared one for exactly the previewed members (the
// server refuses it if they changed), then pins it. Either way the head is
// rewound and moved on in one transaction (trust.RewindChain, which moves
// the alarm to the audit log). If anything isn't what want shows any more
// it changes nothing (ErrRecoveryChanged).
func (c *Client) RecoverChain(ctx context.Context, want ChainRecovery) error {
	s, sid, err := c.e2eSession(ctx)
	if err != nil {
		return err
	}
	for {
		err := c.recoverOnce(ctx, s, sid, want)
		// The server caps rotations when nothing changed (§13.3): seconds,
		// so wait them out (not holding e2eMu).
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.ErrRateLimited {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (c *Client) recoverOnce(ctx context.Context, s *session, sid e2ecrypto.ServerID, want ChainRecovery) error {
	c.e2eMu.Lock()
	defer c.e2eMu.Unlock()
	got, err := c.chainRecovery(ctx, s, sid, want.Chan)
	if err != nil {
		return err
	}
	if !got.same(want) {
		return ErrRecoveryChanged
	}
	then := got.branch
	if !got.Fork {
		e, err := c.commitFresh(ctx, s, sid, got)
		var pe *proto.Error
		if errors.As(err, &pe) && pe.Code == proto.ErrStaleEpoch {
			return ErrRecoveryChanged // the members or the server's chain moved
		}
		if err != nil {
			return err
		}
		then = []trust.Epoch{e}
	}
	detail := fmt.Sprintf("accepted the server's chain: epochs %d–%d (head %x) discarded, back to epoch %d (%x)",
		got.Ancestor+1, got.Head, got.headHash[:8], got.Ancestor, got.ancHash[:8])
	last := then[len(then)-1]
	if got.Fork {
		detail += fmt.Sprintf("; following the server's epochs %d–%d (top %x, first by user %d)",
			got.Ancestor+1, got.Top, last.Hash[:8], got.Rotator)
	} else {
		detail += fmt.Sprintf("; fresh epoch %d (%x) by this device", last.Epoch, last.Hash[:8])
	}
	if len(got.Readded) > 0 {
		detail += fmt.Sprintf("; readded users %v", got.Readded)
	}
	from := trust.Head{Epoch: got.Head, Hash: got.headHash}
	to := trust.Head{Epoch: got.Ancestor, Hash: got.ancHash}
	if err := c.trust.RewindChain(sid, want.Chan, from, to, then, c.Me().ID, detail); err != nil {
		if errors.Is(err, trust.ErrChainMoved) || errors.Is(err, trust.ErrNoAlarm) {
			return ErrRecoveryChanged
		}
		return err
	}
	if !got.Fork {
		_ = c.trust.PayRotation(sid, want.Chan, last.Boxed)
	}
	// Cached bundles above the shared epoch are the discarded epochs or
	// ones now verified: sync fetches whatever comes next afresh.
	c.db.Exec(`DELETE FROM epochs WHERE chan = ? AND epoch > ?`, want.Chan, got.Ancestor)
	c.db.Exec(`UPDATE chans SET epoch = ? WHERE id = ?`, last.Epoch, want.Chan)
	// Messages sealed under the accepted epochs were judged against the
	// discarded ones (another bundle of their epoch: invalid). Judge them
	// again; the enc names its bundle, so ones sealed under a discarded
	// epoch stay invalid.
	above := binary.BigEndian.AppendUint32(nil, got.Ancestor)
	c.db.Exec(`UPDATE msgs SET sec = ?, body = '' WHERE chan = ? AND sec = ? AND deleted = 0 AND enc IS NOT NULL
		AND substr(enc, 2, 4) > ?`, SecWaiting, want.Chan, SecInvalid, above)
	c.mu.Lock()
	delete(c.probed, want.Chan)
	c.mu.Unlock()
	c.kickE2E()
	c.post(Update{Channels: true})
	return nil
}

// commitFresh builds epoch r.Ancestor+1 on the shared epoch for r.Roster,
// sealed to their verified devices, and has the server commit it. The
// trust store isn't touched: the caller pins it with the rewind.
func (c *Client) commitFresh(ctx context.Context, s *session, sid e2ecrypto.ServerID, r ChainRecovery) (trust.Epoch, error) {
	me := s.myDevice()
	if me == nil {
		return trust.Epoch{}, errors.New("this device isn't registered yet")
	}
	anc, ok, err := c.trust.Epoch(sid, r.Chan, r.Ancestor)
	if err != nil || !ok {
		return trust.Epoch{}, fmt.Errorf("epoch %d isn't recorded: %v", r.Ancestor, err)
	}
	if anc.Key == nil {
		return trust.Epoch{}, refuse("this device has no key for epoch %d, so it can't start the next one", r.Ancestor)
	}
	c.verifyDevices(sid, s.sshPub, c.Me().ID)
	p := identity.EpochParams{ServerID: sid, Chan: r.Chan, Epoch: r.Ancestor + 1, Time: time.Now().UnixMilli(),
		Prev: anc.Hash, PrevK: anc.Key, Roster: slices.Clone(r.Roster)}
	var unverified UnverifiedMembersError
	for _, u := range p.Roster {
		devs, err := c.trust.Devices(sid, u)
		if err != nil {
			return trust.Epoch{}, err
		}
		n := 0
		for _, d := range devs {
			if !d.Revoked {
				p.Recipients = append(p.Recipients, identity.Recipient{Dev: d.ID, X25519: d.X25519})
				n++
			}
		}
		if n == 0 && c.hasUnverifiedDevice(sid, u) {
			unverified.Users = append(unverified.Users, u)
			unverified.Names = append(unverified.Names, "@"+sanitize.Line(c.User(u).Name))
		}
	}
	if len(unverified.Users) > 0 {
		return trust.Epoch{}, &unverified
	}
	k, err := e2ecrypto.NewEpochKey(rand.Reader)
	if err != nil {
		return trust.Epoch{}, err
	}
	p.K = k
	b, err := identity.NewEpochBundle(rand.Reader, me, p)
	if err != nil {
		return trust.Epoch{}, fmt.Errorf("building epoch bundle: %w", err)
	}
	raw, err := s.call(ctx, proto.OpKeyRotate, proto.KeyRotateArgs{Chan: r.Chan, Bundle: proto.Signed(b)}, nil)
	if err != nil {
		return trust.Epoch{}, err
	}
	var res struct {
		Seq uint64 `msgpack:"seq"`
	}
	_ = proto.Unmarshal(raw, &res)
	eb, err := identity.PeekEpochBundle(b)
	if err != nil {
		return trust.Epoch{}, err
	}
	return epochOf(eb, b, res.Seq, &k), nil
}

// epochOf is what the trust store records for a verified bundle.
func epochOf(eb identity.EpochBundle, b identity.Signed, seq uint64, k *e2ecrypto.Key) trust.Epoch {
	e := trust.Epoch{Epoch: eb.Epoch, Hash: e2ecrypto.ChainHash(b.B), KC: eb.KC, Roster: eb.Roster, Key: k, Seq: seq}
	if eb.Back != nil {
		e.Back = eb.Back[:]
	}
	e.Boxed = make([]e2ecrypto.DevID, 0, len(eb.Boxes))
	for _, bx := range eb.Boxes {
		e.Boxed = append(e.Boxed, bx.Dev)
	}
	return e
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
	if d, ok, err := c.trust.Alarm(sid, chID); err != nil {
		return r, err
	} else if !ok {
		return r, ErrNoAlarm
	} else if !chainAlarm(d) {
		// Hard evidence (a forged bundle, a key sealed to a non-member)
		// isn't what a restore leaves: never let it be accepted away.
		return r, refuse("this alarm isn't a rollback or fork (/alarm explains it)")
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
	floor := uint32(1)
	if head.Epoch > maxRewind {
		floor = head.Epoch - maxRewind
	}
	for hi := head.Epoch + maxEpochAhead; hi >= floor && r.Ancestor == 0; {
		lo := floor
		if hi >= floor+window {
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
			if e == lo {
				break
			}
		}
		if lo == floor {
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

	keyHolders := map[uint64]bool{} // in a roster of an epoch accepting pins
	if r.Top == r.Ancestor {
		// A strict prefix: the fresh epoch goes to the current members the
		// shared epoch includes (what rosterNow picks; the server refuses
		// a roster other than its members).
		r.Roster = slices.DeleteFunc(c.liveMembers(ch), func(u uint64) bool { return !slices.Contains(anc.Roster, u) })
		if !slices.Contains(r.Roster, me) {
			return r, refuse("you aren't in epoch %d's roster on the server", r.Ancestor)
		}
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
			for _, u := range eb.Roster {
				keyHolders[u] = true
			}
			prev = epochOf(eb, bs[0].b, bs[0].seq, openBox(sid, eb, s.myDevice()))
			r.branch = append(r.branch, prev)
		}
		r.Roster = slices.Clone(prev.Roster)
	}
	slices.Sort(r.Roster)
	for _, u := range r.Roster {
		keyHolders[u] = true
	}
	for u := range keyHolders {
		if !slices.Contains(headEp.Roster, u) {
			r.Readded = append(r.Readded, u)
		}
	}
	slices.Sort(r.Readded)
	for _, u := range headEp.Roster {
		if !slices.Contains(r.Roster, u) {
			r.Dropped = append(r.Dropped, u)
		}
	}
	return r, nil
}
