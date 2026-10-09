package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/trust"
)

// End-to-end key handling (PROTOCOL.md §13.2, §13.3, §13.7; docs/E2E.md
// "Review r1 → r2"): which devices to trust, which epoch bundles to accept,
// the epoch keys K, key.need/key.share, and rotating.
//
// The cache holds what the server sent (unverified certs, revokes, bundles,
// shares); a worker goroutine per e2e1 session (e2eLoop) moves what verifies
// into the trust store, which a cache reset never touches. Everything that
// reads or advances the trust store's chains runs under e2eMu, so the
// worker and RotateKey never interleave.

// E2EState is an encrypted channel's key state.
type E2EState int

const (
	// E2EOff: the channel isn't end-to-end encrypted.
	E2EOff E2EState = iota
	// E2EOK: the chain verifies and this device holds the current K.
	E2EOK
	// E2EWaitingKey: no key yet (no epoch, no box for us, or kc failed);
	// key.need was or will be sent.
	E2EWaitingKey
	// E2EUnverified: the next bundle's rotator is a device we can't verify
	// (yet), e.g. after an unacknowledged key change.
	E2EUnverified
	// E2EAlarm: the server presented something an honest one can't: a fork,
	// rollback, forged or out-of-roster bundle. Sticky (trust store).
	E2EAlarm
)

func (s E2EState) String() string {
	return [...]string{"off", "ok", "waiting_key", "unverified", "alarm"}[s]
}

// E2EStatus describes a channel's key state. Epoch is the verified head (0
// if none); Detail says why for anything but E2EOK/E2EOff.
type E2EStatus struct {
	State  E2EState
	Epoch  uint32
	Detail string
}

// UnverifiedMembersError is RotateKey's refusal when some members have
// devices but none this device verified: sealing to nobody of theirs would
// cut them off, sealing to an unverified device could hand K to the server.
type UnverifiedMembersError struct {
	Users []uint64
	Names []string
}

func (e *UnverifiedMembersError) Error() string {
	return "can't verify any device of: " + strings.Join(e.Names, ", ")
}

// Bounds on what a server can make the cache hold.
const (
	maxCachedBundles = 1024 // unverified bundles per channel
	maxCachedShares  = 64   // key.share events per channel
)

func (s *session) serverID() (e2ecrypto.ServerID, bool) {
	var sid e2ecrypto.ServerID
	if len(s.welcome.ServerID) != e2ecrypto.IDSize {
		return sid, false
	}
	copy(sid[:], s.welcome.ServerID)
	return sid, true
}

// kickE2E wakes the worker (non-blocking; kicks coalesce).
func (c *Client) kickE2E() {
	select {
	case c.e2eKick <- struct{}{}:
	default:
	}
}

// e2eKinds are the events after which the worker has something to do.
func e2eKind(kind string) bool {
	switch kind {
	case proto.KDeviceNew, proto.KDeviceRevoke, proto.KKeyEpoch, proto.KKeyShare,
		proto.KChanNew, proto.KChanUpdate, proto.KChanJoin, proto.KChanLeave, proto.KUserUpdate:
		return true
	}
	return false
}

// ---- cache: bundles and shares ----

// storeBundle keeps a bundle the server sent for channel ch (0: the
// bundle's own) with its key.epoch seq, and raises the cached epoch.
// Undecodable bundles, or ones naming another channel, are dropped: they
// can't be filed under an epoch.
func storeBundle(q dbtx, ch uint64, b proto.Signed, seq uint64) error {
	eb, err := identity.PeekEpochBundle(identity.Signed(b))
	if err != nil || (ch != 0 && eb.Chan != ch) || seq == 0 {
		return nil
	}
	if cur, err := metaBytes(q, "server_id"); err == nil && len(cur) > 0 && !bytes.Equal(cur, eb.ServerID[:]) {
		return nil
	}
	var n int
	q.QueryRow(`SELECT COUNT(*) FROM epochs WHERE chan = ?`, eb.Chan).Scan(&n)
	if n >= maxCachedBundles {
		return nil
	}
	raw := proto.MustRaw(b)
	h := sha256.Sum256(raw)
	if _, err := q.Exec(`INSERT OR IGNORE INTO epochs (chan, epoch, hash, bundle, seq) VALUES (?, ?, ?, ?, ?)`,
		eb.Chan, eb.Epoch, h[:], []byte(raw), seq); err != nil {
		return err
	}
	_, err = q.Exec(`UPDATE chans SET epoch = max(epoch, ?) WHERE id = ?`, eb.Epoch, eb.Chan)
	return err
}

func storeShare(q dbtx, ch uint64, d proto.KeyShareArgs) error {
	if ch == 0 || len(d.Dev) != e2ecrypto.IDSize || len(d.Eph) != 32 || len(d.CT) != e2ecrypto.BoxCTSize {
		return nil
	}
	var n int
	q.QueryRow(`SELECT COUNT(*) FROM key_shares WHERE chan = ?`, ch).Scan(&n)
	if n >= maxCachedShares {
		return nil
	}
	_, err := q.Exec(`INSERT OR IGNORE INTO key_shares (chan, epoch, dev, eph, ct) VALUES (?, ?, ?, ?, ?)`,
		ch, d.Epoch, d.Dev, d.Eph, d.CT)
	return err
}

type cachedBundle struct {
	b    identity.Signed
	seq  uint64
	hash []byte // cache key
}

