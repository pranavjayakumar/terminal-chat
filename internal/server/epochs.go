package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// Epochs (PROTOCOL.md §13.3). As with devices, the server only enforces the
// honest case; clients verify the chain, roster and boxes themselves.

const epochsSchema = `
CREATE TABLE IF NOT EXISTS epochs (
	chan INTEGER NOT NULL,
	epoch INTEGER NOT NULL,
	bundle BLOB NOT NULL, -- msgpack Signed, forwarded byte for byte
	hash BLOB NOT NULL,   -- ChainHash(b): the next bundle's prev
	by_user INTEGER NOT NULL,
	time INTEGER NOT NULL, -- server clock, for the rotate rate cap
	seq INTEGER NOT NULL DEFAULT 0, -- of its key.epoch event
	state BLOB, -- epochState at rotation time, for the rate cap (NULL before it existed)
	PRIMARY KEY (chan, epoch)
);
-- The rate cap looks up a user's last rotation under the write lock.
CREATE INDEX IF NOT EXISTS epochs_by ON epochs(chan, by_user, time);
-- Rosters (key.rotate, EpochStale) list a channel's current members.
CREATE INDEX IF NOT EXISTS membership_chan ON membership(chan, user) WHERE leave_seq IS NULL;
`

// The rotate rate caps, both waived when the roster or its active devices
// changed since the current epoch: one key.rotate per channel per user per
// 10 s, and one per channel per 2 s whoever rotates.
const (
	rotateEveryMs     = 10_000
	rotateChanEveryMs = 2_000
)

