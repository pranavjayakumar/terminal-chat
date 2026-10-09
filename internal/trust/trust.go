// Package trust is the client's end-to-end trust store, TC_HOME/e2e.db
// (docs/E2E.md, "Review r1 → r2").
//
// It holds everything a malicious server must not be able to make the client
// forget: this device's private keys, pinned SSH keys, verified devices,
// per-channel epoch chain heads and the sticky "this is encrypted" flags. It is
// a separate file from the message cache so that a cache reset (which the
// server can force) never touches it. Every row is keyed by the server's
// 16-byte server_id, so state from two servers never mixes.
//
// Several processes (the TUI and `tc tail`) may open the same file; every
// read-modify-write runs in one IMMEDIATE transaction.
package trust

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite"

	"github.com/skailama/tc/internal/e2ecrypto"
)

// FileName is the trust store's file name inside TC_HOME.
const FileName = "e2e.db"

// Schema history: v1 = step 3; v2 adds device_keys.user, revoked_devices.seq,
// epochs and chan_alarms; v3 adds revoked_devices.time; v4 adds server_pins
// and server_alarms; v5 makes revoked_devices.seq nullable (NULL = unknown;
// v1–v4 stored unknown as 0, which the migration turns into NULL); v6 adds
// key_changes.last_seen (pending key changes are an LRU of maxKeyChanges).
const schemaVersion = 6

// maxKeyChanges bounds the pending key changes kept per user and server; the
// least recently seen is dropped (it is re-recorded if seen again).
const maxKeyChanges = 16

const schema = `
CREATE TABLE IF NOT EXISTS device_keys (
	server_id BLOB PRIMARY KEY, keys BLOB NOT NULL, created INTEGER NOT NULL,
	user INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS pinned_keys (
	server_id BLOB NOT NULL, user INTEGER NOT NULL, fp TEXT NOT NULL, wire BLOB NOT NULL,
	how TEXT NOT NULL, first_seen INTEGER NOT NULL, PRIMARY KEY (server_id, user, fp));
CREATE TABLE IF NOT EXISTS key_changes (
	server_id BLOB NOT NULL, user INTEGER NOT NULL, fp TEXT NOT NULL, wire BLOB NOT NULL,
	how TEXT NOT NULL, first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (server_id, user, fp));
CREATE TABLE IF NOT EXISTS devices (
	server_id BLOB NOT NULL, dev BLOB NOT NULL, user INTEGER NOT NULL, x25519 BLOB NOT NULL,
	ed25519 BLOB NOT NULL, cert BLOB NOT NULL, PRIMARY KEY (server_id, dev));
CREATE TABLE IF NOT EXISTS revoked_devices (
	server_id BLOB NOT NULL, dev BLOB NOT NULL, seq INTEGER,
	time INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (server_id, dev));
CREATE TABLE IF NOT EXISTS chain_heads (
	server_id BLOB NOT NULL, chan INTEGER NOT NULL, epoch INTEGER NOT NULL, hash BLOB NOT NULL,
	eh BLOB NOT NULL, PRIMARY KEY (server_id, chan));
CREATE TABLE IF NOT EXISTS e2e_servers (server_id BLOB PRIMARY KEY);
CREATE TABLE IF NOT EXISTS e2e_chans (
	server_id BLOB NOT NULL, chan INTEGER NOT NULL, PRIMARY KEY (server_id, chan));
CREATE TABLE IF NOT EXISTS epochs (
	server_id BLOB NOT NULL, chan INTEGER NOT NULL, epoch INTEGER NOT NULL, hash BLOB NOT NULL,
	kc BLOB NOT NULL, roster BLOB NOT NULL, back BLOB, k BLOB, PRIMARY KEY (server_id, chan, epoch));
CREATE TABLE IF NOT EXISTS chan_alarms (
	server_id BLOB NOT NULL, chan INTEGER NOT NULL, detail TEXT NOT NULL, time INTEGER NOT NULL,
	PRIMARY KEY (server_id, chan));
CREATE TABLE IF NOT EXISTS server_pins (
	addr TEXT NOT NULL, pin TEXT NOT NULL, server_id BLOB NOT NULL, first_seen INTEGER NOT NULL,
	PRIMARY KEY (addr, pin));
CREATE TABLE IF NOT EXISTS server_alarms (
	server_id BLOB PRIMARY KEY, detail TEXT NOT NULL, time INTEGER NOT NULL);
`

// migrateV1 adds the v2 columns to a v1 file (the v2 tables come from schema).
const migrateV1 = `
ALTER TABLE device_keys ADD COLUMN user INTEGER NOT NULL DEFAULT 0;
ALTER TABLE revoked_devices ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;
`

// migrateV2 adds the v3 column to a v1 or v2 file.
const migrateV2 = `
ALTER TABLE revoked_devices ADD COLUMN time INTEGER NOT NULL DEFAULT 0;
`

// migrateV4 rebuilds revoked_devices with a nullable seq; a stored 0 meant
// "unknown" but won every min(), invalidating every bundle of the device.
const migrateV4 = `
CREATE TABLE revoked_devices_v5 (
	server_id BLOB NOT NULL, dev BLOB NOT NULL, seq INTEGER,
	time INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (server_id, dev));
INSERT INTO revoked_devices_v5 (server_id, dev, seq, time)
	SELECT server_id, dev, NULLIF(seq, 0), time FROM revoked_devices;
DROP TABLE revoked_devices;
ALTER TABLE revoked_devices_v5 RENAME TO revoked_devices;
`

