package client

import (
	"bytes"
	"context"
	"encoding/hex"
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

// Device management (PROTOCOL.md §13.2): the "I lost my laptop" flow.
//
//	func (c *Client) Devices(user uint64) []DeviceInfo                       // local, works offline
//	func (c *Client) RefreshDevices(ctx, user) error                          // devices.list, then verify
//	func (c *Client) FindDevice(user uint64, prefix string) (DeviceInfo, error)
//	func (c *Client) RevokeDevice(ctx, dev, allowSelf) (rotated int, err error)

var (
	// ErrRevokeSelf refuses to revoke the device we are running on unless
	// the caller explicitly allows it.
	ErrRevokeSelf = errors.New("that is this device (revoke it from another device, or allow it explicitly)")
	// ErrAlreadyRevoked means the device was revoked before.
	ErrAlreadyRevoked = errors.New("that device is already revoked")
	// ErrNoDevice means no device of the user matches.
	ErrNoDevice = errors.New("no such device")
)

// DeviceRef names a device (Update.Revoked, Update.NewOwn).
type DeviceRef struct {
	User uint64
	Dev  e2ecrypto.DevID
}

// Short is the device's id as shown to people.
func (d DeviceRef) Short() string { return ShortDevID(d.Dev) }

// ShortDevID is the first 8 hex digits of a device id: what tc shows and
// what `tc devices revoke` accepts (any unique prefix works).
func ShortDevID(id e2ecrypto.DevID) string { return hex.EncodeToString(id[:4]) }

// DeviceInfo is one device of a user as this client knows it.
type DeviceInfo struct {
	ID      e2ecrypto.DevID
	User    uint64
	Created time.Time // from the signed cert (the registering device's clock)
	// KeyFP is the SHA256 fingerprint of the SSH key that signed the cert.
	KeyFP string
	// Verified: the cert verifies under a key this device pinned for User.
	// An unverified device is only the server's claim.
	Verified bool
	// This is the device this client runs on.
	This bool
	// Former is an earlier id of this device: keys it retired, e.g. after
	// it was revoked and re-registered.
	Former bool
	// Revoked by a revoke this client verified (or, for an unverified
	// device, one the server sent).
	Revoked   bool
	RevokedAt time.Time // the revoke's signed time; zero if unknown
}

// Short is the device's id as shown to people.
func (d DeviceInfo) Short() string { return ShortDevID(d.ID) }

// Devices lists user's devices from the local cache and trust store:
// active ones first (this device first), then by creation time. Run
// RefreshDevices first for an up-to-date list.
func (c *Client) Devices(user uint64) []DeviceInfo {
	sid, ok := c.cacheServerID()
	if !ok {
		return nil
	}
	var this e2ecrypto.DevID
	var former []formerDev
	haveThis := false
	if user == c.Me().ID {
		this, haveThis = c.ourDev(c.db)
		former = c.formerDevs(sid, user)
	}
	verified, _ := c.trust.Devices(sid, user)
	var out []DeviceInfo
	seen := map[e2ecrypto.DevID]bool{}
	add := func(raw []byte, v *trust.Device) {
		var s proto.Signed
		if proto.Unmarshal(raw, &s) != nil {
			return
		}
		dc, err := identity.PeekDeviceCert(identity.Signed(s))
		if err != nil || dc.User != user || dc.ServerID != sid || seen[dc.Dev] {
			return
		}
		seen[dc.Dev] = true
		d := DeviceInfo{ID: dc.Dev, User: user, Created: time.UnixMilli(dc.Created), Verified: v != nil,
			This: haveThis && dc.Dev == this}
		// Only a verified row: the server's claim alone mustn't look like ours.
		d.Former = !d.This && v != nil && isFormer(former, dc.Dev, dc.X25519, dc.Ed25519)
		if pk, err := ssh.ParsePublicKey(dc.SSH); err == nil {
			d.KeyFP = ssh.FingerprintSHA256(pk)
		}
		if v != nil {
			d.Revoked = v.Revoked
			if v.RevokedTime != 0 {
				d.RevokedAt = time.UnixMilli(v.RevokedTime)
			}
		}
		out = append(out, d)
	}
	for i := range verified {
		add(verified[i].Cert, &verified[i])
	}
	rows, err := c.db.Query(`SELECT cert FROM devices WHERE user = ? ORDER BY rowid`, user)
	if err == nil {
		for rows.Next() {
			var raw []byte
			if rows.Scan(&raw) == nil {
				add(raw, nil)
			}
		}
		rows.Close()
	}
	// Unverified devices: the server's revokes are all we have.
	for i := range out {
		if out[i].Verified {
			continue
		}
		var n int
		c.db.QueryRow(`SELECT COUNT(*) FROM device_revokes WHERE user = ? AND dev = ?`, user, out[i].ID[:]).Scan(&n)
		out[i].Revoked = n > 0
	}
	slices.SortStableFunc(out, func(a, b DeviceInfo) int {
		switch {
		case a.Revoked != b.Revoked:
			if a.Revoked {
				return 1
			}
			return -1
		case a.This != b.This:
			if a.This {
				return -1
			}
			return 1
		}
		return a.Created.Compare(b.Created)
	})
	return out
}

// FindDevice resolves a device id or a unique prefix of its hex (as shown
// by Short) among user's known devices.
func (c *Client) FindDevice(user uint64, prefix string) (DeviceInfo, error) {
	p := strings.ToLower(strings.TrimSpace(prefix))
	if p == "" {
		return DeviceInfo{}, ErrNoDevice
	}
	var hit []DeviceInfo
	for _, d := range c.Devices(user) {
		if strings.HasPrefix(hex.EncodeToString(d.ID[:]), p) {
			hit = append(hit, d)
		}
	}
	if len(hit) > 1 {
		// A server can invent unverified look-alikes of a real device's
		// short id; they mustn't block naming the real one.
		var v []DeviceInfo
		for _, d := range hit {
			if d.Verified {
				v = append(v, d)
			}
		}
		if len(v) == 1 {
			hit = v
		}
	}
	switch len(hit) {
	case 0:
		return DeviceInfo{}, ErrNoDevice
	case 1:
		return hit[0], nil
	}
	return DeviceInfo{}, fmt.Errorf("device id %q is ambiguous: give more digits", p)
}

// RefreshDevices fetches user's devices from the server (devices.list) and
// verifies them against pinned keys, so Devices is current.
func (c *Client) RefreshDevices(ctx context.Context, user uint64) error {
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
	if _, _, err := c.fetchDevices(ctx, s, proto.DevicesListArgs{Users: []uint64{user}}); err != nil {
		return err
	}
	c.e2eMu.Lock()
	defer c.e2eMu.Unlock()
	c.verifyDevices(sid, s.sshPub, c.Me().ID)
	return nil
}

// RevokeOpts are RevokeDeviceOpts's options.
type RevokeOpts struct {
	// Self allows revoking the device we run on.
	Self bool
	// Force revokes this device even when the account has no device
	// registrations left, so it can never register again.
	Force bool
}

// ErrLastRegistration refuses to revoke this device when it couldn't
// register again afterwards (RevokeOpts.Force overrides).
var ErrLastRegistration = fmt.Errorf("%w: revoked, this device could never register again and would lose encryption for good", ErrNoRegistrations)

// RevokeDevice is RevokeDeviceOpts with RevokeOpts{Self: allowSelf}.
func (c *Client) RevokeDevice(ctx context.Context, dev e2ecrypto.DevID, allowSelf bool) (rotated int, err error) {
	return c.RevokeDeviceOpts(ctx, dev, RevokeOpts{Self: allowSelf})
}

// RevokeDeviceOpts revokes one of our own devices for good (device.revoke).
// It signs with our SSH key, or, when that can't sign (agent locked, key
// unavailable), with this device's key. Revoking the device we run on
// needs o.Self; this device then reconnects as a new device (refused with
// ErrLastRegistration when no registration is left, unless o.Force).
//
// It works while this device isn't registered (e.g. the account has the
// most devices allowed, so the server refused it): that is when an old
// device needs revoking. This device then registers in the freed slot.
//
// Afterwards every encrypted channel we're in whose current key the revoked
// device may hold (boxed to it, or shared with it by key.share) gets a new
// epoch without it right away (rotated counts them; a channel that fails to
// rotate now, or can't because this device isn't registered, rotates before
// anyone's next send anyway).
func (c *Client) RevokeDeviceOpts(ctx context.Context, dev e2ecrypto.DevID, o RevokeOpts) (rotated int, err error) {
	s, sid, err := c.e2eConn(ctx)
	if err != nil {
		return 0, err
	}
	me := c.Me().ID
	this, err := c.trust.DeviceKeys(sid, me)
	if err != nil {
		return 0, err
	}
	self := this.ID == dev
	if self && !o.Self {
		return 0, ErrRevokeSelf
	}
	registered := s.registered()
	mine := s.myDevice()
	if !registered || (mine != nil && mine.ID != this.ID) {
		mine = nil // the server doesn't know it (or it's being replaced): can't sign revokes
	}
	if self && !o.Force {
		certs, revokes, err := c.fetchDevices(ctx, s, proto.DevicesListArgs{Users: []uint64{me}})
		if err != nil {
			return 0, err
		}
		if _, total := countDevices(me, certs, revokes); total >= proto.MaxDeviceRegistrations {
			return 0, ErrLastRegistration
		}
	}
	d, ok, err := c.trust.Device(sid, dev)
	if err != nil {
		return 0, err
	}
	if !ok {
		// Maybe registered since our last look.
		if err := c.RefreshDevices(ctx, me); err != nil {
			return 0, err
		}
		if d, ok, err = c.trust.Device(sid, dev); err != nil {
			return 0, err
		}
	}
	if !ok || d.User != me {
		return 0, fmt.Errorf("%w: %s isn't a verified device of yours", ErrNoDevice, ShortDevID(dev))
	}
	if d.Revoked {
		return 0, ErrAlreadyRevoked
	}

	now := time.Now()
	var rev identity.Signed
	signer, serr := identity.Load(c.cfg.KeyPath)
	if serr == nil && !bytes.Equal(signer.PublicKey().Marshal(), s.sshPub) {
		serr = errors.New("the SSH key changed since this session logged in")
	}
	if serr == nil {
		rev, serr = identity.NewDeviceRevoke(signer, sid, me, dev, now)
	}
	if serr != nil {
		if self || mine == nil {
			return 0, fmt.Errorf("signing the revoke with your SSH key: %w", serr)
		}
		if rev, err = identity.NewDeviceRevokeByDevice(mine, sid, me, dev, now); err != nil {
			return 0, fmt.Errorf("signing the revoke: %w", err)
		}
	}
	// Other clients judge which epochs the revoked device may hold by the
	// server's seqs; this one must not need to. Every channel owes a
	// rotation (recorded before the revoke, so a crash can't skip it) until
	// we commit an epoch of our own without the device: anyone else's newer
	// epoch may have reached it by key.share. Sends here rotate first, and
	// the e2e worker keeps trying.
	// Held through the revoke so the e2e worker neither uses keys a --self
	// revoke retires nor rotates in between (the rotations below are ours).
	c.e2eMu.Lock()
	locked := true
	defer func() {
		if locked {
			c.e2eMu.Unlock()
		}
	}()
	if err := c.oweRotationsLocked(sid, me, dev); err != nil {
		return 0, err
	}
	raw, err := s.call(ctx, proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: proto.Signed(rev)}, nil)
	if err != nil {
		return 0, err
	}
	var res proto.SeqRes
	_ = proto.Unmarshal(raw, &res)
	// Don't wait for the event: the trust store must know before we rotate.
	if err := storeRevoke(c.db, proto.Signed(rev), res.Seq); err != nil {
		return 0, err
	}
	if err := c.oweRotationsLocked(sid, me, dev); err != nil {
		return 0, err
	}
	if self {
		// Our keys are dead: retire them now (we signed the revoke, so we
		// needn't wait for the server to list it) and reconnect, which
		// registers the new keys (ensureDevice); the next session's worker
		// makes the owed rotations (rotateOwed).
		keys, err := c.trust.RetireDeviceKeys(sid, me, dev)
		if err == nil {
			c.myDev.Store(&keys.ID)
		}
		s.restart()
		return 0, err
	}
	if !registered {
		// The revoke may have freed the slot the server refused us: register
		// now, so this device can rotate (and read and send) at once. The
		// worker registers, so let go of e2eMu meanwhile.
		c.e2eMu.Unlock()
		locked = false
		if c.retryDevice(ctx, s) != nil || !s.registered() {
			c.post(Update{Channels: true})
			return 0, nil // DeviceError says why; the worker rotates once registered
		}
		c.e2eMu.Lock()
		locked = true
	}

	c.verifyDevices(sid, s.sshPub, me)
	chans, err := c.e2eChannels(sid, me)
	if err != nil {
		return 0, nil
	}
	for _, ch := range chans {
		if ctx.Err() != nil {
			break
		}
		// Every current epoch of ours predates the revoke we just made, and
		// the revoked device (ours) may hold its key: rotate them all
		// rather than trust the server's seqs (revokedStale) to say which.
		if _, ok, err := c.trust.ChainHead(sid, ch.ID); err != nil || !ok {
			continue
		}
		if c.rotateLocked(ctx, s, sid, ch.ID) == nil {
			rotated++
		}
	}
	c.post(Update{Channels: true})
	return rotated, nil
}