// migrateEpochs adds the epochs table, channels.e2e/epoch, epochs.seq,
// devices.revoked_seq (the seq of the device.revoke event) and
// devices.created_seq (of the device.new event).
func migrateEpochs(db *sql.DB) error {
	if _, err := db.Exec(epochsSchema); err != nil {
		return fmt.Errorf("creating epochs schema: %w", err)
	}
	for _, c := range [][3]string{
		{"channels", "e2e", `INTEGER NOT NULL DEFAULT 0`},
		{"channels", "epoch", `INTEGER NOT NULL DEFAULT 0`},
		{"epochs", "seq", `INTEGER NOT NULL DEFAULT 0`},
		{"epochs", "state", `BLOB`},
		{"devices", "revoked_seq", `INTEGER`},
		// seq of the device.new; NULL: registered before this column existed
		{"devices", "created_seq", `INTEGER`},
		{"messages", "enc", `BLOB`}, // §13.4; never in msg_fts (body is "" then)
		{"messages", "mid", `BLOB`}, // e2ecrypto.MID(enc): one message per enc and channel
		{"messages", "edit_enc", `BLOB`},
		{"messages", "del_enc", `BLOB`},
		{"reactions", "enc", `BLOB`},
		{"channels", "legacy", `INTEGER NOT NULL DEFAULT 0`}, // chan.encrypt: newest plaintext message id
	} {
		if err := addColumn(db, c[0], c[1], c[2]); err != nil {
			return err
		}
	}
	if err := backfillMids(db); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS messages_mid ON messages(chan, mid) WHERE mid IS NOT NULL`); err != nil {
		return fmt.Errorf("creating messages_mid: %w", err)
	}
	if err := migrateCreator(db); err != nil {
		return err
	}
	return backfillSeqs(db)
}

// backfillMids fills messages.mid for encrypted messages stored before it
// existed. A replayed enc (a second row with the same mid in a channel)
// keeps NULL, so the unique index can still be built.
func backfillMids(db *sql.DB) error {
	rows, err := db.Query(`SELECT id, chan, enc FROM messages WHERE enc IS NOT NULL AND mid IS NULL ORDER BY id`)
	if err != nil {
		return err
	}
	type row struct {
		id, ch uint64
		mid    e2ecrypto.MsgID
	}
	var todo []row
	for rows.Next() {
		var r row
		var enc []byte
		if err := rows.Scan(&r.id, &r.ch, &enc); err != nil {
			rows.Close()
			return err
		}
		r.mid = e2ecrypto.MID(enc)
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(todo) == 0 {
		return err
	}
	seen := map[[2]any]bool{}
	for _, r := range todo {
		k := [2]any{r.ch, r.mid}
		if seen[k] {
			continue
		}
		seen[k] = true
		if _, err := db.Exec(`UPDATE messages SET mid = ? WHERE id = ? AND NOT EXISTS
			(SELECT 1 FROM messages WHERE chan = ? AND mid = ?)`, r.mid[:], r.id, r.ch, r.mid[:]); err != nil {
			return fmt.Errorf("backfilling messages.mid: %w", err)
		}
	}
	return nil
}

// migrateCreator adds channels.creator, backfilled from each channel's
// chan.new actor (0 for #general, which the server creates).
func migrateCreator(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('channels') WHERE name = 'creator'`).Scan(&n); err != nil || n > 0 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`ALTER TABLE channels ADD COLUMN creator INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("adding channels.creator: %w", err)
	}
	if _, err := tx.Exec(`UPDATE channels SET creator = e.actor FROM events e
		WHERE e.kind = ? AND e.chan = channels.id`, proto.KChanNew); err != nil {
		return fmt.Errorf("backfilling channels.creator: %w", err)
	}
	return tx.Commit()
}

// backfillSeqs fills epochs.seq and devices.revoked_seq for rows written
// before those columns existed, from their events.
func backfillSeqs(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM epochs WHERE seq = 0)
		+ (SELECT COUNT(*) FROM devices WHERE revoked IS NOT NULL AND revoked_seq IS NULL)`).Scan(&n); err != nil || n == 0 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT seq, kind, chan, data FROM events WHERE kind IN (?, ?) ORDER BY seq`,
		proto.KKeyEpoch, proto.KDeviceRevoke)
	if err != nil {
		return err
	}
	type upd struct {
		q    string
		args []any
	}
	var ups []upd
	for rows.Next() {
		var seq uint64
		var kind string
		var ch sql.NullInt64
		var data []byte
		if err := rows.Scan(&seq, &kind, &ch, &data); err != nil {
			rows.Close()
			return err
		}
		switch kind {
		case proto.KKeyEpoch:
			var d proto.KeyEpoch
			if proto.Unmarshal(data, &d) != nil {
				continue
			}
			if eb, err := identity.PeekEpochBundle(identity.Signed(d.Bundle)); err == nil {
				ups = append(ups, upd{`UPDATE epochs SET seq = ? WHERE chan = ? AND epoch = ? AND seq = 0`, []any{seq, ch.Int64, eb.Epoch}})
			}
		case proto.KDeviceRevoke:
			var d proto.DeviceRevokeArgs
			if proto.Unmarshal(data, &d) != nil {
				continue
			}
			if r, err := identity.PeekDeviceRevoke(identity.Signed(d.Revoke)); err == nil {
				ups = append(ups, upd{`UPDATE devices SET revoked_seq = ? WHERE dev = ? AND revoked IS NOT NULL AND revoked_seq IS NULL`, []any{seq, r.Dev[:]}})
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range ups {
		if _, err := tx.Exec(u.q, u.args...); err != nil {
			return fmt.Errorf("backfilling seqs: %w", err)
		}
	}
	return tx.Commit()
}

// isE2EKind: events only e2e1 sessions receive, live or replayed.
func isE2EKind(kind string) bool {
	return isDeviceKind(kind) || kind == proto.KKeyEpoch || kind == proto.KKeyShare
}

// epochHead is an encrypted channel's chain head: its current bundle (nil
// at epoch 0), that bundle's chain hash, the seq of its key.epoch event,
// when it was stored and the epochState then (nil for old rows).
type epochHead struct {
	e2e    bool
	epoch  uint32
	bundle *proto.Signed
	hash   []byte
	seq    uint64
	time   int64
	state  []byte
}

// chanEpoch returns ch's chain head.
func chanEpoch(ctx context.Context, q querier, ch uint64) (h epochHead, err error) {
	if err = q.QueryRowContext(ctx, `SELECT e2e, epoch FROM channels WHERE id = ?`, ch).Scan(&h.e2e, &h.epoch); err != nil {
		return
	}
	if h.epoch == 0 {
		return
	}
	var raw []byte
	if err = q.QueryRowContext(ctx, `SELECT bundle, hash, seq, time, state FROM epochs WHERE chan = ? AND epoch = ?`, ch, h.epoch).
		Scan(&raw, &h.hash, &h.seq, &h.time, &h.state); err != nil {
		return
	}
	h.bundle = new(proto.Signed)
	err = proto.Unmarshal(raw, h.bundle)
	return
}

// rosterQuery is ch's current non-disabled members (via membership_chan).
const rosterQuery = `SELECT m.user FROM membership m JOIN users u ON u.id = m.user
		WHERE m.chan = ? AND m.leave_seq IS NULL AND u.disabled = 0 ORDER BY m.user`

// rosterOf is ch's current non-disabled members, sorted: what a bundle's
// roster must be.
func rosterOf(ctx context.Context, q querier, ch uint64) ([]uint64, error) {
	rows, err := q.QueryContext(ctx, rosterQuery, ch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uint64{}
	for rows.Next() {
		var u uint64
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// rosterDevices maps the non-revoked devices of users to their X25519 keys
// (the devices a bundle may box to) and to their owners.
func rosterDevices(ctx context.Context, q querier, users []uint64) (map[e2ecrypto.DevID][32]byte, map[e2ecrypto.DevID]uint64, error) {
	js, err := json.Marshal(users)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT dev, x25519, user FROM devices
		WHERE revoked IS NULL AND user IN (SELECT value FROM json_each(?))`, string(js))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := map[e2ecrypto.DevID][32]byte{}
	owners := map[e2ecrypto.DevID]uint64{}
	for rows.Next() {
		var dev, x []byte
		var owner uint64
		if err := rows.Scan(&dev, &x, &owner); err != nil {
			return nil, nil, err
		}
		var id e2ecrypto.DevID
		var k [32]byte
		if len(dev) != len(id) || len(x) != len(k) {
			return nil, nil, fmt.Errorf("malformed stored device %x", dev)
		}
		copy(id[:], dev)
		copy(k[:], x)
		out[id] = k
		owners[id] = owner
	}
	return out, owners, rows.Err()
}

