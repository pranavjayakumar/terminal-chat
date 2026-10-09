package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
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
`

// The rotate rate caps, both waived when the roster or its active devices
// changed since the current epoch: one key.rotate per channel per user per
// 10 s, and one per channel per 2 s whoever rotates.
const (
	rotateEveryMs     = 10_000
	rotateChanEveryMs = 2_000
)

// migrateEpochs adds the epochs table, channels.e2e/epoch, epochs.seq and
// devices.revoked_seq (the seq of the device.revoke event).
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
	} {
		if err := addColumn(db, c[0], c[1], c[2]); err != nil {
			return err
		}
	}
	if err := migrateCreator(db); err != nil {
		return err
	}
	return backfillSeqs(db)
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
func isE2EKind(kind string) bool { return isDeviceKind(kind) || kind == proto.KKeyEpoch }

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

// rosterOf is ch's current non-disabled members, sorted: what a bundle's
// roster must be.
func rosterOf(ctx context.Context, q querier, ch uint64) ([]uint64, error) {
	rows, err := q.QueryContext(ctx, `SELECT m.user FROM membership m JOIN users u ON u.id = m.user
		WHERE m.chan = ? AND m.leave_seq IS NULL AND u.disabled = 0 ORDER BY m.user`, ch)
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

// boxesEveryUser reports whether every user owning a device in owners has
// at least one box in eb.
func boxesEveryUser(eb identity.EpochBundle, owners map[e2ecrypto.DevID]uint64) bool {
	boxed := map[uint64]bool{}
	for _, bx := range eb.Boxes {
		boxed[owners[bx.Dev]] = true
	}
	for _, u := range owners {
		if !boxed[u] {
			return false
		}
	}
	return true
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
	sid := s.sid()
	var res proto.SeqRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		// Cheap checks first: non-members never make the server decode 16 KiB.
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
		peek, err := identity.PeekEpochBundle(identity.Signed(a.Bundle))
		switch {
		case errors.Is(err, e2ecrypto.ErrTooLarge):
			return nil, proto.Errf(proto.ErrTooLarge, "epoch bundles are limited to 128 boxes")
		case err != nil:
			return nil, badRequest("malformed epoch bundle")
		case peek.ServerID != sid:
			return nil, badRequest("epoch bundle is for another server")
		case peek.Chan != a.Chan:
			return nil, badRequest("epoch bundle is for another channel")
		}
		// The rotator: one of the caller's active devices, whose key signed it.
		var owner uint64
		var edPub, revoked []byte
		err = tx.QueryRowContext(ctx, `SELECT user, ed25519, revoked FROM devices WHERE dev = ?`, peek.By[:]).
			Scan(&owner, &edPub, &revoked)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (owner != u.ID || revoked != nil)) {
			return nil, proto.Errf(proto.ErrForbidden, "bundle is not signed by one of your active devices")
		}
		if err != nil {
			return nil, err
		}
		eb, err := identity.Signed(a.Bundle).VerifyEpochBundle(sid, ed25519.PublicKey(edPub))
		if err != nil {
			return nil, badRequest("epoch bundle signature does not verify")
		}

		// Compare-and-set on the chain head.
		var prev [32]byte
		copy(prev[:], head.hash)
		if eb.Epoch != head.epoch+1 || eb.Prev != prev {
			return nil, staleEpoch(fmt.Sprintf("channel is at epoch %d", head.epoch))
		}
		// Who may extend the chain: a previous-roster user (the caller, since
		// by is the caller's device); for epoch 1 the creator, or any member
		// of a DM. Clients check the same (§13.3).
		if head.bundle != nil {
			old, err := identity.PeekEpochBundle(identity.Signed(*head.bundle))
			if err != nil {
				return nil, err
			}
			if !slices.Contains(old.Roster, u.ID) {
				return nil, proto.Errf(proto.ErrForbidden, "only members of the previous epoch may rotate")
			}
		} else if s.kinds[a.Chan] != proto.ChanDM {
			var creator uint64
			if err := tx.QueryRowContext(ctx, `SELECT creator FROM channels WHERE id = ?`, a.Chan).Scan(&creator); err != nil {
				return nil, err
			}
			if creator != u.ID {
				return nil, proto.Errf(proto.ErrForbidden, "only the channel's creator may create epoch 1")
			}
		}
		roster, err := rosterOf(ctx, tx, a.Chan)
		if err != nil {
			return nil, err
		}
		if !slices.Equal(eb.Roster, roster) {
			return nil, staleEpoch("roster is not the channel's current members")
		}
		devs, owners, err := rosterDevices(ctx, tx, roster)
		if err != nil {
			return nil, err
		}
		if err := checkBoxes(ctx, tx, eb, roster, devs, head.seq); err != nil {
			return nil, err
		}
		if err := eb.CheckBoxKeys(devs); err != nil {
			return nil, badRequest("bundle seals twice to one key")
		}
		// Clients may skip devices they haven't verified, but a member left
		// without any box could only ever get K from someone's key.share.
		if !boxesEveryUser(eb, owners) {
			return nil, badRequest("every member with a device needs at least one box")
		}

		now := s.now()
		state := epochState(roster, devs)
		// The caps apply unless the members or their active devices changed
		// since the current epoch was made. Compared against what the server
		// recorded then, not what the bundle boxed: otherwise a member with
		// one never-boxed device could rotate without limit.
		if head.bundle != nil && bytes.Equal(head.state, state[:]) {
			if now-head.time < rotateChanEveryMs {
				return nil, proto.Errf(proto.ErrRateLimited, "nothing changed since the channel's last rotation")
			}
			var last sql.NullInt64
			if err := tx.QueryRowContext(ctx, `SELECT MAX(time) FROM epochs WHERE chan = ? AND by_user = ?`,
				a.Chan, u.ID).Scan(&last); err != nil {
				return nil, err
			}
			if last.Valid && now-last.Int64 < rotateEveryMs {
				return nil, proto.Errf(proto.ErrRateLimited, "nothing changed since your last rotation")
			}
		}

		ev, err := appendEvent(ctx, tx, proto.KKeyEpoch, a.Chan, u.ID, 0, now, proto.KeyEpoch{Bundle: a.Bundle})
		if err != nil {
			return nil, err
		}
		hash := e2ecrypto.ChainHash(a.Bundle.B)
		if _, err := tx.ExecContext(ctx, `INSERT INTO epochs (chan, epoch, bundle, hash, by_user, time, seq, state) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			a.Chan, eb.Epoch, proto.MustRaw(a.Bundle), hash[:], u.ID, now, ev.Seq, state[:]); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE channels SET epoch = ? WHERE id = ?`, eb.Epoch, a.Chan); err != nil {
			return nil, err
		}
		res.Seq = ev.Seq
		return []proto.Event{ev}, nil
	})
	return res, err
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
// non-disabled members, or it boxes a revoked device. Unencrypted channels
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
	devs := eb.BoxDevs()
	ids := make([]string, len(devs))
	for i := range devs {
		ids[i] = strings.ToUpper(hex.EncodeToString(devs[i][:])) // SQLite's hex()
	}
	js, err := json.Marshal(ids)
	if err != nil {
		return false, err
	}
	var revoked int
	err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE revoked IS NOT NULL
		AND hex(dev) IN (SELECT value FROM json_each(?))`, string(js)).Scan(&revoked)
	return revoked > 0, err
}

// snapshotKeys sends the current bundle of every e2e channel in chans that
// u belongs to (e2e1 sessions only).
func (sess *session) snapshotKeys(ctx context.Context, tx *sql.Tx, chans []proto.Channel) error {
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
			if err := sess.pushSnapshot(part); err != nil {
				return err
			}
			part, size = proto.Snapshot{}, 0
		}
		part.Keys = append(part.Keys, proto.EpochKey{Bundle: b, Seq: seq})
		size += len(raw)
	}
	if size > 0 {
		return sess.pushSnapshot(part)
	}
	return nil
}
