package e2ecrypto

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"fmt"
	"io"
)

const (
	deviceKeysVersion = 1
	// DeviceKeysSize is the MarshalBinary length: version || id || x25519 priv || ed25519 seed.
	DeviceKeysSize = 1 + IDSize + 32 + ed25519.SeedSize
)

// DeviceKeys is one device's long-term secret material.
type DeviceKeys struct {
	ID         DevID
	X25519     *ecdh.PrivateKey
	X25519Pub  [32]byte
	Ed25519    ed25519.PrivateKey
	Ed25519Pub ed25519.PublicKey
}

// Generate creates device keys from rand. Keys are derived from bytes read from
// rand (not via GenerateKey) so a deterministic reader gives reproducible keys.
func Generate(rand io.Reader) (*DeviceKeys, error) {
	var raw [DeviceKeysSize - 1]byte
	if _, err := io.ReadFull(rand, raw[:]); err != nil {
		return nil, fmt.Errorf("generating device keys: %w", err)
	}
	return fromRaw(raw[:])
}

func fromRaw(raw []byte) (*DeviceKeys, error) {
	k := &DeviceKeys{}
	copy(k.ID[:], raw[:IDSize])
	x, err := ecdh.X25519().NewPrivateKey(raw[IDSize : IDSize+32])
	if err != nil {
		return nil, fmt.Errorf("x25519 key: %w", err)
	}
	k.X25519 = x
	copy(k.X25519Pub[:], x.PublicKey().Bytes())
	k.Ed25519 = ed25519.NewKeyFromSeed(raw[IDSize+32:])
	k.Ed25519Pub = k.Ed25519.Public().(ed25519.PublicKey)
	return k, nil
}

// MarshalBinary returns the versioned secret encoding. The output is secret.
func (k *DeviceKeys) MarshalBinary() ([]byte, error) {
	if k == nil || k.X25519 == nil || len(k.Ed25519) != ed25519.PrivateKeySize {
		return nil, ErrBadKey
	}
	b := make([]byte, 0, DeviceKeysSize)
	b = append(b, deviceKeysVersion)
	b = append(b, k.ID[:]...)
	b = append(b, k.X25519.Bytes()...)
	return append(b, k.Ed25519.Seed()...), nil
}

// UnmarshalBinary parses MarshalBinary output; public keys are re-derived, never trusted from input.
func (k *DeviceKeys) UnmarshalBinary(b []byte) error {
	if len(b) != DeviceKeysSize {
		return ErrMalformed
	}
	if b[0] != deviceKeysVersion {
		return ErrVersion
	}
	nk, err := fromRaw(b[1:])
	if err != nil {
		return err
	}
	*k = *nk
	return nil
}