// unboxedUsers lists the users owning a device in owners who have no box
// in eb.
func unboxedUsers(eb identity.EpochBundle, owners map[e2ecrypto.DevID]uint64) []uint64 {
	boxed := map[uint64]bool{}
	for _, bx := range eb.Boxes {
		boxed[owners[bx.Dev]] = true
	}
	var out []uint64
	for _, u := range owners {
		if !boxed[u] && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}

// checkEveryUserBoxed refuses a bundle that leaves a member with an active
// device without any box. When each such member's devices were all
// registered after headSeq (the current epoch's key.epoch; 0 at epoch 0),
// the rotator may not have seen their device.new yet: a race →
// stale_epoch, and the retry boxes them. Otherwise → bad_request.
func checkEveryUserBoxed(ctx context.Context, q querier, eb identity.EpochBundle,
	owners map[e2ecrypto.DevID]uint64, headSeq uint64) error {
	missing := unboxedUsers(eb, owners)
	if len(missing) == 0 {
		return nil
	}
	js, err := json.Marshal(missing)
	if err != nil {
		return err
	}
	// Rows from before created_seq existed (NULL) predate any race.
	var seen int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE revoked IS NULL
		AND user IN (SELECT value FROM json_each(?)) AND COALESCE(created_seq, 0) <= ?`,
		string(js), headSeq).Scan(&seen); err != nil {
		return err
	}
	if seen == 0 {
		return staleEpoch("a member registered a device since the current epoch")
	}
	return badRequest("every member with a device needs at least one box")
}

// epochState hashes what a rotation answers to: the sorted roster and the
// sorted active devices of its users. Stored with each epoch for the
// rotate rate cap.
func epochState(roster []uint64, devs map[e2ecrypto.DevID][32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte("tc-epoch-state-v1\x00"))
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(roster)))) // count, then 8-byte ids, then 16-byte ids: unambiguous
	for _, u := range roster {
		h.Write(binary.BigEndian.AppendUint64(nil, u))
	}
	ids := slices.SortedFunc(maps.Keys(devs), func(a, b e2ecrypto.DevID) int { return bytes.Compare(a[:], b[:]) })
	for _, id := range ids {
		h.Write(id[:])
	}
	return [32]byte(h.Sum(nil))
}

// checkBoxes checks that every box goes to an active device (in devs) of a
// roster user. Boxes may be missing: clients seal only to devices they've
// verified, and the rest get the key later via key.share (docs/E2E.md).
// A box to a device revoked after headSeq (the current epoch's key.epoch;
// 0 at epoch 0) is a race the client couldn't have seen → stale_epoch.
// Anything else — unknown device, another user's, one revoked before the
// client's view of the chain — is a client error → bad_request.
func checkBoxes(ctx context.Context, q querier, eb identity.EpochBundle, roster []uint64,
	devs map[e2ecrypto.DevID][32]byte, headSeq uint64) error {
	for _, bx := range eb.Boxes {
		if _, ok := devs[bx.Dev]; ok {
			continue
		}
		var owner uint64
		var revSeq sql.NullInt64
		err := q.QueryRowContext(ctx, `SELECT user, revoked_seq FROM devices WHERE dev = ?`, bx.Dev[:]).Scan(&owner, &revSeq)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return badRequest("bundle boxes an unknown device")
		case err != nil:
			return err
		case !slices.Contains(roster, owner):
			return badRequest("bundle boxes a device of a non-member")
		case revSeq.Valid && uint64(revSeq.Int64) > headSeq:
			return staleEpoch("bundle boxes a device revoked since the current epoch")
		default:
			return badRequest("bundle boxes a revoked device")
		}
	}
	return nil
}

func staleEpoch(msg string) error { return proto.Errf(proto.ErrStaleEpoch, msg) }

func (s *Server) opKeyRotate(ctx context.Context, u proto.User, a proto.KeyRotateArgs) (any, error) {
	if signedSize(a.Bundle) > proto.MaxBundle {
		return nil, proto.Errf(proto.ErrTooLarge, "epoch bundles are limited to 16 KiB")
	}
	var res proto.SeqRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		// Cheap checks first: non-members never make the server decode 16 KiB.
		if _, ok := s.kinds[a.Chan]; !ok {
			return nil, errNotFound
		}
		if !s.members[a.Chan][u.ID] {
			return nil, errForbidden
		}
		ev, err := s.rotateTx(ctx, tx, u, a.Chan, a.Bundle)
		if err != nil {
			return nil, err
		}
		res.Seq = ev.Seq
		return []proto.Event{ev}, nil
	})
	return res, err
}

// rotateTx checks bundle as ch's next epoch by u against the state inside
// tx (§13.3, from "channel is encrypted" on), stores it and returns its
// key.epoch event, which the caller must emit. chan.add, chan.remove and
// chan.encrypt call it after changing the membership or the e2e flag in
// the same transaction, so the roster check sees the new members. The
// caller has checked that the channel exists, u is a member (as of before
// the change) and the bundle's size.
func (s *Server) rotateTx(ctx context.Context, tx *sql.Tx, u proto.User, ch uint64, bundle proto.Signed) (proto.Event, error) {
	var none proto.Event
	sid := s.sid()
	head, err := chanEpoch(ctx, tx, ch)
	if err != nil {
		return none, err
	}
	if !head.e2e {
		return none, badRequest("channel is not encrypted")
	}
	peek, err := identity.PeekEpochBundle(identity.Signed(bundle))
	switch {
	case errors.Is(err, e2ecrypto.ErrTooLarge):
		return none, proto.Errf(proto.ErrTooLarge, "epoch bundles are limited to 128 boxes")
	case err != nil:
		return none, badRequest("malformed epoch bundle")
	case peek.ServerID != sid:
		return none, badRequest("epoch bundle is for another server")
	case peek.Chan != ch:
		return none, badRequest("epoch bundle is for another channel")
	}
	// The rotator: one of the caller's active devices, whose key signed it.
	var owner uint64
	var edPub, revoked []byte
	err = tx.QueryRowContext(ctx, `SELECT user, ed25519, revoked FROM devices WHERE dev = ?`, peek.By[:]).
		Scan(&owner, &edPub, &revoked)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (owner != u.ID || revoked != nil)) {
		return none, proto.Errf(proto.ErrForbidden, "bundle is not signed by one of your active devices")
	}
	if err != nil {
		return none, err
	}
	eb, err := identity.Signed(bundle).VerifyEpochBundle(sid, ed25519.PublicKey(edPub))
	if err != nil {
		return none, badRequest("epoch bundle signature does not verify")
	}

	// Compare-and-set on the chain head.
	var prev [32]byte
	copy(prev[:], head.hash)
	if eb.Epoch != head.epoch+1 || eb.Prev != prev {
		return none, staleEpoch(fmt.Sprintf("channel is at epoch %d", head.epoch))
	}
	// Who may extend the chain: a previous-roster user (the caller, since
	// by is the caller's device); for epoch 1 the creator, or any member
	// of a DM. Clients check the same (§13.3).
	if head.bundle != nil {
		old, err := identity.PeekEpochBundle(identity.Signed(*head.bundle))
		if err != nil {
			return none, err
		}
		if !slices.Contains(old.Roster, u.ID) {
			return none, proto.Errf(proto.ErrForbidden, "only members of the previous epoch may rotate")
		}
	} else if s.kinds[ch] != proto.ChanDM {
		var creator uint64
		if err := tx.QueryRowContext(ctx, `SELECT creator FROM channels WHERE id = ?`, ch).Scan(&creator); err != nil {
			return none, err
		}
		if creator != u.ID {
			return none, proto.Errf(proto.ErrForbidden, "only the channel's creator may create epoch 1")
		}
	}
	// dispatch checked "disabled" before the write lock: a user disabled
	// meanwhile must not commit a rotation (they'd hold the new K).
	if s.disabled[u.ID] {
		return none, proto.Errf(proto.ErrUnauthorized, "account disabled")
	}
	roster, err := rosterOf(ctx, tx, ch)
	if err != nil {
		return none, err
	}
	if !slices.Equal(eb.Roster, roster) {
		return none, staleEpoch("roster is not the channel's current members")
	}
	if !slices.Contains(roster, u.ID) {
		return none, proto.Errf(proto.ErrForbidden, "only members of the new epoch may rotate")
	}
	devs, owners, err := rosterDevices(ctx, tx, roster)
	if err != nil {
		return none, err
	}
	if err := checkBoxes(ctx, tx, eb, roster, devs, head.seq); err != nil {
		return none, err
	}
	if err := eb.CheckBoxKeys(devs); err != nil {
		return none, badRequest("bundle seals twice to one key")
	}
	// Clients may skip devices they haven't verified, but a member left
	// without any box could only ever get K from someone's key.share.
	if err := checkEveryUserBoxed(ctx, tx, eb, owners, head.seq); err != nil {
		return none, err
	}

	now := s.now()
	state := epochState(roster, devs)
	// The caps apply unless the members or their active devices changed
	// since the current epoch was made. Compared against what the server
	// recorded then, not what the bundle boxed: otherwise a member with
	// one never-boxed device could rotate without limit.
	capped := head.bundle != nil && bytes.Equal(head.state, state[:])
	if capped {
		// A device registered after the epoch and revoked since leaves the
		// state unchanged, but it may hold K by key.share: rotating away
		// from it must not wait.
		rev, err := revokedSince(ctx, tx, ch, head.seq)
		if err != nil {
			return none, err
		}
		capped = !rev
	}
	if capped {
		if now-head.time < rotateChanEveryMs {
			return none, proto.Errf(proto.ErrRateLimited, "nothing changed since the channel's last rotation")
		}
		var last sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(time) FROM epochs WHERE chan = ? AND by_user = ?`,
			ch, u.ID).Scan(&last); err != nil {
			return none, err
		}
		if last.Valid && now-last.Int64 < rotateEveryMs {
			return none, proto.Errf(proto.ErrRateLimited, "nothing changed since your last rotation")
		}
	}

	ev, err := appendEvent(ctx, tx, proto.KKeyEpoch, ch, u.ID, 0, now, proto.KeyEpoch{Bundle: bundle})
	if err != nil {
		return none, err
	}
	hash := e2ecrypto.ChainHash(bundle.B)
	if _, err := tx.ExecContext(ctx, `INSERT INTO epochs (chan, epoch, bundle, hash, by_user, time, seq, state) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ch, eb.Epoch, proto.MustRaw(bundle), hash[:], u.ID, now, ev.Seq, state[:]); err != nil {
		return none, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE channels SET epoch = ? WHERE id = ?`, eb.Epoch, ch); err != nil {
		return none, err
	}
	return ev, nil
}

