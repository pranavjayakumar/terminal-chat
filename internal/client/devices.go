package client

import (
	"bytes"
	"context"
	"crypto/rand"
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

// storeDevice keeps a device.new / snapshot device. Malformed objects are dropped.
func storeDevice(q dbtx, d proto.Device) error {
	c, err := identity.PeekDeviceCert(identity.Signed(d.Cert))
	if err != nil {
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
	if err != nil {
		return nil
	}
	_, err = q.Exec(`INSERT OR IGNORE INTO device_endorse (endorse, user, new_ssh) VALUES (?, ?, ?)`,
		[]byte(proto.MustRaw(e)), ke.User, ke.NewSSH)
	return err
}

func storeRevoke(q dbtx, r proto.Signed) error {
	dr, err := identity.PeekDeviceRevoke(identity.Signed(r))
	if err != nil {
		return nil
	}
	_, err = q.Exec(`INSERT OR IGNORE INTO device_revokes (revoke, dev, user) VALUES (?, ?, ?)`,
		[]byte(proto.MustRaw(r)), dr.Dev[:], dr.User)
	return err
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
	keys, err := loadDeviceKeys(c.home, sid, me, nil)
	if err != nil {
		return err
	}
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
	var certs, revokes []proto.Signed
	for {
		raw, err := s.call(ctx, proto.OpDevicesList, args, nil)
		if err != nil {
			return err
		}
		var res proto.DevicesListRes
		if err := proto.Unmarshal(raw, &res); err != nil {
			return err
		}
		certs = append(certs, res.Certs...)
		revokes = append(revokes, res.Revoked...)
		if res.Next != 0 && res.Next <= args.After {
			return errors.New("devices.list paging does not advance")
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
			for _, r := range res.Revoked {
				if err := storeRevoke(tx, r); err != nil {
					return err
				}
			}
			if res.Next == 0 && len(args.Users) == 0 {
				return setMeta(tx, "devices_full", 1)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if res.Next == 0 {
			break
		}
		args.After = res.Next
	}

	mySSH := signer.PublicKey().Marshal()
	if ownRevoked(sid, me, keys.ID, mySSH, certs, revokes) {
		// This device is gone for good: continue as a new one.
		if keys, err = loadDeviceKeys(c.home, sid, me, &keys.ID); err != nil {
			return err
		}
	} else if slices.ContainsFunc(certs, func(sc proto.Signed) bool {
		dc, err := identity.PeekDeviceCert(identity.Signed(sc))
		return err == nil && dc.User == me && dc.Dev == keys.ID && bytes.Equal(dc.Ed25519, keys.Ed25519Pub)
	}) {
		return nil
	}
	cert, err := identity.NewDeviceCert(signer, sid, me, keys, time.Now())
	if err != nil {
		return err
	}
	// The device.new event lands in the cache through the event stream.
	_, err = s.call(ctx, proto.OpDeviceRegister, proto.DeviceRegisterArgs{Cert: proto.Signed(cert)}, nil)
	return err
}

// ownRevoked reports whether dev has a revoke we can check ourselves: signed
// by our SSH key, or by another of our devices whose cert our SSH key
// signed. A revoke we can't check (made with another of our SSH keys) is
// left for the trust store; a server can't make us drop keys by claiming one.
func ownRevoked(sid e2ecrypto.ServerID, me uint64, dev e2ecrypto.DevID, mySSH []byte, certs, revokes []proto.Signed) bool {
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
			if vr, err := identity.Signed(r).VerifyDeviceRevokeByDevice(sid, dc.Ed25519); err == nil && vr.User == me && vr.Dev == dev {
				return true
			}
		}
	}
	return false
}

// ---- device keys ----

// deviceKeyFile holds this device's E2E secret keys:
// server_id(16) || user(u64 BE) || e2ecrypto.DeviceKeys.MarshalBinary.
// device.key.lock serializes processes sharing TC_HOME (TUI + tc send).
//
// TODO(internal/trust): temporary. The trust store (TC_HOME/e2e.db,
// docs/E2E.md PR plan step 3) owns device keys once it lands on this branch;
// migrate this file into it.
const deviceKeyFile = "device.key"

const deviceKeyHeader = e2ecrypto.IDSize + 8

// loadDeviceKeys returns this device's keys for (sid, user), generating and
// persisting them (0600) on first use. A file for another server or user
// (TC_HOME re-used for a different login), or holding the keys of device
// retire, is replaced by fresh keys.
func loadDeviceKeys(home string, sid e2ecrypto.ServerID, user uint64, retire *e2ecrypto.DevID) (*e2ecrypto.DeviceKeys, error) {
	path := filepath.Join(home, deviceKeyFile)
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("locking device key: %w", err)
	}
	defer lock.Close() // releases the flock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("locking device key: %w", err)
	}

	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading device key: %w", err)
	}
	if err == nil && len(b) == deviceKeyHeader+e2ecrypto.DeviceKeysSize &&
		bytes.Equal(b[:e2ecrypto.IDSize], sid[:]) && binary.BigEndian.Uint64(b[e2ecrypto.IDSize:]) == user {
		var k e2ecrypto.DeviceKeys
		if err := k.UnmarshalBinary(b[deviceKeyHeader:]); err != nil {
			return nil, fmt.Errorf("reading device key: %w", err)
		}
		if retire == nil || k.ID != *retire {
			return &k, nil
		}
	}
	return writeDeviceKeys(path, sid, user)
}

// writeDeviceKeys replaces path with fresh keys (atomically, 0600). The
// caller holds the lock.
func writeDeviceKeys(path string, sid e2ecrypto.ServerID, user uint64) (*e2ecrypto.DeviceKeys, error) {
	k, err := e2ecrypto.Generate(rand.Reader)
	if err != nil {
		return nil, err
	}
	secret, err := k.MarshalBinary()
	if err != nil {
		return nil, err
	}
	b := make([]byte, deviceKeyHeader, deviceKeyHeader+len(secret))
	copy(b, sid[:])
	binary.BigEndian.PutUint64(b[e2ecrypto.IDSize:], user)
	b = append(b, secret...)

	tmp, err := os.CreateTemp(filepath.Dir(path), "device-*.key")
	if err != nil {
		return nil, fmt.Errorf("writing device key: %w", err)
	}
	defer os.Remove(tmp.Name())
	err = tmp.Chmod(0o600)
	if err == nil {
		_, err = tmp.Write(b)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return nil, fmt.Errorf("writing device key: %w", err)
	}
	return k, nil
}
