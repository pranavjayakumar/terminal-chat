package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"slices"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// Devices (PROTOCOL.md §12.2). The server checks certs so an honest server
// turns garbage away early, but clients never rely on these checks.

func (s *Server) sid() e2ecrypto.ServerID {
	var id e2ecrypto.ServerID
	copy(id[:], s.st.serverID)
	return id
}

func isDeviceKind(kind string) bool { return kind == proto.KDeviceNew || kind == proto.KDeviceRevoke }

func signedSize(v proto.Signed) int { return len(v.B) + len(v.Sig) }

// userKeys returns the wire-encoded SSH keys registered for u.
func userKeys(ctx context.Context, q querier, u uint64) ([][]byte, error) {
	rows, err := q.QueryContext(ctx, `SELECT pubkey FROM keys WHERE user = ? ORDER BY created`, u)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var k []byte
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// verifyCert finds the caller's key that signed sc. Verification comes
// first; the unverified decode only picks the error code.
func (s *Server) verifyCert(sc proto.Signed, u uint64, keys [][]byte) (identity.DeviceCert, error) {
	sid := s.sid()
	for _, k := range keys {
		c, err := identity.Signed(sc).VerifyDeviceCert(sid, k)
		if err == nil {
			if c.User != u {
				return c, proto.Errf(proto.ErrForbidden, "device cert is for another user")
			}
			return c, nil
		}
		if errors.Is(err, identity.ErrWrongServer) {
			return c, badRequest("device cert is for another server")
		}
	}
	c, err := identity.PeekDeviceCert(identity.Signed(sc))
	switch {
	case err != nil:
		return c, badRequest("malformed device cert")
	case c.ServerID != sid:
		return c, badRequest("device cert is for another server")
	case c.User != u:
		return c, proto.Errf(proto.ErrForbidden, "device cert is for another user")
	case !slices.ContainsFunc(keys, func(k []byte) bool { return bytes.Equal(k, c.SSH) }):
		return c, proto.Errf(proto.ErrForbidden, "device cert is not signed by one of your registered SSH keys")
	}
	return c, badRequest("device cert signature does not verify")
}

func (s *Server) verifyEndorse(se proto.Signed, c identity.DeviceCert, keys [][]byte) error {
	sid := s.sid()
	for _, k := range keys {
		e, err := identity.Signed(se).VerifyKeyEndorse(sid, k)
		if err != nil {
			continue
		}
		if e.User != c.User || !bytes.Equal(e.NewSSH, c.SSH) {
			return badRequest("endorsement is not for this cert's key")
		}
		return nil
	}
	return badRequest("endorsement does not verify under any of your registered SSH keys")
}

func (s *Server) opDeviceRegister(ctx context.Context, u proto.User, a proto.DeviceRegisterArgs) (any, error) {
	if signedSize(a.Cert) > proto.MaxSigned || (a.Endorse != nil && signedSize(*a.Endorse) > proto.MaxSigned) {
		return nil, proto.Errf(proto.ErrTooLarge, "signed objects are limited to 4 KiB")
	}
	// Keys are only ever added, so verifying outside the write lock is safe
	// and keeps slow (RSA) verifications from stalling every writer.
	keys, err := userKeys(ctx, s.st.db, u.ID)
	if err != nil {
		return nil, err
	}
	c, err := s.verifyCert(a.Cert, u.ID, keys)
	if err != nil {
		return nil, err
	}
	var endorse []byte
	if a.Endorse != nil {
		if err := s.verifyEndorse(*a.Endorse, c, keys); err != nil {
			return nil, err
		}
		endorse = proto.MustRaw(*a.Endorse)
	}
	cert := proto.MustRaw(a.Cert)

	var res proto.SeqRes
	_, err = s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		var owner uint64
		var oldCert, oldEndorse []byte
		err := tx.QueryRowContext(ctx, `SELECT user, cert, endorse FROM devices WHERE dev = ?`, c.Dev[:]).
			Scan(&owner, &oldCert, &oldEndorse)
		if err == nil {
			if owner == u.ID && bytes.Equal(oldCert, cert) && bytes.Equal(oldEndorse, endorse) {
				return nil, nil // retry of a registration that already happened
			}
			return nil, proto.Errf(proto.ErrConflict, "device id already registered")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		var active, total int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE revoked IS NULL) FROM devices WHERE user = ?`,
			u.ID).Scan(&total, &active); err != nil {
			return nil, err
		}
		if active >= proto.MaxDevices {
			return nil, badRequest("too many devices: revoke one first")
		}
		if total >= proto.MaxDeviceRegistrations {
			return nil, badRequest("device registration limit reached")
		}
		now := s.now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO devices (dev, user, cert, endorse, created) VALUES (?, ?, ?, ?, ?)`,
			c.Dev[:], u.ID, cert, endorse, now); err != nil {
			return nil, err
		}
		ev, err := appendEvent(ctx, tx, proto.KDeviceNew, 0, u.ID, 0, now, a)
		res.Seq = ev.Seq
		return []proto.Event{ev}, err
	})
	return res, err
}