func (s *Server) opKeyFetch(ctx context.Context, u proto.User, a proto.KeyFetchArgs) (any, error) {
	if a.From == 0 || a.To < a.From || a.To-a.From >= proto.MaxKeyFetch {
		return nil, badRequest("need 1 ≤ from ≤ to and at most 256 epochs")
	}
	// Like sync: check membership and open the read snapshot under the
	// write lock, so a member who just left can't read later epochs.
	s.mu.Lock()
	member := s.members[a.Chan][u.ID]
	var tx *sql.Tx
	var err error
	if member {
		tx, err = s.st.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err == nil {
			_, err = head(ctx, tx) // pins the snapshot before unlocking
		}
	}
	s.mu.Unlock()
	if !member {
		return nil, errForbidden
	}
	if tx != nil {
		defer tx.Rollback()
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT bundle, seq FROM epochs WHERE chan = ? AND epoch >= ? AND epoch <= ? ORDER BY epoch`,
		a.Chan, a.From, a.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := proto.KeyFetchRes{Bundles: []proto.Signed{}, Seqs: []uint64{}}
	size := 0
	for rows.Next() {
		var raw []byte
		var seq uint64
		if err := rows.Scan(&raw, &seq); err != nil {
			return nil, err
		}
		if size += len(raw) + 9; size > proto.MaxResultBytes { // + a msgpack u64
			break
		}
		var b proto.Signed
		if err := proto.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		res.Bundles = append(res.Bundles, b)
		res.Seqs = append(res.Seqs, seq)
	}
	return res, rows.Err()
}

// EpochStale reports whether encrypted channel ch needs a rotation before
// anyone encrypts to it: it has no bundle yet, its roster isn't the current
// non-disabled members, it boxes a revoked device, or a member's device
// was revoked after its key.epoch (that device may hold K by key.share,
// PROTOCOL §13.3). Unencrypted channels
// are never stale. It reads outside any transaction; a write that depends
// on the answer must call epochStale with its own tx inside s.write.
func (s *Server) EpochStale(ctx context.Context, ch uint64) (bool, error) {
	return epochStale(ctx, s.st.db, ch)
}

func epochStale(ctx context.Context, q querier, ch uint64) (bool, error) {
	h, err := chanEpoch(ctx, q, ch)
	if err != nil || !h.e2e {
		return false, err
	}
	cur := h.bundle
	if cur == nil {
		return true, nil
	}
	eb, err := identity.PeekEpochBundle(identity.Signed(*cur))
	if err != nil {
		return false, err
	}
	roster, err := rosterOf(ctx, q, ch)
	if err != nil {
		return false, err
	}
	if !slices.Equal(eb.Roster, roster) {
		return true, nil
	}
	devs := eb.BoxDevs() // 1..128, so a bounded number of placeholders
	args := make([]any, len(devs))
	for i := range devs {
		args[i] = devs[i][:]
	}
	var revoked int
	if err = q.QueryRowContext(ctx, revokedBoxesQuery(len(devs)), args...).Scan(&revoked); err != nil || revoked > 0 {
		return revoked > 0, err
	}
	return revokedSince(ctx, q, ch, h.seq)
}

// revokedSince reports whether a device of a current member of ch was
// revoked after seq (the current epoch's key.epoch). Such a device may
// hold the epoch's K without a box (key.share to a device registered
// after the bundle), so the epoch is stale (§13.3).
func revokedSince(ctx context.Context, q querier, ch, seq uint64) (bool, error) {
	var hit bool
	err := q.QueryRowContext(ctx, revokedSinceQuery, ch, seq).Scan(&hit)
	return hit, err
}

// (Members as in rosterQuery: not disabled.)
const revokedSinceQuery = `SELECT EXISTS (SELECT 1 FROM membership m JOIN users u ON u.id = m.user
		JOIN devices d ON d.user = m.user
		WHERE m.chan = ? AND m.leave_seq IS NULL AND u.disabled = 0 AND d.revoked IS NOT NULL AND d.revoked_seq > ?)`

// errHookE2E: webhooks can't post into encrypted channels (their text
// comes from outside, and only members' devices can encrypt).
var errHookE2E = proto.Errf(proto.ErrForbidden, "webhooks can't post into end-to-end encrypted channels")

// checkEnc applies the encrypted-send rules (§13.5) that follow the
// idempotency lookup: stale_epoch if enc isn't for the channel's current
// epoch or that epoch's bundle is stale, then enc must be signed by an
// active device of the caller. The server can't check anything inside it.
func checkEnc(ctx context.Context, tx *sql.Tx, sid e2ecrypto.ServerID, user, ch uint64, enc []byte,
	e e2ecrypto.Enc, head epochHead) error {
	if head.epoch == 0 || e.Epoch != head.epoch {
		return staleEpoch(fmt.Sprintf("channel is at epoch %d", head.epoch))
	}
	stale, err := epochStale(ctx, tx, ch)
	if err != nil {
		return err
	}
	if stale {
		return staleEpoch("the channel's key must be rotated first")
	}
	if !bytes.Equal(e.EH[:], head.hash[:min(len(head.hash), e2ecrypto.IDSize)]) {
		return badRequest("enc names another bundle of this epoch")
	}
	return checkEncSigner(ctx, tx, sid, user, ch, enc, e)
}

// checkEncSigner: enc must be signed by an active device of user.
func checkEncSigner(ctx context.Context, tx *sql.Tx, sid e2ecrypto.ServerID, user, ch uint64, enc []byte, e e2ecrypto.Enc) error {
	var owner uint64
	var edPub, revoked []byte
	err := tx.QueryRowContext(ctx, `SELECT user, ed25519, revoked FROM devices WHERE dev = ?`, e.Dev[:]).
		Scan(&owner, &edPub, &revoked)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (owner != user || revoked != nil)) {
		return badRequest("enc is not from one of your active devices")
	}
	if err != nil {
		return err
	}
	if _, err := e2ecrypto.VerifyEnc(ed25519.PublicKey(edPub), sid, ch, enc); err != nil {
		return badRequest("enc signature does not verify")
	}
	return nil
}

// checkEncOfMsg checks the enc of a delete or reaction (§13.5): it is
// sealed under the reacted/deleted message's own epoch (the reaction tag
// is keyed by it), so it must name the same epoch and bundle as the
// message's enc, and be signed by an active device of user.
func checkEncOfMsg(ctx context.Context, tx *sql.Tx, sid e2ecrypto.ServerID, user uint64, m proto.Message, enc []byte) error {
	// Their Plain fits one padding block; bigger ones would only let a
	// member bloat every copy of the message (one per reaction).
	if len(enc) > e2ecrypto.MinEncSize {
		return proto.Errf(proto.ErrTooLarge, "encrypted delete or reaction too long")
	}
	e, err := e2ecrypto.ParseEnc(enc)
	if err != nil {
		return badRequest("malformed enc")
	}
	orig, err := e2ecrypto.ParseEnc(m.Enc)
	if err != nil {
		return badRequest("this message has no encrypted payload")
	}
	if e.Epoch != orig.Epoch || e.EH != orig.EH {
		return badRequest("enc must use the message's epoch")
	}
	return checkEncSigner(ctx, tx, sid, user, m.Chan, enc, e)
}

// revokedBoxesQuery counts the revoked devices among n bound ids: n point
// lookups on the devices primary key.
func revokedBoxesQuery(n int) string {
	return `SELECT COUNT(*) FROM devices WHERE dev IN (?` + strings.Repeat(`, ?`, n-1) + `) AND revoked IS NOT NULL`
}

// snapshotKeys collects the current bundle of every e2e channel in chans that
// u belongs to (e2e1 sessions only).
func (sess *session) snapshotKeys(ctx context.Context, tx *sql.Tx, chans []proto.Channel, out *frames) error {
	var part proto.Snapshot
	size := 0
	for _, c := range chans {
		if !c.E2E || c.Epoch == 0 || !slices.Contains(c.Members, sess.user.ID) {
			continue
		}
		var raw []byte
		var seq uint64
		if err := tx.QueryRowContext(ctx, `SELECT bundle, seq FROM epochs WHERE chan = ? AND epoch = ?`, c.ID, c.Epoch).Scan(&raw, &seq); err != nil {
			return err
		}
		var b proto.Signed
		if err := proto.Unmarshal(raw, &b); err != nil {
			return err
		}
		if size+len(raw) > snapshotPart && size > 0 {
			if err := out.snapshot(part); err != nil {
				return err
			}
			part, size = proto.Snapshot{}, 0
		}
		part.Keys = append(part.Keys, proto.EpochKey{Bundle: b, Seq: seq})
		size += len(raw)
	}
	if size > 0 {
		return out.snapshot(part)
	}
	return nil
}