func (c *Client) cachedBundles(ch uint64, epoch uint32) ([]cachedBundle, error) {
	rows, err := c.db.Query(`SELECT bundle, seq, hash FROM epochs WHERE chan = ? AND epoch = ? ORDER BY seq, hash`, ch, epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cachedBundle
	for rows.Next() {
		var raw []byte
		var cb cachedBundle
		if err := rows.Scan(&raw, &cb.seq, &cb.hash); err != nil {
			return nil, err
		}
		var s proto.Signed
		if proto.Unmarshal(raw, &s) != nil {
			continue
		}
		cb.b = identity.Signed(s)
		out = append(out, cb)
	}
	return out, rows.Err()
}

// ---- worker ----

// e2eLoop runs for one e2e1 session: register our device, then verify
// whatever arrives and answer key.need until the session ends.
func (c *Client) e2eLoop(ctx context.Context, s *session, signer ssh.Signer) {
	sid, ok := s.serverID()
	if !ok {
		return
	}
	// Best effort: a failure is retried on the next connection.
	_ = c.ensureDevice(ctx, s, signer)
	t := time.NewTicker(c.timing.keyNeed)
	defer t.Stop()
	for {
		c.e2ePass(ctx, s, sid)
		for wait := true; wait; {
			select {
			case <-ctx.Done():
				return
			case <-s.done:
				return
			case e := <-c.keyNeeds:
				c.answerKeyNeed(ctx, s, sid, e)
			case <-c.e2eKick:
				wait = false
			case <-t.C:
				wait = false
			}
		}
	}
}

// myDevice is this session's device keys (nil until ensureDevice loaded them).
func (s *session) myDevice() *e2ecrypto.DeviceKeys { return s.dev.Load() }

// e2ePass verifies new devices, then walks every encrypted channel we're in.
func (c *Client) e2ePass(ctx context.Context, s *session, sid e2ecrypto.ServerID) {
	c.e2eMu.Lock()
	defer c.e2eMu.Unlock()
	me := c.Me().ID
	changed := c.verifyDevices(sid, s.sshPub, me)
	chans, err := c.e2eChannels(sid, me)
	if err != nil {
		return
	}
	for _, ch := range chans {
		if ctx.Err() != nil {
			return
		}
		if c.syncChannel(ctx, s, sid, ch) {
			changed = true
		}
	}
	if changed {
		c.post(Update{Channels: true})
	}
}

// e2eChannels lists the cached channels we're a member of that are, or
// were once seen, encrypted. A channel the trust store remembers as
// encrypted that the server now calls plaintext is an alarm.
func (c *Client) e2eChannels(sid e2ecrypto.ServerID, me uint64) ([]proto.Channel, error) {
	all, err := c.memberChannels(me)
	if err != nil {
		return nil, err
	}
	var out []proto.Channel
	for _, ch := range all {
		if !ch.E2E {
			if was, _ := c.trust.ChannelE2E(sid, ch.ID); was {
				_ = c.trust.RaiseAlarm(sid, ch.ID, "the server says this channel is no longer encrypted")
			}
			continue
		}
		out = append(out, ch)
	}
	return out, nil
}

// memberChannels lists the cached non-public channels me is a member of.
func (c *Client) memberChannels(me uint64) ([]proto.Channel, error) {
	rows, err := c.db.Query(`SELECT ` + chanCols + ` FROM chans WHERE kind != 'public'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []proto.Channel
	for rows.Next() {
		ch, err := scanChan(rows)
		if err != nil {
			return nil, err
		}
		if slices.Contains(ch.Members, me) {
			all = append(all, ch)
		}
	}
	return all, rows.Err()
}

// ServerID is the server's id as cached by the last snapshot; ok is false
// before the first sync. Trust store rows are keyed by it.
func (c *Client) ServerID() (e2ecrypto.ServerID, bool) { return c.cacheServerID() }

func (c *Client) cacheServerID() (e2ecrypto.ServerID, bool) {
	var sid e2ecrypto.ServerID
	b, err := metaBytes(c.db, "server_id")
	if err != nil || len(b) != e2ecrypto.IDSize {
		return sid, false
	}
	copy(sid[:], b)
	return sid, true
}

func (c *Client) setSoft(ch uint64, detail string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.e2eSoft[ch] == detail {
		return false
	}
	if detail == "" {
		delete(c.e2eSoft, ch)
	} else {
		c.e2eSoft[ch] = detail
	}
	return true
}

// ---- device trust ----

type certRow struct {
	raw []byte
	s   identity.Signed
	dc  identity.DeviceCert
}

// verifyDevices moves every cached cert, endorsement and revoke that
// verifies against pinned SSH keys into the trust store (§13.2):
//
//   - our own login key is pinned as known (we hold it);
//   - KeyEndorse signed by a pinned key pins the endorsed key;
//   - a cert under a pinned key is a verified device;
//   - a user's first key is pinned from the first valid cert seen for them
//     (TOFU, in arrival order);
//   - a valid cert under any other key is a pending key change (nothing is
//     sealed to it until acknowledged);
//   - a revoke counts if signed by a pinned key of the device's owner, or by
//     another of their verified devices not revoked before it.
//
// Forged objects are ignored. It reports whether anything changed.
func (c *Client) verifyDevices(sid e2ecrypto.ServerID, mySSH []byte, me uint64) bool {
	ts := c.trust
	changed := false
	if len(mySSH) > 0 {
		if pk, err := ssh.ParsePublicKey(mySSH); err == nil {
			fp := ssh.FingerprintSHA256(pk)
			if err := ts.PinKey(sid, me, fp, mySSH, trust.Verified); errors.Is(err, trust.ErrKeyChanged) {
				// Our own key, so we know it's ours: no banner for it.
				_ = ts.AcknowledgeKeyChange(sid, me, fp)
			}
		}
	}

	var certs []certRow
	var endorse []identity.Signed
	type revRow struct {
		s   identity.Signed
		seq uint64
	}
	var revokes []revRow
	load := func(q string, fn func(raw []byte, seq uint64)) {
		rows, err := c.db.Query(q)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			var seq uint64
			if rows.Scan(&raw, &seq) == nil {
				fn(raw, seq)
			}
		}
	}
	decode := func(raw []byte) (identity.Signed, bool) {
		var s proto.Signed
		if proto.Unmarshal(raw, &s) != nil {
			return identity.Signed{}, false
		}
		return identity.Signed(s), true
	}
	load(`SELECT cert, 0 FROM devices ORDER BY rowid`, func(raw []byte, _ uint64) {
		s, ok := decode(raw)
		if !ok {
			return
		}
		dc, err := identity.PeekDeviceCert(s)
		if err == nil && dc.ServerID == sid && dc.User != 0 {
			certs = append(certs, certRow{raw: raw, s: s, dc: dc})
		}
	})
	load(`SELECT endorse, 0 FROM device_endorse ORDER BY rowid`, func(raw []byte, _ uint64) {
		if s, ok := decode(raw); ok {
			endorse = append(endorse, s)
		}
	})
	load(`SELECT revoke, seq FROM device_revokes ORDER BY rowid`, func(raw []byte, seq uint64) {
		if s, ok := decode(raw); ok {
			revokes = append(revokes, revRow{s, seq})
		}
	})
	pins := map[uint64][][]byte{}
	pinsOf := func(u uint64) [][]byte {
		if p, ok := pins[u]; ok {
			return p
		}
		ks, _ := ts.PinnedKeys(sid, u)
		p := [][]byte{}
		for _, k := range ks {
			p = append(p, k.Wire)
		}
		pins[u] = p
		return p
	}
	pinned := func(u uint64, wire []byte) bool {
		return slices.ContainsFunc(pinsOf(u), func(p []byte) bool { return bytes.Equal(p, wire) })
	}
	fingerprint := func(wire []byte) (string, bool) {
		pk, err := ssh.ParsePublicKey(wire)
		if err != nil {
			return "", false
		}
		return ssh.FingerprintSHA256(pk), true
	}
	put := func(cr certRow) {
		d := trust.Device{ID: cr.dc.Dev, User: cr.dc.User, X25519: cr.dc.X25519, Cert: cr.raw}
		copy(d.Ed25519[:], cr.dc.Ed25519)
		if old, ok, _ := ts.Device(sid, d.ID); ok && bytes.Equal(old.Cert, d.Cert) {
			return
		}
		if ts.PutDevice(sid, d) == nil {
			changed = true
		}
	}

	done := make([]bool, len(certs))
	for i, cr := range certs {
		if d, ok, _ := ts.Device(sid, cr.dc.Dev); ok && bytes.Equal(d.Cert, cr.raw) {
			done[i] = true // verified in an earlier pass
			continue
		}
		_, err := cr.s.VerifyDeviceCert(sid, cr.dc.SSH)
		done[i] = err != nil // forged: never counts
	}
	for round := 0; round < 8; round++ {
		progress := false
		for _, e := range endorse {
			ke, err := identity.PeekKeyEndorse(e)
			if err != nil || ke.ServerID != sid || pinned(ke.User, ke.NewSSH) {
				continue
			}
			for _, p := range pinsOf(ke.User) {
				v, err := e.VerifyKeyEndorse(sid, p)
				if err != nil || v.User != ke.User || !bytes.Equal(v.NewSSH, ke.NewSSH) {
					continue
				}
				if fp, ok := fingerprint(ke.NewSSH); ok && ts.PinKey(sid, ke.User, fp, ke.NewSSH, trust.Endorsed) == nil {
					pins[ke.User] = append(pins[ke.User], ke.NewSSH)
					progress, changed = true, true
				}
				break
			}
		}
		for i, cr := range certs {
			if done[i] {
				continue
			}
			u := cr.dc.User
			if pinned(u, cr.dc.SSH) {
				put(cr)
				done[i] = true
				continue
			}
			if len(pinsOf(u)) == 0 {
				fp, ok := fingerprint(cr.dc.SSH)
				if ok && ts.PinKey(sid, u, fp, cr.dc.SSH, trust.TOFU) == nil {
					pins[u] = append(pins[u], cr.dc.SSH)
					put(cr)
					done[i] = true
					progress, changed = true, true
				}
			}
		}
		if !progress {
			break
		}
	}
	// What's left verifies under a key that isn't pinned or endorsed: a key
	// change, pending until the user acknowledges it.
	for i, cr := range certs {
		if done[i] {
			continue
		}
		if fp, ok := fingerprint(cr.dc.SSH); ok {
			err := ts.PinKey(sid, cr.dc.User, fp, cr.dc.SSH, trust.TOFU)
			if err == nil { // acknowledged meanwhile
				put(cr)
			} else if errors.Is(err, trust.ErrKeyChanged) {
				changed = true
			}
		}
	}

	for _, r := range revokes {
		pr, err := identity.PeekDeviceRevoke(r.s)
		if err != nil || pr.ServerID != sid {
			continue
		}
		d, ok, _ := ts.Device(sid, pr.Dev)
		// Only a known device of the revoke's user: a member must not be able
		// to pre-revoke someone else's device id. Skip a revoke if we already
		// hold one at least as early in both seq and signed time.
		if !ok || d.User != pr.User || (d.Revoked && d.RevokedSeq != 0 && d.RevokedSeq <= r.seq && d.RevokedTime != 0 && d.RevokedTime <= pr.Time) {
			continue
		}
		valid := false
		var signedAt int64
		for _, p := range pinsOf(pr.User) {
			if v, err := r.s.VerifyDeviceRevoke(sid, p); err == nil && v.User == pr.User && v.Dev == pr.Dev {
				valid, signedAt = true, v.Time
				break
			}
		}
		if !valid {
			devs, _ := ts.Devices(sid, pr.User)
			for _, o := range devs {
				// A signer revoked at an unknown seq may have been revoked
				// before this revoke: it doesn't count.
				if o.ID == pr.Dev || (o.Revoked && (o.RevokedSeq == 0 || o.RevokedSeq < r.seq)) {
					continue
				}
				if v, err := r.s.VerifyDeviceRevokeByDevice(sid, o.Ed25519[:]); err == nil && v.User == pr.User && v.Dev == pr.Dev {
					valid, signedAt = true, v.Time
					break
				}
			}
		}
		if valid && ts.RevokeDeviceAtTime(sid, pr.Dev, r.seq, signedAt) == nil {
			changed = true
		}
	}
	return changed
}

// ---- epoch chain ----

// errSoft marks a bundle we can't judge yet (its rotator isn't verified).
type errSoft struct{ msg string }

func (e errSoft) Error() string { return e.msg }

// validateBundle checks cb as epoch `epoch` of ch on top of prev (nil for
// epoch 1), per §13.3 "Client validity". A plain error is evidence against
// the server (alarm); errSoft means we can't tell yet.
func (c *Client) validateBundle(sid e2ecrypto.ServerID, ch proto.Channel, epoch uint32, prev *trust.Epoch, cb cachedBundle) (identity.EpochBundle, error) {
	peek, err := identity.PeekEpochBundle(cb.b)
	switch {
	case err != nil:
		return peek, fmt.Errorf("epoch %d: malformed bundle", epoch)
	case peek.ServerID != sid:
		return peek, fmt.Errorf("epoch %d: bundle for another server", epoch)
	case peek.Chan != ch.ID || peek.Epoch != epoch:
		return peek, fmt.Errorf("epoch %d: bundle for another channel or epoch", epoch)
	}
	by, ok, err := c.trust.Device(sid, peek.By)
	if err != nil {
		return peek, errSoft{err.Error()}
	}
	if !ok {
		return peek, errSoft{fmt.Sprintf("epoch %d: can't verify the rotating device", epoch)}
	}
	eb, err := cb.b.VerifyEpochBundle(sid, by.Ed25519[:])
	if err != nil {
		return eb, fmt.Errorf("epoch %d: bundle signature doesn't verify", epoch)
	}
	// Invalid if EITHER the server's seq order OR the bundle's own signed
	// time puts it after a signed revoke of its device: a server can't
	// reorder past both (docs/E2E.md step 6). A revoke whose seq or time we
	// don't know (0) is judged by the other rule alone.
	if by.Revoked && by.RevokedSeq != 0 && by.RevokedSeq < cb.seq {
		return eb, fmt.Errorf("epoch %d: signed by a revoked device", epoch)
	}
	if by.Revoked && by.RevokedTime != 0 && eb.Time >= by.RevokedTime {
		return eb, fmt.Errorf("epoch %d: signed after its device was revoked", epoch)
	}
	if prev == nil {
		allowed := by.User == ch.Creator
		if ch.Kind == proto.ChanDM {
			allowed = slices.Contains(ch.Members, by.User)
		}
		if !allowed || !slices.Contains(eb.Roster, by.User) {
			return eb, fmt.Errorf("epoch 1: not created by the channel's creator")
		}
	} else {
		if eb.Prev != prev.Hash {
			return eb, fmt.Errorf("epoch %d: doesn't chain to epoch %d", epoch, prev.Epoch)
		}
		if !slices.Contains(prev.Roster, by.User) {
			return eb, fmt.Errorf("epoch %d: rotated by someone not in epoch %d", epoch, prev.Epoch)
		}
	}
	keys := map[[32]byte]bool{}
	for _, bx := range eb.Boxes {
		d, ok, err := c.trust.Device(sid, bx.Dev)
		if err != nil || !ok {
			continue // not ours to judge: the rotator may have verified it
		}
		if !slices.Contains(eb.Roster, d.User) {
			return eb, fmt.Errorf("epoch %d: key sealed to a non-member", epoch)
		}
		if d.Revoked && d.RevokedSeq != 0 && d.RevokedSeq < cb.seq {
			return eb, fmt.Errorf("epoch %d: key sealed to a revoked device", epoch)
		}
		if keys[d.X25519] {
			return eb, fmt.Errorf("epoch %d: key sealed twice to one X25519 key", epoch)
		}
		keys[d.X25519] = true
	}
	return eb, nil
}

// openBox returns K from our box in eb, or nil if there is none, it doesn't
// open, or it doesn't match kc.
func openBox(sid e2ecrypto.ServerID, eb identity.EpochBundle, me *e2ecrypto.DeviceKeys) *e2ecrypto.Key {
	if me == nil {
		return nil
	}
	for _, bx := range eb.Boxes {
		if bx.Dev != me.ID {
			continue
		}
		k, err := e2ecrypto.OpenKey(me.X25519, eb.Eph, e2ecrypto.BoxAD{
			ServerID: sid, Chan: eb.Chan, Epoch: eb.Epoch, Recipient: me.ID, By: eb.By,
		}, bx.CT[:])
		if err != nil || e2ecrypto.KeyCommitment(k, sid, eb.Chan, eb.Epoch) != eb.KC {
			return nil
		}
		return &k
	}
	return nil
}

func (c *Client) alarm(sid e2ecrypto.ServerID, ch uint64, detail string) bool {
	_ = c.trust.RaiseAlarm(sid, ch, detail)
	return true
}

// fetchBundles asks the server for epochs from..to of ch into the cache.
// It reports whether it got any.
func (c *Client) fetchBundles(ctx context.Context, s *session, ch uint64, from, to uint32) bool {
	if to-from >= proto.MaxKeyFetch {
		to = from + proto.MaxKeyFetch - 1
	}
	raw, err := s.call(ctx, proto.OpKeyFetch, proto.KeyFetchArgs{Chan: ch, From: from, To: to}, nil)
	if err != nil {
		return false
	}
	var res proto.KeyFetchRes
	if proto.Unmarshal(raw, &res) != nil || len(res.Seqs) != len(res.Bundles) {
		return false
	}
	err = c.tx(func(tx *sql.Tx) error {
		for i, b := range res.Bundles {
			if eb, err := identity.PeekEpochBundle(identity.Signed(b)); err != nil || eb.Epoch < from || eb.Epoch > to {
				continue
			}
			if err := storeBundle(tx, ch, b, res.Seqs[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return err == nil && len(res.Bundles) > 0
}

// syncChannel brings ch's verified chain up to the newest epoch the server
// shows, picks up keys (our boxes, backlinks, shares), asks for a missing
// one, and creates epoch 1 if it's ours to create. Caller holds e2eMu.
func (c *Client) syncChannel(ctx context.Context, s *session, sid e2ecrypto.ServerID, ch proto.Channel) (changed bool) {
	ts := c.trust
	if err := ts.MarkChannelE2E(sid, ch.ID); err != nil {
		return false
	}
	if _, ok, _ := ts.Alarm(sid, ch.ID); ok {
		return false
	}
	head, hasHead, err := ts.ChainHead(sid, ch.ID)
	if err != nil {
		return false
	}
	var latest uint32
	c.db.QueryRow(`SELECT COALESCE(MAX(epoch), 0) FROM epochs WHERE chan = ?`, ch.ID).Scan(&latest)
	latest = max(latest, ch.Epoch)
	if hasHead && latest < head.Epoch {
		return c.alarm(sid, ch.ID, fmt.Sprintf("rollback: the server shows epoch %d, this device verified %d", latest, head.Epoch))
	}

	// Bundles for epochs we already verified must be the ones we verified.
	if hasHead {
		rows, err := c.db.Query(`SELECT epoch, bundle, hash FROM epochs WHERE chan = ? AND epoch <= ?`, ch.ID, head.Epoch)
		if err == nil {
			type old struct {
				epoch     uint32
				raw, hash []byte
			}
			var olds []old
			for rows.Next() {
				var o old
				if rows.Scan(&o.epoch, &o.raw, &o.hash) == nil {
					olds = append(olds, o)
				}
			}
			rows.Close()
			for _, o := range olds {
				var b proto.Signed
				if proto.Unmarshal(o.raw, &b) != nil {
					continue
				}
				if ep, ok, _ := ts.Epoch(sid, ch.ID, o.epoch); ok && e2ecrypto.ChainHash(b.B) != ep.Hash {
					return c.alarm(sid, ch.ID, fmt.Sprintf("fork: a second bundle for epoch %d", o.epoch))
				}
				// Only rows we checked: one arriving meanwhile gets checked next pass.
				c.db.Exec(`DELETE FROM epochs WHERE chan = ? AND epoch = ? AND hash = ?`, ch.ID, o.epoch, o.hash)
			}
		}
	}

	soft := ""
	for next := head.Epoch + 1; next <= latest && ctx.Err() == nil; next++ {
		cands, err := c.cachedBundles(ch.ID, next)
		if err != nil {
			break
		}
		if len(cands) == 0 {
			c.fetchBundles(ctx, s, ch.ID, next, latest)
			if cands, err = c.cachedBundles(ch.ID, next); err != nil {
				break
			}
			if len(cands) == 0 {
				soft = fmt.Sprintf("the server doesn't provide epoch %d", next)
				break
			}
		}
		var prev *trust.Epoch
		if next > 1 {
			p, ok, err := ts.Epoch(sid, ch.ID, next-1)
			if err != nil || !ok {
				soft = fmt.Sprintf("epoch %d isn't verified", next-1)
				break
			}
			prev = &p
		}
		// An honest server has exactly one bundle per epoch.
		distinct := map[[32]byte]bool{}
		for _, cb := range cands {
			distinct[e2ecrypto.ChainHash(cb.b.B)] = true
		}
		if len(distinct) > 1 {
			return c.alarm(sid, ch.ID, fmt.Sprintf("fork: %d different bundles for epoch %d", len(distinct), next))
		}
		var valid []identity.EpochBundle
		var validB []cachedBundle
		var hard error
		for _, cb := range cands {
			eb, err := c.validateBundle(sid, ch, next, prev, cb)
			var se errSoft
			switch {
			case err == nil:
				dup := slices.ContainsFunc(validB, func(v cachedBundle) bool { return bytes.Equal(v.b.B, cb.b.B) })
				if !dup {
					valid, validB = append(valid, eb), append(validB, cb)
				}
			case errors.As(err, &se):
				if soft == "" {
					soft = se.msg
				}
			default:
				hard = err
			}
		}
		if hard != nil {
			return c.alarm(sid, ch.ID, hard.Error())
		}
		if len(valid) == 0 {
			break // soft is set: wait for the rotator's device to verify
		}
		soft = ""
		eb := valid[0]
		e := trust.Epoch{Epoch: next, Hash: e2ecrypto.ChainHash(validB[0].b.B), KC: eb.KC, Roster: eb.Roster,
			Key: openBox(sid, eb, s.myDevice())}
		if eb.Back != nil {
			e.Back = eb.Back[:]
		}
		if err := ts.AdvanceEpoch(sid, ch.ID, e); err != nil {
			if errors.Is(err, trust.ErrFork) || errors.Is(err, trust.ErrRollback) {
				return c.alarm(sid, ch.ID, err.Error())
			}
			break // another process advanced meanwhile; the next pass catches up
		}
		changed = true
		for _, cb := range cands {
			c.db.Exec(`DELETE FROM epochs WHERE chan = ? AND epoch = ? AND hash = ?`, ch.ID, next, cb.hash)
		}
	}
	if c.setSoft(ch.ID, soft) {
		changed = true
	}
	if c.applyShares(sid, ch.ID, s.myDevice()) {
		changed = true
	}
	if c.walkBack(sid, ch.ID) {
		changed = true
	}
	c.sendKeyNeed(s, sid, ch.ID)
	c.autoEpoch1(ctx, s, sid, ch)
	return changed
}

// walkBack recovers older epoch keys through backlinks: K_{e-1} is sealed
// under K_e in epoch e's bundle (and checked against epoch e-1's kc).
func (c *Client) walkBack(sid e2ecrypto.ServerID, ch uint64) bool {
	missing, err := c.trust.MissingKeys(sid, ch)
	if err != nil {
		return false
	}
	changed := false
	for _, e := range missing { // newest first, so each success feeds the next
		next, ok, err := c.trust.Epoch(sid, ch, e+1)
		if err != nil || !ok || next.Key == nil || len(next.Back) == 0 {
			continue
		}
		k, err := e2ecrypto.OpenBack(*next.Key, sid, ch, e+1, next.Back)
		if err == nil && c.trust.SetEpochKey(sid, ch, e, k) == nil {
			changed = true
		}
	}
	return changed
}

// applyShares opens key.share boxes sent to our device for epochs we've
// verified. A share is accepted only if K matches the epoch's kc.
func (c *Client) applyShares(sid e2ecrypto.ServerID, ch uint64, me *e2ecrypto.DeviceKeys) bool {
	rows, err := c.db.Query(`SELECT epoch, dev, eph, ct FROM key_shares WHERE chan = ?`, ch)
	if err != nil {
		return false
	}
	type share struct {
		epoch        uint32
		dev, eph, ct []byte
	}
	var shares []share
	for rows.Next() {
		var sh share
		if rows.Scan(&sh.epoch, &sh.dev, &sh.eph, &sh.ct) == nil {
			shares = append(shares, sh)
		}
	}
	rows.Close()
	changed := false
	for _, sh := range shares {
		drop := func() {
			c.db.Exec(`DELETE FROM key_shares WHERE chan = ? AND epoch = ? AND dev = ? AND eph = ?`, ch, sh.epoch, sh.dev, sh.eph)
		}
		if me == nil {
			return changed
		}
		if !bytes.Equal(sh.dev, me.ID[:]) {
			drop()
			continue
		}
		ep, ok, err := c.trust.Epoch(sid, ch, sh.epoch)
		if err != nil || !ok {
			continue // not verified yet: keep it
		}
		if ep.Key == nil {
			k, err := e2ecrypto.OpenShare(me.X25519, [32]byte(sh.eph), e2ecrypto.ShareAD{
				ServerID: sid, Chan: ch, Epoch: sh.epoch, Recipient: me.ID,
			}, sh.ct)
			if err == nil && c.trust.SetEpochKey(sid, ch, sh.epoch, k) == nil {
				changed = true
			}
		}
		drop()
	}
	return changed
}

// sendKeyNeed asks the channel's members for the newest epoch key we lack,
// at most once per channel per timing.keyNeed.
func (c *Client) sendKeyNeed(s *session, sid e2ecrypto.ServerID, ch uint64) {
	me := s.myDevice()
	if me == nil {
		return
	}
	missing, err := c.trust.MissingKeys(sid, ch)
	if err != nil || len(missing) == 0 {
		return
	}
	c.mu.Lock()
	if t, ok := c.needSent[ch]; ok && time.Since(t) < c.timing.keyNeed {
		c.mu.Unlock()
		return
	}
	c.needSent[ch] = time.Now()
	c.mu.Unlock()
	_ = s.send(proto.TEphemeral, proto.Ephemeral{Kind: proto.EphKeyNeed, Chan: ch, Epoch: missing[0], Dev: me.ID[:]})
}

type shareKey struct {
	ch    uint64
	epoch uint32
	dev   e2ecrypto.DevID
}

// answerKeyNeed seals K to the asking device if it's a verified, live device
// of the asking user and that user is in the epoch's roster.
func (c *Client) answerKeyNeed(ctx context.Context, s *session, sid e2ecrypto.ServerID, e proto.Ephemeral) {
	me := s.myDevice()
	if me == nil || len(e.Dev) != e2ecrypto.IDSize || e.Chan == 0 || e.Epoch == 0 {
		return
	}
	dev := e2ecrypto.DevID(e.Dev)
	if dev == me.ID {
		return
	}
	c.e2eMu.Lock()
	d, ok, err := c.trust.Device(sid, dev)
	if err == nil && !ok {
		// Its device.new came just before; the worker may not have run yet.
		c.verifyDevices(sid, s.sshPub, c.Me().ID)
		d, ok, err = c.trust.Device(sid, dev)
	}
	if err != nil || !ok || d.Revoked || d.User != e.User {
		c.e2eMu.Unlock()
		return
	}
	ep, ok, err := c.trust.Epoch(sid, e.Chan, e.Epoch)
	c.e2eMu.Unlock()
	if err != nil || !ok || ep.Key == nil || !slices.Contains(ep.Roster, d.User) {
		return
	}
	key := shareKey{e.Chan, e.Epoch, dev}
	c.mu.Lock()
	if t, ok := c.shared[key]; ok && time.Since(t) < c.timing.keyNeed {
		c.mu.Unlock()
		return
	}
	c.shared[key] = time.Now()
	c.mu.Unlock()
	eph, ct, err := e2ecrypto.SealShare(rand.Reader, d.X25519, e2ecrypto.ShareAD{
		ServerID: sid, Chan: e.Chan, Epoch: e.Epoch, Recipient: dev,
	}, *ep.Key)
	if err != nil {
		return
	}
	_, _ = s.call(ctx, proto.OpKeyShare, proto.KeyShareArgs{Chan: e.Chan, Epoch: e.Epoch, Dev: dev[:], Eph: eph[:], CT: ct[:]}, nil)
}

// autoEpoch1 creates epoch 1 of an encrypted channel we created (§13.3:
// "clients rotate epoch 1 right after chan.create"), at most once per
// channel per timing.autoEpoch. Caller holds e2eMu.
func (c *Client) autoEpoch1(ctx context.Context, s *session, sid e2ecrypto.ServerID, ch proto.Channel) {
	if ch.Epoch != 0 || ch.Creator != c.Me().ID {
		return
	}
	if _, ok, _ := c.trust.ChainHead(sid, ch.ID); ok {
		return
	}
	select {
	case <-s.devReady:
	default:
		return
	}
	c.mu.Lock()
	if t, ok := c.autoTried[ch.ID]; ok && time.Since(t) < c.timing.autoEpoch {
		c.mu.Unlock()
		return
	}
	c.autoTried[ch.ID] = time.Now()
	c.mu.Unlock()
	_ = c.rotateLocked(ctx, s, sid, ch.ID)
}

// ---- rotation ----

// RotateKey starts a new epoch of encrypted channel ch: a fresh K sealed to
// every verified, non-revoked device of the current members (at least one
// per member who has a device — else *UnverifiedMembersError), linked back
// to the current K, chained to the verified head and signed by this device.
// A stale_epoch (someone else rotated, or the members changed) is retried
// once on the refreshed state. Online only.
func (c *Client) RotateKey(ctx context.Context, ch uint64) error {
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return ErrOffline
	}
	sid, ok := s.serverID()
	if !ok || !s.e2e() {
		return errors.New("server doesn't support end-to-end encryption")
	}
	select {
	case <-s.devReady:
	case <-s.done:
		return ErrOffline
	case <-ctx.Done():
		return ctx.Err()
	}
	c.e2eMu.Lock()
	defer c.e2eMu.Unlock()
	return c.rotateLocked(ctx, s, sid, ch)
}

func (c *Client) rotateLocked(ctx context.Context, s *session, sid e2ecrypto.ServerID, chID uint64) error {
	for attempt := 0; ; attempt++ {
		err := c.rotateOnce(ctx, s, sid, chID)
		var pe *proto.Error
		if attempt == 0 && errors.As(err, &pe) && pe.Code == proto.ErrStaleEpoch {
			// The winning key.epoch (or the membership change) was applied
			// before our RES: rebuild on it.
			continue
		}
		return err
	}
}

func (c *Client) rotateOnce(ctx context.Context, s *session, sid e2ecrypto.ServerID, chID uint64) error {
	me := s.myDevice()
	if me == nil {
		return errors.New("this device isn't registered yet")
	}
	ch, ok, err := getChan(c.db, chID)
	if err != nil {
		return err
	}
	if !ok || !ch.E2E {
		return errors.New("not an encrypted channel")
	}
	user := c.Me().ID
	if !slices.Contains(ch.Members, user) {
		return errors.New("not a member")
	}
	c.syncChannel(ctx, s, sid, ch)
	if ch, _, err = getChan(c.db, chID); err != nil {
		return err
	}
	if st := c.e2eStatus(sid, ch); st.State == E2EAlarm || st.State == E2EUnverified {
		return fmt.Errorf("can't rotate: %s", st.Detail)
	}
	head, hasHead, err := c.trust.ChainHead(sid, chID)
	if err != nil {
		return err
	}
	p := identity.EpochParams{ServerID: sid, Chan: chID, Epoch: 1, Time: time.Now().UnixMilli()}
	if hasHead {
		cur, ok, err := c.trust.Epoch(sid, chID, head.Epoch)
		if err != nil || !ok {
			return fmt.Errorf("epoch %d isn't verified", head.Epoch)
		}
		if cur.Key == nil {
			return fmt.Errorf("waiting for the key of epoch %d", head.Epoch)
		}
		p.Epoch, p.Prev, p.PrevK = head.Epoch+1, head.Hash, cur.Key
	} else if ch.Epoch > 0 {
		return errors.New("the channel's key chain isn't verified yet")
	} else if ch.Kind != proto.ChanDM && ch.Creator != user {
		return errors.New("only the channel's creator can create its first key")
	}

	c.mu.Lock()
	for _, u := range ch.Members {
		if !c.users[u].Disabled {
			p.Roster = append(p.Roster, u)
		}
	}
	c.mu.Unlock()
	slices.Sort(p.Roster)
	p.Roster = slices.Compact(p.Roster)
	var unverified UnverifiedMembersError
	for _, u := range p.Roster {
		devs, err := c.trust.Devices(sid, u)
		if err != nil {
			return err
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
			unverified.Names = append(unverified.Names, "@"+c.User(u).Name)
		}
	}
	if len(unverified.Users) > 0 {
		return &unverified
	}
	k, err := e2ecrypto.NewEpochKey(rand.Reader)
	if err != nil {
		return err
	}
	p.K = k
	b, err := identity.NewEpochBundle(rand.Reader, me, p)
	if err != nil {
		return fmt.Errorf("building epoch bundle: %w", err)
	}
	if _, err := s.call(ctx, proto.OpKeyRotate, proto.KeyRotateArgs{Chan: chID, Bundle: proto.Signed(b)}, nil); err != nil {
		return err
	}
	// Our key.epoch event came before the RES: verify it like anyone's.
	if ch, _, err = getChan(c.db, chID); err != nil {
		return err
	}
	c.syncChannel(ctx, s, sid, ch)
	got, ok, err := c.trust.Epoch(sid, chID, p.Epoch)
	if err != nil || !ok || got.Hash != e2ecrypto.ChainHash(b.B) || got.Key == nil {
		return fmt.Errorf("epoch %d was accepted but doesn't verify here", p.Epoch)
	}
	c.post(Update{Channels: true})
	return nil
}

// hasUnverifiedDevice reports whether the cache holds a cert for user that
// isn't a verified or revoked device in the trust store.
func (c *Client) hasUnverifiedDevice(sid e2ecrypto.ServerID, user uint64) bool {
	rows, err := c.db.Query(`SELECT dev FROM devices WHERE user = ?`, user)
	if err != nil {
		return false
	}
	var devs [][]byte
	for rows.Next() {
		var d []byte
		if rows.Scan(&d) == nil && len(d) == e2ecrypto.IDSize {
			devs = append(devs, d)
		}
	}
	rows.Close()
	for _, d := range devs {
		if _, ok, err := c.trust.Device(sid, e2ecrypto.DevID(d)); err == nil && !ok {
			return true
		}
	}
	return false
}

// ---- status ----

// UnverifiedUsers lists the other members of my encrypted channels whose
// devices this device trusts only on first use: none of their pinned SSH
// keys was verified out of band (an endorsed key extends a TOFU anchor, so
// it counts as TOFU too). Their keys are accepted (Signal-like) but a
// server present at first contact could have substituted them; the TUI
// marks them until /verify (step 10). Sorted; works offline.
func (c *Client) UnverifiedUsers() []uint64 {
	sid, ok := c.cacheServerID()
	if !ok {
		return nil
	}
	me := c.Me().ID
	chans, err := c.memberChannels(me)
	if err != nil {
		return nil
	}
	seen := map[uint64]bool{}
	var out []uint64
	for _, ch := range chans {
		if was, _ := c.trust.ChannelE2E(sid, ch.ID); !ch.E2E && !was {
			continue
		}
		for _, u := range ch.Members {
			if u == me || seen[u] {
				continue
			}
			seen[u] = true
			pins, err := c.trust.PinnedKeys(sid, u)
			if err != nil || len(pins) == 0 {
				continue // no device seen yet: nothing trusted at all
			}
			if !slices.ContainsFunc(pins, func(k trust.SSHKey) bool { return k.How == trust.Verified }) {
				out = append(out, u)
			}
		}
	}
	slices.Sort(out)
	return out
}

// TrustAlarm reports a server-wide trust alarm: the server, once seen
// speaking e2e1, stopped advertising it. Every encrypted channel's
// E2EStatus is then an alarm too. Sticky (trust store); works offline.
func (c *Client) TrustAlarm() (detail string, ok bool) {
	sid, ok := c.cacheServerID()
	if !ok {
		return "", false
	}
	detail, ok, _ = c.trust.ServerAlarm(sid)
	return detail, ok
}

// E2EStatus reports ch's key state from the trust store and the cache; it
// works offline.
func (c *Client) E2EStatus(ch uint64) E2EStatus {
	sid, ok := c.cacheServerID()
	if !ok {
		return E2EStatus{}
	}
	cv, ok, err := getChan(c.db, ch)
	if err != nil || !ok {
		return E2EStatus{}
	}
	return c.e2eStatus(sid, cv)
}

func (c *Client) e2eStatus(sid e2ecrypto.ServerID, ch proto.Channel) E2EStatus {
	was, _ := c.trust.ChannelE2E(sid, ch.ID)
	if !ch.E2E && !was {
		return E2EStatus{}
	}
	head, hasHead, _ := c.trust.ChainHead(sid, ch.ID)
	st := E2EStatus{Epoch: head.Epoch}
	if detail, ok, _ := c.trust.ServerAlarm(sid); ok {
		st.State, st.Detail = E2EAlarm, detail
		return st
	}
	if detail, ok, _ := c.trust.Alarm(sid, ch.ID); ok {
		st.State, st.Detail = E2EAlarm, detail
		return st
	}
	if !ch.E2E {
		st.State, st.Detail = E2EAlarm, "the server says this channel is no longer encrypted"
		return st
	}
	c.mu.Lock()
	soft := c.e2eSoft[ch.ID]
	c.mu.Unlock()
	if soft != "" {
		st.State, st.Detail = E2EUnverified, soft
		return st
	}
	if !hasHead {
		st.State, st.Detail = E2EWaitingKey, "the channel has no key yet"
		if ch.Epoch > 0 {
			st.Detail = "verifying the channel's keys"
		}
		return st
	}
	if ep, ok, _ := c.trust.Epoch(sid, ch.ID, head.Epoch); !ok || ep.Key == nil {
		st.State, st.Detail = E2EWaitingKey, fmt.Sprintf("waiting for the key of epoch %d", head.Epoch)
		return st
	}
	st.State = E2EOK
	return st
}
