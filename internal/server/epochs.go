package server

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	PRIMARY KEY (chan, epoch)
);
-- The rate cap looks up a user's last rotation under the write lock.
CREATE INDEX IF NOT EXISTS epochs_by ON epochs(chan, by_user, time);
`

// rotateEveryMs: one key.rotate per channel per user per 10 s, unless the
// roster or device set changed since the current epoch.
const rotateEveryMs = 10_000

// migrateEpochs adds the epochs table and channels.e2e/epoch.
func migrateEpochs(db *sql.DB) error {
	if _, err := db.Exec(epochsSchema); err != nil {
		return fmt.Errorf("creating epochs schema: %w", err)
	}
	if err := addColumn(db, "channels", "e2e", `INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	return addColumn(db, "channels", "epoch", `INTEGER NOT NULL DEFAULT 0`)
}

// isE2EKind: events only e2e1 sessions receive, live or replayed.
func isE2EKind(kind string) bool { return isDeviceKind(kind) || kind == proto.KKeyEpoch }

// chanEpoch returns whether ch is encrypted and its current bundle (nil at
// epoch 0) with its chain hash.
func chanEpoch(ctx context.Context, q querier, ch uint64) (e2e bool, epoch uint32, bundle *proto.Signed, hash []byte, err error) {
	if err = q.QueryRowContext(ctx, `SELECT e2e, epoch FROM channels WHERE id = ?`, ch).Scan(&e2e, &epoch); err != nil {
		return
	}
	if epoch == 0 {
		return
	}
	var raw []byte
	if err = q.QueryRowContext(ctx, `SELECT bundle, hash FROM epochs WHERE chan = ? AND epoch = ?`, ch, epoch).
		Scan(&raw, &hash); err != nil {
		return
	}
	bundle = new(proto.Signed)
	err = proto.Unmarshal(raw, bundle)
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

// rosterDevices maps the non-revoked devices of users to their X25519 keys:
// exactly the devices a bundle must box to.
func rosterDevices(ctx context.Context, q querier, users []uint64) (map[e2ecrypto.DevID][32]byte, error) {
	js, err := json.Marshal(users)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT dev, x25519 FROM devices
		WHERE revoked IS NULL AND user IN (SELECT value FROM json_each(?))`, string(js))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[e2ecrypto.DevID][32]byte{}
	for rows.Next() {
		var dev, x []byte
		if err := rows.Scan(&dev, &x); err != nil {
			return nil, err
		}
		var id e2ecrypto.DevID
		var k [32]byte
		if len(dev) != len(id) || len(x) != len(k) {
			return nil, fmt.Errorf("malformed stored device %x", dev)
		}
		copy(id[:], dev)
		copy(k[:], x)
		out[id] = k
	}
	return out, rows.Err()
}

// sameBoxes reports whether eb boxes exactly the devices in devs.
func sameBoxes(eb identity.EpochBundle, devs map[e2ecrypto.DevID][32]byte) bool {
	if len(eb.Boxes) != len(devs) {
		return false
	}
	for _, bx := range eb.Boxes {
		if _, ok := devs[bx.Dev]; !ok {
			return false
		}
	}
	return true
}

func staleEpoch(msg string) error { return proto.Errf(proto.ErrStaleEpoch, msg) }

func (s *Server) opKeyRotate(ctx context.Context, u proto.User, a proto.KeyRotateArgs) (any, error) {
	if signedSize(a.Bundle) > proto.MaxBundle {
		return nil, proto.Errf(proto.ErrTooLarge, "epoch bundles are limited to 16 KiB")
	}
	sid := s.sid()
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
	var res proto.SeqRes
	_, err = s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		if _, ok := s.kinds[a.Chan]; !ok {
			return nil, errNotFound
		}
		if !s.members[a.Chan][u.ID] {
			return nil, errForbidden
		}
		e2e, cur, curBundle, curHash, err := chanEpoch(ctx, tx, a.Chan)
		if err != nil {
			return nil, err
		}
		if !e2e {
			return nil, badRequest("channel is not encrypted")
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
		copy(prev[:], curHash)
		if eb.Epoch != cur+1 || eb.Prev != prev {
			return nil, staleEpoch(fmt.Sprintf("channel is at epoch %d", cur))
		}
		roster, err := rosterOf(ctx, tx, a.Chan)
		if err != nil {
			return nil, err
		}
		if !slices.Equal(eb.Roster, roster) {
			return nil, staleEpoch("roster is not the channel's current members")
		}
		devs, err := rosterDevices(ctx, tx, roster)
		if err != nil {
			return nil, err
		}
		if !sameBoxes(eb, devs) {
			return nil, staleEpoch("boxes are not exactly the members' active devices")
		}
		if err := eb.CheckBoxKeys(devs); err != nil {
			return nil, badRequest("bundle seals twice to one key")
		}

		now := s.now()
		if curBundle != nil {
			old, err := identity.PeekEpochBundle(identity.Signed(*curBundle))
			if err != nil {
				return nil, err
			}
			if slices.Equal(old.Roster, roster) && sameBoxes(old, devs) {
				var last sql.NullInt64
				if err := tx.QueryRowContext(ctx, `SELECT MAX(time) FROM epochs WHERE chan = ? AND by_user = ?`,
					a.Chan, u.ID).Scan(&last); err != nil {
					return nil, err
				}
				if last.Valid && now-last.Int64 < rotateEveryMs {
					return nil, proto.Errf(proto.ErrRateLimited, "nothing changed since the last rotation")
				}
			}
		}

		hash := e2ecrypto.ChainHash(a.Bundle.B)
		if _, err := tx.ExecContext(ctx, `INSERT INTO epochs (chan, epoch, bundle, hash, by_user, time) VALUES (?, ?, ?, ?, ?, ?)`,
			a.Chan, eb.Epoch, proto.MustRaw(a.Bundle), hash[:], u.ID, now); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE channels SET epoch = ? WHERE id = ?`, eb.Epoch, a.Chan); err != nil {
			return nil, err
		}
		ev, err := appendEvent(ctx, tx, proto.KKeyEpoch, a.Chan, u.ID, 0, now, proto.KeyEpoch{Bundle: a.Bundle})
		res.Seq = ev.Seq
		return []proto.Event{ev}, err
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
	rows, err := tx.QueryContext(ctx, `SELECT bundle FROM epochs WHERE chan = ? AND epoch >= ? AND epoch <= ? ORDER BY epoch`,
		a.Chan, a.From, a.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := proto.KeyFetchRes{Bundles: []proto.Signed{}}
	size := 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if size += len(raw); size > proto.MaxResultBytes {
			break
		}
		var b proto.Signed
		if err := proto.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		res.Bundles = append(res.Bundles, b)
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
	e2e, _, cur, _, err := chanEpoch(ctx, q, ch)
	if err != nil || !e2e {
		return false, err
	}
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
		if err := tx.QueryRowContext(ctx, `SELECT bundle FROM epochs WHERE chan = ? AND epoch = ?`, c.ID, c.Epoch).Scan(&raw); err != nil {
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
		part.Keys = append(part.Keys, b)
		size += len(raw)
	}
	if size > 0 {
		return sess.pushSnapshot(part)
	}
	return nil
}
