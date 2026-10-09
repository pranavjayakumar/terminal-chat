package identity

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// CtxRevokeDev is the Ed25519 context for a revoke signed by another device of the user.
const CtxRevokeDev = e2ecrypto.CtxRevokeDev

const signedVersion = 1

// MaxSignedSize bounds len(b)+len(sig) of every object here (§12.8 DeviceCert ≤ 4 KiB).
const MaxSignedSize = e2ecrypto.MaxDeviceCertSize

var (
	// ErrWrongServer means the object was signed for another server.
	ErrWrongServer = errors.New("identity: object is for another server")
	// ErrWrongKey means the cert's embedded SSH key is not the verifying key.
	ErrWrongKey = errors.New("identity: embedded SSH key differs from verifying key")
)

// Signed is a signed object on the wire: sig over context || b (§12.1).
// B is kept verbatim; it is never re-encoded before verification.
type Signed struct {
	B   []byte `msgpack:"b"`
	Sig []byte `msgpack:"sig"`
}

// DeviceCert binds a device's E2E keys to a user's SSH key. Created is Unix ms.
type DeviceCert struct {
	ServerID e2ecrypto.ServerID
	User     uint64
	Dev      e2ecrypto.DevID
	X25519   [32]byte
	Ed25519  ed25519.PublicKey
	SSH      []byte // ssh wire-encoded public key that signed the cert
	Created  int64
}

// KeyEndorse is an already-endorsed SSH key vouching for NewSSH. Time is Unix ms.
type KeyEndorse struct {
	ServerID e2ecrypto.ServerID
	User     uint64
	NewSSH   []byte // ssh wire-encoded
	Time     int64
}

// DeviceRevoke revokes one device of User. Time is Unix ms.
type DeviceRevoke struct {
	ServerID e2ecrypto.ServerID
	User     uint64
	Dev      e2ecrypto.DevID
	Time     int64
}

// Wire forms of b. Field order is the encoding order and is part of the format.
type deviceCertB struct {
	V        uint32 `msgpack:"v"`
	ServerID []byte `msgpack:"server_id"`
	User     uint64 `msgpack:"user"`
	Dev      []byte `msgpack:"dev"`
	X25519   []byte `msgpack:"x25519"`
	Ed25519  []byte `msgpack:"ed25519"`
	SSH      []byte `msgpack:"ssh"`
	Created  int64  `msgpack:"created"`
}

type keyEndorseB struct {
	V        uint32 `msgpack:"v"`
	ServerID []byte `msgpack:"server_id"`
	User     uint64 `msgpack:"user"`
	NewSSH   []byte `msgpack:"new_ssh"`
	Time     int64  `msgpack:"time"`
}

type deviceRevokeB struct {
	V        uint32 `msgpack:"v"`
	ServerID []byte `msgpack:"server_id"`
	User     uint64 `msgpack:"user"`
	Dev      []byte `msgpack:"dev"`
	Time     int64  `msgpack:"time"`
}

// NewDeviceCert signs dev's public keys with the user's SSH key.
func NewDeviceCert(signer ssh.Signer, serverID e2ecrypto.ServerID, user uint64, dev *e2ecrypto.DeviceKeys, now time.Time) (Signed, error) {
	if dev == nil || len(dev.Ed25519Pub) != ed25519.PublicKeySize {
		return Signed{}, e2ecrypto.ErrBadKey
	}
	return sign(signer, CtxDevice, deviceCertB{
		V: signedVersion, ServerID: serverID[:], User: user, Dev: dev.ID[:],
		X25519: dev.X25519Pub[:], Ed25519: dev.Ed25519Pub,
		SSH: signer.PublicKey().Marshal(), Created: now.UnixMilli(),
	})
}

// VerifyDeviceCert checks s against sshPub (wire-encoded) and serverID, then
// decodes it. The caller still decides whether sshPub is trusted for c.User.
func (s Signed) VerifyDeviceCert(serverID e2ecrypto.ServerID, sshPub []byte) (DeviceCert, error) {
	if err := s.verifySSH(sshPub, CtxDevice); err != nil {
		return DeviceCert{}, err
	}
	c, err := decodeDeviceCert(s.B)
	if err != nil {
		return DeviceCert{}, err
	}
	if c.ServerID != serverID {
		return DeviceCert{}, ErrWrongServer
	}
	if !bytes.Equal(c.SSH, sshPub) {
		return DeviceCert{}, ErrWrongKey
	}
	return c, nil
}