// oweRotationsLocked records that every encrypted channel we're in owes a
// rotation away from revoked device dev (trust.OweRotation): paid only by
// an epoch we commit ourselves. Caller holds e2eMu.
func (c *Client) oweRotationsLocked(sid e2ecrypto.ServerID, me uint64, dev e2ecrypto.DevID) error {
	chans, err := c.e2eChannels(sid, me)
	if err != nil {
		return err
	}
	for _, ch := range chans {
		if err := c.trust.OweRotation(sid, ch.ID, dev); err != nil {
			return err
		}
	}
	return nil
}

// retryDevice asks session s's e2e worker to register this device again
// and waits (at most timing.devWait) for the outcome.
func (c *Client) retryDevice(ctx context.Context, s *session) error {
	t := time.NewTimer(c.timing.devWait)
	defer t.Stop()
	done := make(chan error, 1)
	select {
	case s.devRetry <- done:
	case <-s.done:
		return ErrOffline
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return fmt.Errorf("%w yet (still trying)", ErrDeviceNotRegistered)
	}
	select {
	case err := <-done:
		return err
	case <-s.done:
		return ErrOffline
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return fmt.Errorf("%w yet (still trying)", ErrDeviceNotRegistered)
	}
}

// RegistrationsLeft is how many more devices the account may ever register
// (proto.MaxDeviceRegistrations minus every device the cache knows of ours,
// revoked ones included): the server's word, so only for warnings.
func (c *Client) RegistrationsLeft() int {
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(DISTINCT dev) FROM devices WHERE user = ?`, c.Me().ID).Scan(&n); err != nil {
		return proto.MaxDeviceRegistrations
	}
	return max(proto.MaxDeviceRegistrations-n, 0)
}