// activeDevice is a non-revoked device's id and Ed25519 key, from its stored
// (already verified) cert.
type activeDevice struct {
	id  e2ecrypto.DevID
	pub ed25519.PublicKey
}

func activeDevices(ctx context.Context, q querier, u uint64) ([]activeDevice, error) {
	rows, err := q.QueryContext(ctx, `SELECT cert FROM devices WHERE user = ? AND revoked IS NULL`, u)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []activeDevice
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var sc proto.Signed
		if err := proto.Unmarshal(raw, &sc); err != nil {
			return nil, err
		}
		c, err := identity.PeekDeviceCert(identity.Signed(sc))
		if err != nil {
			return nil, err
		}
		out = append(out, activeDevice{c.Dev, c.Ed25519})
	}
	return out, rows.Err()
}

// verifyRevoke checks sr under the caller's SSH keys, then under the caller's
// other active devices. by is the revoking device, if a device signed.
func (s *Server) verifyRevoke(ctx context.Context, sr proto.Signed, u uint64) (r identity.DeviceRevoke, by *e2ecrypto.DevID, err error) {
	sid := s.sid()
	keys, err := userKeys(ctx, s.st.db, u)
	if err != nil {
		return r, nil, err
	}
	for _, k := range keys {
		r, err := identity.Signed(sr).VerifyDeviceRevoke(sid, k)
		if err == nil {
			return r, nil, nil
		}
		if errors.Is(err, identity.ErrWrongServer) {
			return r, nil, badRequest("revoke is for another server")
		}
	}
	devs, err := activeDevices(ctx, s.st.db, u)
	if err != nil {
		return r, nil, err
	}
	for _, d := range devs {
		r, err := identity.Signed(sr).VerifyDeviceRevokeByDevice(sid, d.pub)
		if errors.Is(err, identity.ErrWrongServer) {
			return r, nil, badRequest("revoke is for another server")
		}
		if err == nil && r.Dev != d.id {
			return r, &d.id, nil
		}
	}
	r, err = identity.PeekDeviceRevoke(identity.Signed(sr))
	switch {
	case err != nil:
		return r, nil, badRequest("malformed revoke")
	case r.ServerID != sid:
		return r, nil, badRequest("revoke is for another server")
	}
	return r, nil, proto.Errf(proto.ErrForbidden, "revoke is not signed by one of your SSH keys or another active device")
}

func (s *Server) opDeviceRevoke(ctx context.Context, u proto.User, a proto.DeviceRevokeArgs) (any, error) {
	if signedSize(a.Revoke) > proto.MaxSigned {
		return nil, proto.Errf(proto.ErrTooLarge, "signed objects are limited to 4 KiB")
	}
	r, by, err := s.verifyRevoke(ctx, a.Revoke, u.ID)
	if err != nil {
		return nil, err
	}
	if r.User != u.ID {
		return nil, proto.Errf(proto.ErrForbidden, "revoke is for another user")
	}
	var res proto.SeqRes
	_, err = s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		var owner uint64
		var revoked []byte
		err := tx.QueryRowContext(ctx, `SELECT user, revoked FROM devices WHERE dev = ?`, r.Dev[:]).Scan(&owner, &revoked)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != u.ID) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		if revoked != nil {
			return nil, nil
		}
		if by != nil {
			// The signing device may have been revoked since we verified.
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE dev = ? AND revoked IS NULL`, by[:]).Scan(&n); err != nil {
				return nil, err
			}
			if n == 0 {
				return nil, proto.Errf(proto.ErrForbidden, "the revoking device is itself revoked")
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE devices SET revoked = ? WHERE dev = ?`, proto.MustRaw(a.Revoke), r.Dev[:]); err != nil {
			return nil, err
		}
		ev, err := appendEvent(ctx, tx, proto.KDeviceRevoke, 0, u.ID, 0, s.now(), a)
		res.Seq = ev.Seq
		return []proto.Event{ev}, err
	})
	return res, err
}