func decodeDeviceCert(b []byte) (DeviceCert, error) {
	var w deviceCertB
	if err := decodeB(b, &w, &w.V); err != nil {
		return DeviceCert{}, err
	}
	c := DeviceCert{User: w.User, SSH: w.SSH, Created: w.Created}
	if !copyFixed(c.ServerID[:], w.ServerID) || !copyFixed(c.Dev[:], w.Dev) ||
		!copyFixed(c.X25519[:], w.X25519) || len(w.Ed25519) != ed25519.PublicKeySize {
		return DeviceCert{}, e2ecrypto.ErrMalformed
	}
	c.Ed25519 = ed25519.PublicKey(w.Ed25519)
	return c, nil
}

// NewKeyEndorse signs, with an already-endorsed key, a statement that newKey
// also belongs to user.
func NewKeyEndorse(signer ssh.Signer, serverID e2ecrypto.ServerID, user uint64, newKey ssh.PublicKey, now time.Time) (Signed, error) {
	if !allowedKey[newKey.Type()] {
		return Signed{}, fmt.Errorf("key type %s not accepted", newKey.Type())
	}
	return sign(signer, CtxEndorse, keyEndorseB{
		V: signedVersion, ServerID: serverID[:], User: user, NewSSH: newKey.Marshal(), Time: now.UnixMilli(),
	})
}

// VerifyKeyEndorse checks s as signed by endorserPub (wire-encoded). The caller
// must already trust endorserPub for e.User.
func (s Signed) VerifyKeyEndorse(serverID e2ecrypto.ServerID, endorserPub []byte) (KeyEndorse, error) {
	if err := s.verifySSH(endorserPub, CtxEndorse); err != nil {
		return KeyEndorse{}, err
	}
	e, err := decodeKeyEndorse(s.B)
	if err != nil {
		return KeyEndorse{}, err
	}
	if e.ServerID != serverID {
		return KeyEndorse{}, ErrWrongServer
	}
	return e, nil
}

// NewDeviceRevoke revokes dev, signed by an endorsed SSH key of user.
func NewDeviceRevoke(signer ssh.Signer, serverID e2ecrypto.ServerID, user uint64, dev e2ecrypto.DevID, now time.Time) (Signed, error) {
	return sign(signer, CtxRevoke, revokeB(serverID, user, dev, now))
}

// NewDeviceRevokeByDevice revokes dev, signed by another device (by) of user.
func NewDeviceRevokeByDevice(by *e2ecrypto.DeviceKeys, serverID e2ecrypto.ServerID, user uint64, dev e2ecrypto.DevID, now time.Time) (Signed, error) {
	if by == nil {
		return Signed{}, e2ecrypto.ErrBadKey
	}
	b, err := proto.Marshal(revokeB(serverID, user, dev, now))
	if err != nil {
		return Signed{}, fmt.Errorf("encoding revoke: %w", err)
	}
	sig, err := e2ecrypto.SignEd25519(by.Ed25519, CtxRevokeDev, b)
	if err != nil {
		return Signed{}, err
	}
	return Signed{B: b, Sig: sig}, nil
}

// VerifyDeviceRevoke checks s as signed by sshPub (wire-encoded, an endorsed
// key of r.User per the caller's trust store).
func (s Signed) VerifyDeviceRevoke(serverID e2ecrypto.ServerID, sshPub []byte) (DeviceRevoke, error) {
	if err := s.verifySSH(sshPub, CtxRevoke); err != nil {
		return DeviceRevoke{}, err
	}
	return decodeRevoke(s.B, serverID)
}

// VerifyDeviceRevokeByDevice checks s as signed by the Ed25519 key of a device.
// The caller must check that device is a different, non-revoked device of r.User.
func (s Signed) VerifyDeviceRevokeByDevice(serverID e2ecrypto.ServerID, by ed25519.PublicKey) (DeviceRevoke, error) {
	if len(s.B)+len(s.Sig) > MaxSignedSize {
		return DeviceRevoke{}, e2ecrypto.ErrTooLarge
	}
	if !e2ecrypto.VerifyEd25519(by, CtxRevokeDev, s.B, s.Sig) {
		return DeviceRevoke{}, e2ecrypto.ErrBadSignature
	}
	return decodeRevoke(s.B, serverID)
}

func revokeB(serverID e2ecrypto.ServerID, user uint64, dev e2ecrypto.DevID, now time.Time) deviceRevokeB {
	return deviceRevokeB{V: signedVersion, ServerID: serverID[:], User: user, Dev: dev[:], Time: now.UnixMilli()}
}