var (
	// ErrKeyChanged means a user presented an SSH key that isn't pinned and
	// isn't endorsed. The key was recorded as a pending change; nothing may
	// be sealed to it until AcknowledgeKeyChange.
	ErrKeyChanged = errors.New("trust: ssh key changed")
	// ErrNoKeyChange means there is no pending change for that key.
	ErrNoKeyChange = errors.New("trust: no pending key change")
	// ErrNoAnchor means an endorsed key was offered for a user with no pinned
	// key, so there is nothing the endorsement could have been checked against.
	ErrNoAnchor = errors.New("trust: endorsement without a pinned key")
	// ErrRollback means a chain head older than the pinned one was offered.
	ErrRollback = errors.New("trust: epoch rollback")
	// ErrFork means a different bundle was offered for the pinned epoch.
	ErrFork = errors.New("trust: epoch chain fork")
	// ErrDeviceConflict means a device id is already stored with different keys or owner.
	ErrDeviceConflict = errors.New("trust: device id conflict")
	// ErrInvalid means malformed arguments.
	ErrInvalid = errors.New("trust: invalid argument")
	// ErrGap means an epoch was offered that isn't the one after the head.
	ErrGap = errors.New("trust: epoch does not follow the head")
	// ErrUnknownEpoch means no verified bundle is recorded for the epoch.
	ErrUnknownEpoch = errors.New("trust: epoch not verified")
	// ErrKeyMismatch means a key doesn't match the epoch's key commitment.
	ErrKeyMismatch = errors.New("trust: key does not match the epoch's kc")
	// ErrServerChanged means the server at a pinned address and TLS pin
	// presented another server_id than the one first seen there.
	ErrServerChanged = errors.New("server identity changed — log in again to trust it")
)

// How records why an SSH key is trusted.
type How string

const (
	// TOFU: the first key seen for the user (or anchored by the invite).
	TOFU How = "tofu"
	// Endorsed: a KeyEndorse signed by an already-pinned key vouches for it.
	Endorsed How = "endorsed"
	// Verified: confirmed out of band (/verify).
	Verified How = "verified"
)

// rank orders trust levels; re-pinning a key never lowers its level.
func (h How) rank() int {
	switch h {
	case TOFU:
		return 1
	case Endorsed:
		return 2
	case Verified:
		return 3
	}
	return 0
}

// SSHKey is a pinned key or a pending key change.
type SSHKey struct {
	User        uint64
	Fingerprint string // ssh.FingerprintSHA256 of Wire
	Wire        []byte // ssh wire encoding
	How         How
	FirstSeen   time.Time
}

// Device is a device whose cert the caller has verified.
type Device struct {
	ID      e2ecrypto.DevID
	User    uint64
	X25519  [32]byte
	Ed25519 [32]byte
	Cert    []byte // exact Signed cert bytes as received
	Revoked bool
	// RevokedSeq is the seq of the revoke's event (0 = unknown: only the
	// time rule applies until a real seq arrives). It invalidates bundles
	// with a later seq (PROTOCOL §13.3).
	RevokedSeq uint64
	// RevokedTime is the signed time of the revoke (Unix ms; 0 = unknown). A
	// bundle the device signed at or after it is invalid whatever its seq
	// (docs/E2E.md step 6).
	RevokedTime int64
}

// Head is a channel's pinned epoch chain head.
type Head struct {
	Epoch uint32
	Hash  [32]byte            // e2ecrypto.ChainHash of the bundle's b
	EH    e2ecrypto.EpochHash // e2ecrypto.EH of the same b (Hash[:16])
}

// Store is an open trust store. It is safe for concurrent use, including by
// other processes holding their own Store on the same file.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) home/e2e.db. home is created 0700 and the
// database files are forced to 0600, tightening pre-existing ones.
func Open(home string) (*Store, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", home, err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return nil, fmt.Errorf("securing %s: %w", home, err)
	}
	path := filepath.Join(home, FileName)
	// Create with 0600 before SQLite does (it would honour the umask); an
	// existing file keeps its mode, and SQLite copies it to -wal/-shm.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creating trust store: %w", err)
	}
	err = f.Chmod(0o600)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("securing trust store: %w", err)
	}
	for _, p := range []string{path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("securing trust store: %w", err)
		}
	}
	// IMMEDIATE transactions take the write lock up front so busy_timeout
	// covers contention between processes sharing the file.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening trust store: %w", err)
	}
	s := &Store{db: db}
	// Two processes creating the file at once can see SQLITE_BUSY without
	// the busy handler running (the switch to WAL); retry for a while.
	for i := 0; ; i++ {
		err = s.tx(initSchema)
		if err == nil {
			return s, nil
		}
		if i == 50 || !strings.Contains(err.Error(), "SQLITE_BUSY") {
			db.Close()
			return nil, fmt.Errorf("creating trust store schema: %w", err)
		}
		time.Sleep(time.Duration(10+i*10) * time.Millisecond)
	}
}