// deviceRow is one stored device, decoded back into its wire objects.
type deviceRow struct {
	user    uint64
	dev     proto.Device
	revoke  *proto.Signed
	encSize int
}

// listDevices returns devices of users (nil = all) with id > after, ordered
// by user then registration.
func listDevices(ctx context.Context, q querier, users []uint64, after uint64) ([]deviceRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT user, cert, endorse, revoked FROM devices WHERE user > ? ORDER BY user, created, dev`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deviceRow
	for rows.Next() {
		var d deviceRow
		var cert, endorse, revoked []byte
		if err := rows.Scan(&d.user, &cert, &endorse, &revoked); err != nil {
			return nil, err
		}
		if len(users) > 0 && !slices.Contains(users, d.user) {
			continue
		}
		if err := proto.Unmarshal(cert, &d.dev.Cert); err != nil {
			return nil, err
		}
		if endorse != nil {
			d.dev.Endorse = new(proto.Signed)
			if err := proto.Unmarshal(endorse, d.dev.Endorse); err != nil {
				return nil, err
			}
		}
		if revoked != nil {
			d.revoke = new(proto.Signed)
			if err := proto.Unmarshal(revoked, d.revoke); err != nil {
				return nil, err
			}
		}
		d.encSize = len(cert) + len(endorse) + len(revoked)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Server) opDevicesList(ctx context.Context, a proto.DevicesListArgs) (any, error) {
	if len(a.Users) > 4096 {
		return nil, badRequest("too many users")
	}
	devs, err := listDevices(ctx, s.st.db, a.Users, a.After)
	if err != nil {
		return nil, err
	}
	res := proto.DevicesListRes{Certs: []proto.Signed{}, Endorse: []proto.Signed{}, Revoked: []proto.Signed{}}
	size := 0
	for i := 0; i < len(devs); {
		// One user's devices go in whole or not at all; one user always fits
		// (MaxDeviceRegistrations × 3 × MaxSigned < MaxResultBytes).
		j, n := i, 0
		for ; j < len(devs) && devs[j].user == devs[i].user; j++ {
			n += devs[j].encSize
		}
		if size+n > proto.MaxResultBytes && size > 0 {
			res.Next = devs[i-1].user
			break
		}
		size += n
		for _, d := range devs[i:j] {
			res.Certs = append(res.Certs, d.dev.Cert)
			if d.dev.Endorse != nil && !slices.ContainsFunc(res.Endorse, func(e proto.Signed) bool {
				return bytes.Equal(e.B, d.dev.Endorse.B) && bytes.Equal(e.Sig, d.dev.Endorse.Sig)
			}) {
				res.Endorse = append(res.Endorse, *d.dev.Endorse)
			}
			if d.revoke != nil {
				res.Revoked = append(res.Revoked, *d.revoke)
			}
		}
		i = j
	}
	return res, nil
}

// snapshotDevices sends every device and revoke in parts (e2e1 sessions only).
func (sess *session) snapshotDevices(ctx context.Context, tx *sql.Tx) error {
	devs, err := listDevices(ctx, tx, nil, 0)
	if err != nil {
		return err
	}
	var part proto.Snapshot
	size := 0
	for _, d := range devs {
		if size+d.encSize > snapshotPart && size > 0 {
			if err := sess.pushSnapshot(part); err != nil {
				return err
			}
			part, size = proto.Snapshot{}, 0
		}
		part.Devs = append(part.Devs, d.dev)
		if d.revoke != nil {
			part.Revoked = append(part.Revoked, *d.revoke)
		}
		size += d.encSize
	}
	if size > 0 {
		return sess.pushSnapshot(part)
	}
	return nil
}