func decodeKeyEndorse(b []byte) (KeyEndorse, error) {
	var w keyEndorseB
	if err := decodeB(b, &w, &w.V); err != nil {
		return KeyEndorse{}, err
	}
	e := KeyEndorse{User: w.User, NewSSH: w.NewSSH, Time: w.Time}
	if !copyFixed(e.ServerID[:], w.ServerID) {
		return KeyEndorse{}, e2ecrypto.ErrMalformed
	}
	if pk, err := ssh.ParsePublicKey(w.NewSSH); err != nil || !allowedKey[pk.Type()] {
		return KeyEndorse{}, e2ecrypto.ErrMalformed
	}
	return e, nil
}

func decodeRevokeB(b []byte) (DeviceRevoke, error) {
	var w deviceRevokeB
	if err := decodeB(b, &w, &w.V); err != nil {
		return DeviceRevoke{}, err
	}
	r := DeviceRevoke{User: w.User, Time: w.Time}
	if !copyFixed(r.ServerID[:], w.ServerID) || !copyFixed(r.Dev[:], w.Dev) {
		return DeviceRevoke{}, e2ecrypto.ErrMalformed
	}
	return r, nil
}

func decodeRevoke(b []byte, serverID e2ecrypto.ServerID) (DeviceRevoke, error) {
	r, err := decodeRevokeB(b)
	if err != nil {
		return DeviceRevoke{}, err
	}
	if r.ServerID != serverID {
		return DeviceRevoke{}, ErrWrongServer
	}
	return r, nil
}

// The Peek functions decode b WITHOUT checking the signature, the server id or
// who signed it. They exist to index stored objects (by user, device) and to
// pick the right error for an object that failed verification. Nothing they
// return may be trusted.

// PeekDeviceCert decodes an unverified device cert.
func PeekDeviceCert(s Signed) (DeviceCert, error) {
	if len(s.B)+len(s.Sig) > MaxSignedSize {
		return DeviceCert{}, e2ecrypto.ErrTooLarge
	}
	return decodeDeviceCert(s.B)
}

// PeekKeyEndorse decodes an unverified key endorsement.
func PeekKeyEndorse(s Signed) (KeyEndorse, error) {
	if len(s.B)+len(s.Sig) > MaxSignedSize {
		return KeyEndorse{}, e2ecrypto.ErrTooLarge
	}
	return decodeKeyEndorse(s.B)
}

// PeekDeviceRevoke decodes an unverified device revoke.
func PeekDeviceRevoke(s Signed) (DeviceRevoke, error) {
	if len(s.B)+len(s.Sig) > MaxSignedSize {
		return DeviceRevoke{}, e2ecrypto.ErrTooLarge
	}
	return decodeRevokeB(s.B)
}

func sign(signer ssh.Signer, context string, w any) (Signed, error) {
	b, err := proto.Marshal(w)
	if err != nil {
		return Signed{}, fmt.Errorf("encoding signed object: %w", err)
	}
	sig, err := SignContext(signer, context, b)
	if err != nil {
		return Signed{}, err
	}
	if len(b)+len(sig) > MaxSignedSize {
		return Signed{}, e2ecrypto.ErrTooLarge
	}
	return Signed{B: b, Sig: sig}, nil
}

// verifySSH runs the size check and signature check, both before b is decoded.
func (s Signed) verifySSH(pub []byte, context string) error {
	if len(s.B)+len(s.Sig) > MaxSignedSize {
		return e2ecrypto.ErrTooLarge
	}
	if _, err := VerifyContext(pub, s.Sig, context, s.B); err != nil {
		return fmt.Errorf("%w: %v", e2ecrypto.ErrBadSignature, err)
	}
	return nil
}

// decodeB decodes b into w and requires b to be exactly the canonical
// encoding of the result. One signature then has exactly one meaning: no
// duplicate or unknown keys, alternative int widths or trailing bytes that
// another decoder might read differently.
func decodeB(b []byte, w any, v *uint32) error {
	if err := proto.Unmarshal(b, w); err != nil {
		return fmt.Errorf("%w: %v", e2ecrypto.ErrMalformed, err)
	}
	if *v != signedVersion {
		return e2ecrypto.ErrVersion
	}
	re, err := proto.Marshal(w)
	if err != nil || !bytes.Equal(re, b) {
		return e2ecrypto.ErrMalformed
	}
	return nil
}

func copyFixed(dst, src []byte) bool {
	if len(src) != len(dst) {
		return false
	}
	copy(dst, src)
	return true
}