func initSchema(tx *sql.Tx) error {
	var v int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > schemaVersion {
		return fmt.Errorf("trust store schema v%d is newer than this tc (v%d)", v, schemaVersion)
	}
	if v == 1 {
		if _, err := tx.Exec(migrateV1); err != nil {
			return fmt.Errorf("migrating trust store v1: %w", err)
		}
	}
	if v == 1 || v == 2 {
		if _, err := tx.Exec(migrateV2); err != nil {
			return fmt.Errorf("migrating trust store v%d: %w", v, err)
		}
	}
	if v >= 1 && v <= 4 {
		if _, err := tx.Exec(migrateV4); err != nil {
			return fmt.Errorf("migrating trust store v%d: %w", v, err)
		}
	}
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	if v >= 1 && v <= 5 {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('key_changes') WHERE name = 'last_seen'`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := tx.Exec(`ALTER TABLE key_changes ADD COLUMN last_seen INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("migrating trust store v%d: %w", v, err)
			}
		}
	}
	_, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// tx runs fn in one IMMEDIATE transaction, committing if fn returns nil.
func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// SQLite integers are signed; ids are stored bit-for-bit.
func i64(v uint64) int64 { return int64(v) }

// ---- device keys ----

// DeviceKeys returns this device's keys for user on the server, generating
// and storing them with crypto/rand on first use. Concurrent first calls
// from several processes all get the same keys. This is the only place the
// private keys are persisted.
//
// One device per server: keys stored for another user (TC_HOME re-used for
// a different login) are replaced by fresh ones. Keys from before keys were
// bound to a user (user 0) are claimed by the first caller.
func (s *Store) DeviceKeys(sid e2ecrypto.ServerID, user uint64) (*e2ecrypto.DeviceKeys, error) {
	return s.deviceKeys(sid, user, nil)
}

// RetireDeviceKeys replaces the server's device keys with fresh ones if they
// are still those of device old (revoked for good), and returns the current
// keys. Concurrent callers retiring the same device all get the same new keys.
func (s *Store) RetireDeviceKeys(sid e2ecrypto.ServerID, user uint64, old e2ecrypto.DevID) (*e2ecrypto.DeviceKeys, error) {
	return s.deviceKeys(sid, user, &old)
}

func (s *Store) deviceKeys(sid e2ecrypto.ServerID, user uint64, retire *e2ecrypto.DevID) (*e2ecrypto.DeviceKeys, error) {
	if user == 0 {
		return nil, fmt.Errorf("loading device keys: %w: user 0", ErrInvalid)
	}
	var raw []byte
	err := s.tx(func(tx *sql.Tx) error {
		var owner int64
		err := tx.QueryRow(`SELECT keys, user FROM device_keys WHERE server_id = ?`, sid[:]).Scan(&raw, &owner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (owner == 0 || uint64(owner) == user) {
			var k e2ecrypto.DeviceKeys
			if err := k.UnmarshalBinary(raw); err != nil {
				return err
			}
			if retire == nil || k.ID != *retire {
				if owner == 0 {
					_, err = tx.Exec(`UPDATE device_keys SET user = ? WHERE server_id = ?`, i64(user), sid[:])
				}
				return err
			}
		}
		k, err := e2ecrypto.Generate(rand.Reader)
		if err != nil {
			return err
		}
		if raw, err = k.MarshalBinary(); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO device_keys (server_id, keys, created, user) VALUES (?, ?, ?, ?)
			ON CONFLICT (server_id) DO UPDATE SET keys = excluded.keys, created = excluded.created, user = excluded.user`,
			sid[:], raw, time.Now().UnixNano(), i64(user))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("loading device keys: %w", err)
	}
	var k e2ecrypto.DeviceKeys
	if err := k.UnmarshalBinary(raw); err != nil {
		return nil, fmt.Errorf("loading device keys: %w", err)
	}
	return &k, nil
}

// ImportResult is what ImportDeviceKeys did.
type ImportResult int

const (
	// ImportStored: k is now the server's device keys.
	ImportStored ImportResult = iota + 1
	// ImportPresent: exactly these keys were already stored for user.
	ImportPresent
	// ImportConflict: other keys (or keys of another user) are stored; k
	// was not stored.
	ImportConflict
)

// ImportDeviceKeys stores k as user's device keys for the server unless the
// store already has keys for it (migration from TC_HOME/device.key), and
// says which happened. Only ImportStored and ImportPresent mean the caller
// may delete its copy of k.
func (s *Store) ImportDeviceKeys(sid e2ecrypto.ServerID, user uint64, k *e2ecrypto.DeviceKeys) (ImportResult, error) {
	raw, err := k.MarshalBinary()
	if err != nil || user == 0 {
		return 0, fmt.Errorf("importing device keys: %w", errors.Join(ErrInvalid, err))
	}
	var res ImportResult
	err = s.tx(func(tx *sql.Tx) error {
		var cur []byte
		var owner int64
		err := tx.QueryRow(`SELECT keys, user FROM device_keys WHERE server_id = ?`, sid[:]).Scan(&cur, &owner)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			res = ImportStored
			_, err = tx.Exec(`INSERT INTO device_keys (server_id, keys, created, user) VALUES (?, ?, ?, ?)`,
				sid[:], raw, time.Now().UnixNano(), i64(user))
			return err
		case err != nil:
			return err
		case subtle.ConstantTimeCompare(cur, raw) == 1 && (owner == 0 || uint64(owner) == user):
			res = ImportPresent
		default:
			res = ImportConflict
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("importing device keys: %w", err)
	}
	return res, nil
}

// ---- SSH keys ----

func checkKey(fp string, wire []byte) error {
	pk, err := ssh.ParsePublicKey(wire)
	if err != nil {
		return fmt.Errorf("%w: ssh key: %v", ErrInvalid, err)
	}
	if ssh.FingerprintSHA256(pk) != fp {
		return fmt.Errorf("%w: fingerprint does not match key", ErrInvalid)
	}
	return nil
}

// PinKey records that user's SSH key (fp = ssh.FingerprintSHA256 of wire) is
// trusted for reason how:
//
//   - the user's first key is pinned (Endorsed is refused with ErrNoAnchor);
//   - an already-pinned key is kept, upgrading how if it is stronger;
//   - another key with how == Endorsed (its KeyEndorse verified by the caller
//     against a pinned key) is pinned alongside the existing ones;
//   - any other new key is recorded as a pending change and ErrKeyChanged is
//     returned; it is pinned only by AcknowledgeKeyChange.
func (s *Store) PinKey(sid e2ecrypto.ServerID, user uint64, fp string, wire []byte, how How) error {
	if how.rank() == 0 {
		return fmt.Errorf("%w: pin reason %q", ErrInvalid, how)
	}
	if err := checkKey(fp, wire); err != nil {
		return err
	}
	changed := false
	err := s.tx(func(tx *sql.Tx) error {
		var cur How
		err := tx.QueryRow(`SELECT how FROM pinned_keys WHERE server_id = ? AND user = ? AND fp = ?`,
			sid[:], i64(user), fp).Scan(&cur)
		if err == nil {
			if how.rank() > cur.rank() {
				_, err = tx.Exec(`UPDATE pinned_keys SET how = ? WHERE server_id = ? AND user = ? AND fp = ?`,
					how, sid[:], i64(user), fp)
			}
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM pinned_keys WHERE server_id = ? AND user = ?`,
			sid[:], i64(user)).Scan(&n); err != nil {
			return err
		}
		now := time.Now().UnixNano()
		switch {
		case n == 0 && how == Endorsed:
			return ErrNoAnchor
		case n == 0 || how == Endorsed:
			if _, err := tx.Exec(`INSERT INTO pinned_keys (server_id, user, fp, wire, how, first_seen)
				VALUES (?, ?, ?, ?, ?, ?)`, sid[:], i64(user), fp, wire, how, now); err != nil {
				return err
			}
			_, err := tx.Exec(`DELETE FROM key_changes WHERE server_id = ? AND user = ? AND fp = ?`,
				sid[:], i64(user), fp)
			return err
		}
		changed = true
		var pend How
		err = tx.QueryRow(`SELECT how FROM key_changes WHERE server_id = ? AND user = ? AND fp = ?`,
			sid[:], i64(user), fp).Scan(&pend)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.Exec(`INSERT INTO key_changes (server_id, user, fp, wire, how, first_seen, last_seen)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, sid[:], i64(user), fp, wire, how, now, now)
		case err == nil:
			if how.rank() < pend.rank() {
				how = pend
			}
			_, err = tx.Exec(`UPDATE key_changes SET how = ?, last_seen = ? WHERE server_id = ? AND user = ? AND fp = ?`,
				how, now, sid[:], i64(user), fp)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM key_changes WHERE server_id = ? AND user = ? AND fp NOT IN (
			SELECT fp FROM key_changes WHERE server_id = ? AND user = ? ORDER BY last_seen DESC, fp LIMIT ?)`,
			sid[:], i64(user), sid[:], i64(user), maxKeyChanges)
		return err
	})
	if err != nil {
		return fmt.Errorf("pinning key of user %d: %w", user, err)
	}
	if changed {
		return fmt.Errorf("user %d key %s: %w", user, fp, ErrKeyChanged)
	}
	return nil
}

