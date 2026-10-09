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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite"

	"github.com/skailama/tc/internal/e2ecrypto"
)

// FileName is the trust store's file name inside TC_HOME.
const FileName = "e2e.db"

const schemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS device_keys (
	server_id BLOB PRIMARY KEY, keys BLOB NOT NULL, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS pinned_keys (
	server_id BLOB NOT NULL, user INTEGER NOT NULL, fp TEXT NOT NULL, wire BLOB NOT NULL,
	how TEXT NOT NULL, first_seen INTEGER NOT NULL, PRIMARY KEY (server_id, user, fp));
CREATE TABLE IF NOT EXISTS key_changes (
	server_id BLOB NOT NULL, user INTEGER NOT NULL, fp TEXT NOT NULL, wire BLOB NOT NULL,
	how TEXT NOT NULL, first_seen INTEGER NOT NULL, PRIMARY KEY (server_id, user, fp));
CREATE TABLE IF NOT EXISTS devices (
	server_id BLOB NOT NULL, dev BLOB NOT NULL, user INTEGER NOT NULL, x25519 BLOB NOT NULL,
	ed25519 BLOB NOT NULL, cert BLOB NOT NULL, PRIMARY KEY (server_id, dev));
CREATE TABLE IF NOT EXISTS revoked_devices (
	server_id BLOB NOT NULL, dev BLOB NOT NULL, PRIMARY KEY (server_id, dev));
CREATE TABLE IF NOT EXISTS chain_heads (
	server_id BLOB NOT NULL, chan INTEGER NOT NULL, epoch INTEGER NOT NULL, hash BLOB NOT NULL,
	eh BLOB NOT NULL, PRIMARY KEY (server_id, chan));
CREATE TABLE IF NOT EXISTS e2e_servers (server_id BLOB PRIMARY KEY);
CREATE TABLE IF NOT EXISTS e2e_chans (
	server_id BLOB NOT NULL, chan INTEGER NOT NULL, PRIMARY KEY (server_id, chan));
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
	if err := s.tx(initSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating trust store schema: %w", err)
	}
	return s, nil
}

func initSchema(tx *sql.Tx) error {
	var v int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > schemaVersion {
		return fmt.Errorf("trust store schema v%d is newer than this tc (v%d)", v, schemaVersion)
	}
	if _, err := tx.Exec(schema); err != nil {
		return err
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

// DeviceKeys returns this device's keys for the server, generating and
// storing them with crypto/rand on first use. Concurrent first calls from
// several processes all get the same keys. This is the only place the
// private keys are persisted.
func (s *Store) DeviceKeys(sid e2ecrypto.ServerID) (*e2ecrypto.DeviceKeys, error) {
	var raw []byte
	err := s.tx(func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT keys FROM device_keys WHERE server_id = ?`, sid[:]).Scan(&raw)
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		k, err := e2ecrypto.Generate(rand.Reader)
		if err != nil {
			return err
		}
		if raw, err = k.MarshalBinary(); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO device_keys (server_id, keys, created) VALUES (?, ?, ?)`,
			sid[:], raw, time.Now().UnixNano())
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
			_, err = tx.Exec(`INSERT INTO key_changes (server_id, user, fp, wire, how, first_seen)
				VALUES (?, ?, ?, ?, ?, ?)`, sid[:], i64(user), fp, wire, how, now)
		case err == nil && how.rank() > pend.rank():
			_, err = tx.Exec(`UPDATE key_changes SET how = ? WHERE server_id = ? AND user = ? AND fp = ?`,
				how, sid[:], i64(user), fp)
		}
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
			return revoke(tx, sid, d.ID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("putting device %x: %w", d.ID, err)
	}
	return nil
}

func revoke(tx *sql.Tx, sid e2ecrypto.ServerID, dev e2ecrypto.DevID) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO revoked_devices (server_id, dev) VALUES (?, ?)`, sid[:], dev[:])
	return err
}

// RevokeDevice marks a device revoked, permanently. The device need not be
// stored yet: a revocation seen before the cert still applies to it.
func (s *Store) RevokeDevice(sid e2ecrypto.ServerID, dev e2ecrypto.DevID) error {
	if err := s.tx(func(tx *sql.Tx) error { return revoke(tx, sid, dev) }); err != nil {
		return fmt.Errorf("revoking device %x: %w", dev, err)
	}
	return nil
}

const deviceCols = `d.dev, d.user, d.x25519, d.ed25519, d.cert,
	EXISTS (SELECT 1 FROM revoked_devices r WHERE r.server_id = d.server_id AND r.dev = d.dev)`

func scanDevice(sc interface{ Scan(...any) error }) (Device, error) {
	var d Device
	var id, x, ed []byte
	var user int64
	if err := sc.Scan(&id, &user, &x, &ed, &d.Cert, &d.Revoked); err != nil {
		return Device{}, err
	}
	copy(d.ID[:], id)
	copy(d.X25519[:], x)
	copy(d.Ed25519[:], ed)
	d.User = uint64(user)
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
