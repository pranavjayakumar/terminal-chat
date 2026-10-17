package client

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/trust"
)

// Devices (PROTOCOL.md §13.2).
//
// Everything stored here comes from the server and is NOT trusted: rows are
// indexed by the unverified claims inside each object, and all distinct
// objects are kept (a server could send a fake cert for a real device id
// first). Trust decisions — verifying certs against pinned SSH keys, revokes
// against pinned keys or verified devices — come with the trust store
// (docs/E2E.md PR plan step 6).

func (s *session) e2e() bool { return slices.Contains(s.welcome.Caps, proto.CapE2E) }

// maxUnverifiedPerUser bounds how many unverified objects of each kind the
// cache keeps per user, so a malicious server can't grow it without limit.
const maxUnverifiedPerUser = 64

// keepUnverified reports whether an unverified object claiming (sid, user)
// may be stored: it must name this server and the user must be under the cap.
func keepUnverified(q dbtx, table string, sid []byte, user uint64) bool {
	if cur, err := metaBytes(q, "server_id"); err == nil && len(cur) > 0 && !bytes.Equal(cur, sid) {
		return false
	}
	var n int
	q.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE user = ?`, user).Scan(&n)
	return n < maxUnverifiedPerUser
}

// storeDevice keeps a device.new / snapshot device. Malformed objects are dropped.
func storeDevice(q dbtx, d proto.Device) error {
	c, err := identity.PeekDeviceCert(identity.Signed(d.Cert))
	if err != nil || !keepUnverified(q, "devices", c.ServerID[:], c.User) {
		return nil
	}
	if _, err := q.Exec(`INSERT OR IGNORE INTO devices (cert, dev, user) VALUES (?, ?, ?)`,
		[]byte(proto.MustRaw(d.Cert)), c.Dev[:], c.User); err != nil {
		return err
	}
	if d.Endorse != nil {
		return storeEndorse(q, *d.Endorse)
	}
	return nil
}

func storeEndorse(q dbtx, e proto.Signed) error {
	ke, err := identity.PeekKeyEndorse(identity.Signed(e))
	if err != nil || !keepUnverified(q, "device_endorse", ke.ServerID[:], ke.User) {
		return nil
	}
	_, err = q.Exec(`INSERT OR IGNORE INTO device_endorse (endorse, user, new_ssh) VALUES (?, ?, ?)`,
		[]byte(proto.MustRaw(e)), ke.User, ke.NewSSH)
	return err
}

// storeRevoke keeps a revoke with the seq of its device.revoke event (0 if
// unknown: then only the signed-time rule applies until a real seq
// arrives). The lowest known seq wins; 0 never beats a known one.
func storeRevoke(q dbtx, r proto.Signed, seq uint64) error {
	dr, err := identity.PeekDeviceRevoke(identity.Signed(r))
	if err != nil || !keepUnverified(q, "device_revokes", dr.ServerID[:], dr.User) {
		return nil
	}
	_, err = q.Exec(`INSERT INTO device_revokes (revoke, dev, user, seq) VALUES (?, ?, ?, ?)
		ON CONFLICT (revoke) DO UPDATE SET seq = CASE WHEN excluded.seq = 0 THEN seq
			WHEN seq = 0 THEN excluded.seq ELSE min(seq, excluded.seq) END`,
		[]byte(proto.MustRaw(r)), dr.Dev[:], dr.User, seq)
	return err
}

// seqAt is seqs[i], or 0 when the server sent no (or too few) seqs.
func seqAt(seqs []uint64, i int) uint64 {
	if i < len(seqs) {
		return seqs[i]
	}
	return 0
}

// ensureDevice runs once per e2e1 session, after sync: it refreshes the
// cached device list and registers this device if the server doesn't have
// it yet. Failures are retried on the next connection.
func (c *Client) ensureDevice(ctx context.Context, s *session, signer ssh.Signer) error {
	if len(s.welcome.ServerID) != e2ecrypto.IDSize {
		return errors.New("server id is not 16 bytes")
	}
	var sid e2ecrypto.ServerID
	copy(sid[:], s.welcome.ServerID)
	me := c.Me().ID
	keys, err := c.trust.DeviceKeys(sid, me)
	if err != nil {
		return err
	}
	s.dev.Store(keys)
	c.myDev.Store(&keys.ID)
	// Once the cache holds every device (a full list, or a reset snapshot
	// in an e2e1 session) events keep it current; then only our own
	// devices need checking.
	full, err := metaUint(c.db, "devices_full")
	if err != nil {
		return err
	}
	args := proto.DevicesListArgs{}
	if full == 1 {
		args.Users = []uint64{me}
	}
	certs, revokes, err := c.fetchDevices(ctx, s, args)
	if err != nil {
		return err
	}

	mySSH := signer.PublicKey().Marshal()
	if ownRevoked(sid, me, keys.ID, mySSH, certs, revokes) {
		// This device is gone for good: continue as a new one.
		if keys, err = c.trust.RetireDeviceKeys(sid, me, keys.ID); err != nil {
			return err
		}
		s.dev.Store(keys)
		c.myDev.Store(&keys.ID)
	} else if slices.ContainsFunc(certs, func(sc proto.Signed) bool {
		dc, err := identity.PeekDeviceCert(identity.Signed(sc))
		return err == nil && dc.User == me && dc.Dev == keys.ID && bytes.Equal(dc.Ed25519, keys.Ed25519Pub)
	}) {
		s.devOnce.Do(func() { close(s.devReady) })
		return nil
	}
	cert, err := identity.NewDeviceCert(signer, sid, me, keys, time.Now())
	if err != nil {
		return err
	}
	// The device.new event lands in the cache through the event stream.
	if _, err = s.call(ctx, proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: proto.Signed(cert)}, nil); err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) && pe.Code == proto.ErrBadRequest {
			// The server's counts are its word, but they only pick the advice.
			switch active, total := countDevices(me, certs, revokes); {
			case active >= proto.MaxDevices:
				return fmt.Errorf("%w (%d active, the most allowed): revoke one you no longer use with tc devices revoke <id> or /devices", ErrTooManyDevices, active)
			case total >= proto.MaxDeviceRegistrations:
				return ErrNoRegistrations
			}
		}
		return fmt.Errorf("registering this device: %w", err)
	}
	s.devOnce.Do(func() { close(s.devReady) })
	return nil
}

var (
	// ErrDeviceNotRegistered: this device has no registered keys, so it
	// can't read or send encrypted messages (yet).
	ErrDeviceNotRegistered = errors.New("this device isn't registered for encryption")
	// ErrTooManyDevices: registering would pass proto.MaxDevices.
	ErrTooManyDevices = errors.New("you have too many devices")
	// ErrNoRegistrations: the account used all proto.MaxDeviceRegistrations.
	ErrNoRegistrations = fmt.Errorf("your account has used all %d device registrations it may ever make", proto.MaxDeviceRegistrations)
)

// countDevices counts user's devices in a devices.list answer: active
// (not revoked) and total (every registration, revoked included).
func countDevices(user uint64, certs, revokes []proto.Signed) (active, total int) {
	revoked := map[e2ecrypto.DevID]bool{}
	for _, r := range revokes {
		if dr, err := identity.PeekDeviceRevoke(identity.Signed(r)); err == nil && dr.User == user {
			revoked[dr.Dev] = true
		}
	}
	seen := map[e2ecrypto.DevID]bool{}
	for _, sc := range certs {
		dc, err := identity.PeekDeviceCert(identity.Signed(sc))
		if err != nil || dc.User != user || seen[dc.Dev] {
			continue
		}
		seen[dc.Dev] = true
		total++
		if !revoked[dc.Dev] {
			active++
		}
	}
	return active, total
}

// tryDevice runs ensureDevice and records the outcome for e2eSession.
func (c *Client) tryDevice(ctx context.Context, s *session, signer ssh.Signer) error {
	err := c.ensureDevice(ctx, s, signer)
	if err != nil {
		s.devErr.Store(&err)
	} else {
		s.devErr.Store(nil)
	}
	s.devTriedOne.Do(func() { close(s.devTried) })
	c.post(Update{Status: true})
	return err
}

// registered reports whether the server has this session's device.
func (s *session) registered() bool {
	select {
	case <-s.devReady:
		return true
	default:
		return false
	}
}

// deviceErr is why this session's device isn't registered (nil if it is,
// or no attempt has finished yet).
func (s *session) deviceErr() error {
	if s.registered() {
		return nil
	}
	if p := s.devErr.Load(); p != nil {
		if errors.Is(*p, ErrDeviceNotRegistered) {
			return *p
		}
		return fmt.Errorf("%w: %w", ErrDeviceNotRegistered, *p)
	}
	return nil
}

// DeviceError is why this device isn't registered for encryption on the
// current connection (nil when it is, while offline, or not known yet).
func (c *Client) DeviceError() error {
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil || !s.e2e() {
		return nil
	}
	return s.deviceErr()
}

// fetchDevices pages through devices.list with args and stores everything
// in the cache (unverified, like events). A complete list of everyone marks
// the cache full. It returns the raw certs and revokes it got.
func (c *Client) fetchDevices(ctx context.Context, s *session, args proto.DevicesListArgs) (certs, revokes []proto.Signed, err error) {
	for {
		raw, err := s.call(ctx, proto.OpDevicesList, args, nil)
		if err != nil {
			return nil, nil, err
		}
		var res proto.DevicesListRes
		if err := proto.Unmarshal(raw, &res); err != nil {
			return nil, nil, err
		}
		certs = append(certs, res.Certs...)
		revokes = append(revokes, res.Revoked...)
		if res.Next != 0 && res.Next <= args.After {
			return nil, nil, errors.New("devices.list paging does not advance")
		}
		err = c.tx(func(tx *sql.Tx) error {
			for _, sc := range res.Certs {
				if err := storeDevice(tx, proto.Device{Cert: sc}); err != nil {
					return err
				}
			}
			for _, e := range res.Endorse {
				if err := storeEndorse(tx, e); err != nil {
					return err
				}
			}
			for i, r := range res.Revoked {
				if err := storeRevoke(tx, r, seqAt(res.RevokedSeqs, i)); err != nil {
					return err
				}
			}
			if res.Next == 0 && len(args.Users) == 0 {
				return setMeta(tx, "devices_full", 1)
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		if res.Next == 0 {
			return certs, revokes, nil
		}
		args.After = res.Next
	}
}

// ownRevoked reports whether dev has a revoke we can check ourselves: signed
// by our SSH key, or by another of our devices whose cert our SSH key
// signed. A revoke we can't check (made with another of our SSH keys) is
// left for the trust store; a server can't make us drop keys by claiming one.
func ownRevoked(sid e2ecrypto.ServerID, me uint64, dev e2ecrypto.DevID, mySSH []byte, certs, revokes []proto.Signed) bool {
	return revokedBy(sid, me, dev, mySSH, certs, revokes, true)
}

// revokedBy reports whether a revoke of dev verifies under mySSH or under
// another of our devices. With strict, a signing device counts only if no
// revoke of it verifies the same way: a stolen, already revoked device
// can't make our healthy devices drop their keys (step 12 security pass).
// Two devices revoking each other thus cancel out; the trust store judges
// them by seq and time.
func revokedBy(sid e2ecrypto.ServerID, me uint64, dev e2ecrypto.DevID, mySSH []byte, certs, revokes []proto.Signed, strict bool) bool {
	for _, r := range revokes {
		pr, err := identity.PeekDeviceRevoke(identity.Signed(r))
		if err != nil || pr.User != me || pr.Dev != dev {
			continue
		}
		if vr, err := identity.Signed(r).VerifyDeviceRevoke(sid, mySSH); err == nil && vr.User == me && vr.Dev == dev {
			return true
		}
		for _, sc := range certs {
			dc, err := identity.Signed(sc).VerifyDeviceCert(sid, mySSH)
			if err != nil || dc.User != me || dc.Dev == dev {
				continue
			}
			if strict && revokedBy(sid, me, dc.Dev, mySSH, certs, revokes, false) {
				continue
			}
			if vr, err := identity.Signed(r).VerifyDeviceRevokeByDevice(sid, dc.Ed25519); err == nil && vr.User == me && vr.Dev == dev {
				return true
			}
		}
	}
	return false
}

// ---- device keys ----

// deviceKeyFile is where device keys lived before the trust store owned
// them: server_id(16) || user(u64 BE) || e2ecrypto.DeviceKeys.MarshalBinary,
// with device.key.lock serializing processes sharing TC_HOME. Open moves it
// into the trust store once and deletes it.
const deviceKeyFile = "device.key"

const deviceKeyHeader = e2ecrypto.IDSize + 8

// migrateDeviceKeyFile imports home/device.key into ts and removes the file
// only once the trust store holds exactly those keys (imported now, or
// already, e.g. by another process). It holds the old lock while doing so,
// so a concurrent process (old or new) never sees a half-migrated state. A
// malformed file, one without a user, or one whose keys conflict with keys
// already stored is left alone (and doesn't stop Open): it may be the only
// copy of a key.
func migrateDeviceKeyFile(home string, ts *trust.Store) error {
	path := filepath.Join(home, deviceKeyFile)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("locking device key: %w", err)
	}
	defer lock.Close() // releases the flock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking device key: %w", err)
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // migrated by another process while we waited
	}
	if err != nil {
		return fmt.Errorf("reading device key: %w", err)
	}
	if len(b) != deviceKeyHeader+e2ecrypto.DeviceKeysSize {
		return nil
	}
	var sid e2ecrypto.ServerID
	copy(sid[:], b)
	user := binary.BigEndian.Uint64(b[e2ecrypto.IDSize:])
	var k e2ecrypto.DeviceKeys
	if k.UnmarshalBinary(b[deviceKeyHeader:]) != nil {
		return nil
	}
	if user == 0 {
		return nil
	}
	res, err := ts.ImportDeviceKeys(sid, user, &k)
	if err != nil {
		return err
	}
	if res != trust.ImportStored && res != trust.ImportPresent {
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing %s: %w", deviceKeyFile, err)
	}
	_ = os.Remove(path + ".lock")
	return nil
}