// AcknowledgeKeyChange pins a pending key (the user accepted the key-change
// banner), keeping the user's other pinned keys.
func (s *Store) AcknowledgeKeyChange(sid e2ecrypto.ServerID, user uint64, fp string) error {
	err := s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO pinned_keys (server_id, user, fp, wire, how, first_seen)
			SELECT server_id, user, fp, wire, how, first_seen FROM key_changes
			WHERE server_id = ? AND user = ? AND fp = ?`, sid[:], i64(user), fp); err != nil {
			return err
		}
		del, err := tx.Exec(`DELETE FROM key_changes WHERE server_id = ? AND user = ? AND fp = ?`,
			sid[:], i64(user), fp)
		if err != nil {
			return err
		}
		if n, err := del.RowsAffected(); err != nil || n == 0 {
			return errors.Join(ErrNoKeyChange, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("acknowledging key of user %d: %w", user, err)
	}
	return nil
}

func scanKeys(rows *sql.Rows, err error) ([]SSHKey, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SSHKey
	for rows.Next() {
		var k SSHKey
		var user, ns int64
		if err := rows.Scan(&user, &k.Fingerprint, &k.Wire, &k.How, &ns); err != nil {
			return nil, err
		}
		k.User, k.FirstSeen = uint64(user), time.Unix(0, ns)
		out = append(out, k)
	}
	return out, rows.Err()
}

// PinnedKeys returns user's pinned SSH keys, oldest first.
func (s *Store) PinnedKeys(sid e2ecrypto.ServerID, user uint64) ([]SSHKey, error) {
	return scanKeys(s.db.Query(`SELECT user, fp, wire, how, first_seen FROM pinned_keys
		WHERE server_id = ? AND user = ? ORDER BY first_seen, fp`, sid[:], i64(user)))
}

// PendingKeyChanges returns every unacknowledged key change on the server, oldest first.
func (s *Store) PendingKeyChanges(sid e2ecrypto.ServerID) ([]SSHKey, error) {
	return scanKeys(s.db.Query(`SELECT user, fp, wire, how, first_seen FROM key_changes
		WHERE server_id = ? ORDER BY first_seen, user, fp`, sid[:]))
}

// ---- devices ----

// PutDevice stores a device whose cert the caller verified. Storing the same
// device again is a no-op (the first cert is kept); the same id with a
// different owner or keys is ErrDeviceConflict. Revocation is permanent:
// d.Revoked == true revokes, false never un-revokes.
func (s *Store) PutDevice(sid e2ecrypto.ServerID, d Device) error {
	if d.User == 0 || len(d.Cert) == 0 {
		return fmt.Errorf("putting device: %w: missing user or cert", ErrInvalid)
	}
	err := s.tx(func(tx *sql.Tx) error {
		var user int64
		var x, ed []byte
		err := tx.QueryRow(`SELECT user, x25519, ed25519 FROM devices WHERE server_id = ? AND dev = ?`,
			sid[:], d.ID[:]).Scan(&user, &x, &ed)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(`INSERT INTO devices (server_id, dev, user, x25519, ed25519, cert)
				VALUES (?, ?, ?, ?, ?, ?)`, sid[:], d.ID[:], i64(d.User), d.X25519[:], d.Ed25519[:], d.Cert); err != nil {
				return err
			}
		case err != nil:
			return err
		case uint64(user) != d.User || subtle.ConstantTimeCompare(x, d.X25519[:]) != 1 ||
			subtle.ConstantTimeCompare(ed, d.Ed25519[:]) != 1:
			return ErrDeviceConflict
		}
		if d.Revoked {
			return revoke(tx, sid, d.ID, d.RevokedSeq, d.RevokedTime)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("putting device %x: %w", d.ID, err)
	}
	return nil
}

func revoke(tx *sql.Tx, sid e2ecrypto.ServerID, dev e2ecrypto.DevID, seq uint64, at int64) error {
	// The earliest known revoke seq and time win: they invalidate the most
	// bundles. Seq 0 (stored NULL) and time 0 are "unknown" and never beat a
	// known value.
	var s sql.NullInt64
	if seq != 0 {
		s = sql.NullInt64{Int64: i64(seq), Valid: true}
	}
	_, err := tx.Exec(`INSERT INTO revoked_devices (server_id, dev, seq, time) VALUES (?, ?, ?, ?)
		ON CONFLICT (server_id, dev) DO UPDATE SET
		seq = CASE WHEN excluded.seq IS NULL THEN seq WHEN seq IS NULL THEN excluded.seq
			ELSE min(seq, excluded.seq) END,
		time = CASE WHEN excluded.time = 0 THEN time WHEN time = 0 THEN excluded.time
			ELSE min(time, excluded.time) END`, sid[:], dev[:], s, max(at, 0))
	return err
}

// RevokeDevice marks a device revoked, permanently, at an unknown seq and
// time (so no bundle is invalidated by it until one is known). The device
// need not be stored yet: a revocation seen before the cert still applies.
func (s *Store) RevokeDevice(sid e2ecrypto.ServerID, dev e2ecrypto.DevID) error {
	return s.RevokeDeviceAt(sid, dev, 0)
}

// RevokeDeviceAt marks a device revoked, permanently, by the revoke at event
// seq (0 = unknown): bundles with a later seq must not be signed by it or
// box to it. Revoking again keeps the lowest known seq.
func (s *Store) RevokeDeviceAt(sid e2ecrypto.ServerID, dev e2ecrypto.DevID, seq uint64) error {
	return s.RevokeDeviceAtTime(sid, dev, seq, 0)
}

// RevokeDeviceAtTime is RevokeDeviceAt that also records the revoke's signed
// time (Unix ms, 0 = unknown), which invalidates bundles the device signed at
// or after it even if the server ordered them before the revoke. Revoking
// again keeps the earliest seq and the earliest known time.
func (s *Store) RevokeDeviceAtTime(sid e2ecrypto.ServerID, dev e2ecrypto.DevID, seq uint64, at int64) error {
	if err := s.tx(func(tx *sql.Tx) error { return revoke(tx, sid, dev, seq, at) }); err != nil {
		return fmt.Errorf("revoking device %x: %w", dev, err)
	}
	return nil
}

const deviceCols = `d.dev, d.user, d.x25519, d.ed25519, d.cert,
	EXISTS (SELECT 1 FROM revoked_devices r WHERE r.server_id = d.server_id AND r.dev = d.dev),
	(SELECT r.seq FROM revoked_devices r WHERE r.server_id = d.server_id AND r.dev = d.dev),
	(SELECT r.time FROM revoked_devices r WHERE r.server_id = d.server_id AND r.dev = d.dev)`

func scanDevice(sc interface{ Scan(...any) error }) (Device, error) {
	var d Device
	var id, x, ed []byte
	var user int64
	var rseq, rtime sql.NullInt64
	if err := sc.Scan(&id, &user, &x, &ed, &d.Cert, &d.Revoked, &rseq, &rtime); err != nil {
		return Device{}, err
	}
	copy(d.ID[:], id)
	copy(d.X25519[:], x)
	copy(d.Ed25519[:], ed)
	d.User = uint64(user)
	d.RevokedSeq, d.RevokedTime = uint64(rseq.Int64), rtime.Int64
	return d, nil
}

// Device returns a stored device; ok is false if it isn't stored.
func (s *Store) Device(sid e2ecrypto.ServerID, dev e2ecrypto.DevID) (d Device, ok bool, err error) {
	d, err = scanDevice(s.db.QueryRow(`SELECT `+deviceCols+` FROM devices d
		WHERE d.server_id = ? AND d.dev = ?`, sid[:], dev[:]))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, false, nil
	}
	if err != nil {
		return Device{}, false, fmt.Errorf("loading device %x: %w", dev, err)
	}
	return d, true, nil
}

// Devices returns user's stored devices, revoked ones included, ordered by id.
func (s *Store) Devices(sid e2ecrypto.ServerID, user uint64) ([]Device, error) {
	rows, err := s.db.Query(`SELECT `+deviceCols+` FROM devices d
		WHERE d.server_id = ? AND d.user = ? ORDER BY d.dev`, sid[:], i64(user))
	if err != nil {
		return nil, fmt.Errorf("loading devices: %w", err)
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("loading devices: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- epoch chain ----

// ChainHead returns the channel's pinned head; ok is false if none is pinned.
func (s *Store) ChainHead(sid e2ecrypto.ServerID, ch uint64) (h Head, ok bool, err error) {
	var ep int64
	var hash, eh []byte
	err = s.db.QueryRow(`SELECT epoch, hash, eh FROM chain_heads WHERE server_id = ? AND chan = ?`,
		sid[:], i64(ch)).Scan(&ep, &hash, &eh)
	if errors.Is(err, sql.ErrNoRows) {
		return Head{}, false, nil
	}
	if err != nil {
		return Head{}, false, fmt.Errorf("loading chain head: %w", err)
	}
	h.Epoch = uint32(ep)
	copy(h.Hash[:], hash)
	copy(h.EH[:], eh)
	return h, true, nil
}

// AdvanceChain moves the channel's head forward to h, which the caller has
// verified extends the current head. A lower epoch is ErrRollback; a
// different hash at the current epoch is ErrFork; the same head again is a
// no-op. Among concurrent writers offering different heads for one epoch,
// exactly one wins.
func (s *Store) AdvanceChain(sid e2ecrypto.ServerID, ch uint64, h Head) error {
	if h.Epoch == 0 || [e2ecrypto.IDSize]byte(h.EH) != [e2ecrypto.IDSize]byte(h.Hash[:e2ecrypto.IDSize]) {
		return fmt.Errorf("advancing chain: %w: bad head", ErrInvalid)
	}
	err := s.tx(func(tx *sql.Tx) error {
		var ep int64
		var hash []byte
		err := tx.QueryRow(`SELECT epoch, hash FROM chain_heads WHERE server_id = ? AND chan = ?`,
			sid[:], i64(ch)).Scan(&ep, &hash)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case uint32(ep) > h.Epoch:
			return fmt.Errorf("%w: epoch %d below pinned %d", ErrRollback, h.Epoch, ep)
		case uint32(ep) == h.Epoch:
			if subtle.ConstantTimeCompare(hash, h.Hash[:]) != 1 {
				return fmt.Errorf("%w at epoch %d", ErrFork, ep)
			}
			return nil
		}
		_, err = tx.Exec(`INSERT INTO chain_heads (server_id, chan, epoch, hash, eh) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (server_id, chan) DO UPDATE SET epoch = excluded.epoch, hash = excluded.hash, eh = excluded.eh`,
			sid[:], i64(ch), int64(h.Epoch), h.Hash[:], h.EH[:])
		return err
	})
	if err != nil {
		return fmt.Errorf("advancing chain of channel %d: %w", ch, err)
	}
	return nil
}

// Epoch is a verified epoch bundle of a channel: what later checks need
// without the bundle itself (which lives in the cache and can be reset).
type Epoch struct {
	Epoch  uint32
	Hash   [32]byte // e2ecrypto.ChainHash of the bundle's b
	KC     [32]byte
	Roster []uint64
	Back   []byte         // the bundle's backlink (K of the epoch before, under K); nil for epoch 1
	Key    *e2ecrypto.Key // K, once this device has it
}

func packRoster(r []uint64) []byte {
	b := make([]byte, 0, 8*len(r))
	for _, u := range r {
		b = binary.BigEndian.AppendUint64(b, u)
	}
	return b
}

func unpackRoster(b []byte) []uint64 {
	out := make([]uint64, 0, len(b)/8)
	for ; len(b) >= 8; b = b[8:] {
		out = append(out, binary.BigEndian.Uint64(b))
	}
	return out
}

// AdvanceEpoch records a bundle the caller verified as extending the head
// and makes it the head, in one transaction. e.Epoch must be the head's
// epoch + 1 (1 without a head), else ErrGap; a lower epoch is ErrRollback;
// a different bundle for the head's epoch is ErrFork; the same head again
// is a no-op. e.Key, if set, must match e.KC (ErrKeyMismatch).
func (s *Store) AdvanceEpoch(sid e2ecrypto.ServerID, ch uint64, e Epoch) error {
	if e.Epoch == 0 || len(e.Roster) == 0 {
		return fmt.Errorf("advancing epoch: %w: bad epoch", ErrInvalid)
	}
	if e.Key != nil && e2ecrypto.KeyCommitment(*e.Key, sid, ch, e.Epoch) != e.KC {
		return fmt.Errorf("advancing epoch: %w", ErrKeyMismatch)
	}
	err := s.tx(func(tx *sql.Tx) error {
		var ep int64
		var hash []byte
		err := tx.QueryRow(`SELECT epoch, hash FROM chain_heads WHERE server_id = ? AND chan = ?`,
			sid[:], i64(ch)).Scan(&ep, &hash)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			ep = 0
		case err != nil:
			return err
		case uint32(ep) > e.Epoch:
			return fmt.Errorf("%w: epoch %d below pinned %d", ErrRollback, e.Epoch, ep)
		case uint32(ep) == e.Epoch:
			if subtle.ConstantTimeCompare(hash, e.Hash[:]) != 1 {
				return fmt.Errorf("%w at epoch %d", ErrFork, ep)
			}
			return nil
		}
		if e.Epoch != uint32(ep)+1 {
			return fmt.Errorf("%w: epoch %d after head %d", ErrGap, e.Epoch, ep)
		}
		var k []byte
		if e.Key != nil {
			k = e.Key[:]
		}
		if _, err := tx.Exec(`INSERT INTO epochs (server_id, chan, epoch, hash, kc, roster, back, k) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (server_id, chan, epoch) DO UPDATE SET hash = excluded.hash, kc = excluded.kc,
				roster = excluded.roster, back = excluded.back, k = excluded.k`,
			sid[:], i64(ch), int64(e.Epoch), e.Hash[:], e.KC[:], packRoster(e.Roster), e.Back, k); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO chain_heads (server_id, chan, epoch, hash, eh) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (server_id, chan) DO UPDATE SET epoch = excluded.epoch, hash = excluded.hash, eh = excluded.eh`,
			sid[:], i64(ch), int64(e.Epoch), e.Hash[:], e.Hash[:e2ecrypto.IDSize])
		return err
	})
	if err != nil {
		return fmt.Errorf("advancing chain of channel %d: %w", ch, err)
	}
	return nil
}

// Epoch returns a verified epoch of the channel; ok is false if none is recorded.
func (s *Store) Epoch(sid e2ecrypto.ServerID, ch uint64, epoch uint32) (e Epoch, ok bool, err error) {
	var hash, kc, roster, k []byte
	err = s.db.QueryRow(`SELECT hash, kc, roster, back, k FROM epochs WHERE server_id = ? AND chan = ? AND epoch = ?`,
		sid[:], i64(ch), int64(epoch)).Scan(&hash, &kc, &roster, &e.Back, &k)
	if errors.Is(err, sql.ErrNoRows) {
		return Epoch{}, false, nil
	}
	if err != nil {
		return Epoch{}, false, fmt.Errorf("loading epoch: %w", err)
	}
	e.Epoch = epoch
	copy(e.Hash[:], hash)
	copy(e.KC[:], kc)
	e.Roster = unpackRoster(roster)
	if len(k) == e2ecrypto.KeySize {
		e.Key = new(e2ecrypto.Key)
		copy(e.Key[:], k)
	}
	return e, true, nil
}

// SetEpochKey stores K for a verified epoch. It must match the epoch's kc
// (ErrKeyMismatch); an epoch that isn't recorded is ErrUnknownEpoch. Setting
// it again is a no-op.
func (s *Store) SetEpochKey(sid e2ecrypto.ServerID, ch uint64, epoch uint32, k e2ecrypto.Key) error {
	err := s.tx(func(tx *sql.Tx) error {
		var kc []byte
		err := tx.QueryRow(`SELECT kc FROM epochs WHERE server_id = ? AND chan = ? AND epoch = ?`,
			sid[:], i64(ch), int64(epoch)).Scan(&kc)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownEpoch
		}
		if err != nil {
			return err
		}
		want := e2ecrypto.KeyCommitment(k, sid, ch, epoch)
		if subtle.ConstantTimeCompare(kc, want[:]) != 1 {
			return ErrKeyMismatch
		}
		_, err = tx.Exec(`UPDATE epochs SET k = ? WHERE server_id = ? AND chan = ? AND epoch = ?`,
			k[:], sid[:], i64(ch), int64(epoch))
		return err
	})
	if err != nil {
		return fmt.Errorf("storing key of channel %d epoch %d: %w", ch, epoch, err)
	}
	return nil
}

// MissingKeys returns the channel's verified epochs this device has no K
// for, newest first.
func (s *Store) MissingKeys(sid e2ecrypto.ServerID, ch uint64) ([]uint32, error) {
	rows, err := s.db.Query(`SELECT epoch FROM epochs WHERE server_id = ? AND chan = ? AND k IS NULL
		ORDER BY epoch DESC`, sid[:], i64(ch))
	if err != nil {
		return nil, fmt.Errorf("loading epochs: %w", err)
	}
	defer rows.Close()
	var out []uint32
	for rows.Next() {
		var e int64
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, uint32(e))
	}
	return out, rows.Err()
}

// ---- alarms ----

// RaiseAlarm records that the server presented something about the channel's
// chain that an honest server can't (a fork, rollback, forged bundle). The
// first detail is kept. There is deliberately no way to clear an alarm
// yet: like the sticky flags, nothing the sync code calls may undo it (the
// TUI's acknowledge flow, step 10, will add one).
func (s *Store) RaiseAlarm(sid e2ecrypto.ServerID, ch uint64, detail string) error {
	err := s.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT OR IGNORE INTO chan_alarms (server_id, chan, detail, time) VALUES (?, ?, ?, ?)`,
			sid[:], i64(ch), detail, time.Now().UnixNano())
		return err
	})
	if err != nil {
		return fmt.Errorf("raising alarm for channel %d: %w", ch, err)
	}
	return nil
}

// Alarm returns the channel's alarm detail; ok is false if there is none.
func (s *Store) Alarm(sid e2ecrypto.ServerID, ch uint64) (detail string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT detail FROM chan_alarms WHERE server_id = ? AND chan = ?`, sid[:], i64(ch)).Scan(&detail)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("loading alarm: %w", err)
	}
	return detail, true, nil
}

// ---- sticky flags ----
//
// There is deliberately no way to clear these: once a server or channel has
// been seen encrypted, a server claiming otherwise is an attack.

// MarkServerE2E records that the server speaks e2e.
func (s *Store) MarkServerE2E(sid e2ecrypto.ServerID) error {
	if err := s.tx(func(tx *sql.Tx) error { return markServer(tx, sid) }); err != nil {
		return fmt.Errorf("marking server e2e: %w", err)
	}
	return nil
}

func markServer(tx *sql.Tx, sid e2ecrypto.ServerID) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO e2e_servers (server_id) VALUES (?)`, sid[:])
	return err
}

// ServerUsesE2E reports whether MarkServerE2E (or MarkChannelE2E) was ever called for the server.
func (s *Store) ServerUsesE2E(sid e2ecrypto.ServerID) (bool, error) {
	var ok bool
	err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM e2e_servers WHERE server_id = ?)`, sid[:]).Scan(&ok)
	return ok, err
}

// MarkChannelE2E records that the channel is encrypted; it also marks the server.
func (s *Store) MarkChannelE2E(sid e2ecrypto.ServerID, ch uint64) error {
	err := s.tx(func(tx *sql.Tx) error {
		if err := markServer(tx, sid); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT OR IGNORE INTO e2e_chans (server_id, chan) VALUES (?, ?)`, sid[:], i64(ch))
		return err
	})
	if err != nil {
		return fmt.Errorf("marking channel %d e2e: %w", ch, err)
	}
	return nil
}

// ChannelE2E reports whether the channel was ever marked encrypted.
func (s *Store) ChannelE2E(sid e2ecrypto.ServerID, ch uint64) (bool, error) {
	var ok bool
	err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM e2e_chans WHERE server_id = ? AND chan = ?)`,
		sid[:], i64(ch)).Scan(&ok)
	return ok, err
}

// ---- server identity ----
//
// Every other row is keyed by server_id, which the server itself announces
// in WELCOME. A server that changed its id would make the client start over
// with empty trust (fresh TOFU), so the first id seen at a config's address
// and TLS pin is pinned, and only an explicit login re-trusts.

// ServerIdentity returns the server_id pinned for (addr, pin); ok is false if none is.
func (s *Store) ServerIdentity(addr, pin string) (sid e2ecrypto.ServerID, ok bool, err error) {
	var b []byte
	err = s.db.QueryRow(`SELECT server_id FROM server_pins WHERE addr = ? AND pin = ?`, addr, pin).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return sid, false, nil
	}
	if err != nil {
		return sid, false, fmt.Errorf("loading server identity: %w", err)
	}
	copy(sid[:], b)
	return sid, len(b) == e2ecrypto.IDSize, nil
}

// PinServer records sid as the identity of the server at (addr, pin) unless
// one is pinned already; a different pinned id is ErrServerChanged (and is
// kept: nothing here ever resets trust).
func (s *Store) PinServer(addr, pin string, sid e2ecrypto.ServerID) error {
	err := s.tx(func(tx *sql.Tx) error {
		var b []byte
		err := tx.QueryRow(`SELECT server_id FROM server_pins WHERE addr = ? AND pin = ?`, addr, pin).Scan(&b)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.Exec(`INSERT INTO server_pins (addr, pin, server_id, first_seen) VALUES (?, ?, ?, ?)`,
				addr, pin, sid[:], time.Now().UnixNano())
			return err
		case err != nil:
			return err
		case subtle.ConstantTimeCompare(b, sid[:]) != 1:
			return ErrServerChanged
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("pinning server identity: %w", err)
	}
	return nil
}

// RepinServer makes sid the identity of the server at (addr, pin), replacing
// any earlier one. Only an explicit `tc login` calls it.
func (s *Store) RepinServer(addr, pin string, sid e2ecrypto.ServerID) error {
	err := s.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO server_pins (addr, pin, server_id, first_seen) VALUES (?, ?, ?, ?)
			ON CONFLICT (addr, pin) DO UPDATE SET server_id = excluded.server_id, first_seen = excluded.first_seen`,
			addr, pin, sid[:], time.Now().UnixNano())
		return err
	})
	if err != nil {
		return fmt.Errorf("re-pinning server identity: %w", err)
	}
	return nil
}

// RaiseServerAlarm records that the server did something an honest one
// can't (e.g. dropped e2e1 after speaking it). The first detail is kept;
// like channel alarms it can't be cleared yet (step 10).
func (s *Store) RaiseServerAlarm(sid e2ecrypto.ServerID, detail string) error {
	err := s.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT OR IGNORE INTO server_alarms (server_id, detail, time) VALUES (?, ?, ?)`,
			sid[:], detail, time.Now().UnixNano())
		return err
	})
	if err != nil {
		return fmt.Errorf("raising server alarm: %w", err)
	}
	return nil
}

// ServerAlarm returns the server's alarm detail; ok is false if there is none.
func (s *Store) ServerAlarm(sid e2ecrypto.ServerID) (detail string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT detail FROM server_alarms WHERE server_id = ?`, sid[:]).Scan(&detail)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("loading server alarm: %w", err)
	}
	return detail, true, nil
}
