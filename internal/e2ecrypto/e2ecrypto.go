// Package e2ecrypto implements the cryptographic primitives of tc's end-to-end
// encryption (docs/E2E.md, "Review r1 → r2", PROTOCOL §12.3/§12.4).
//
// It is pure: no I/O beyond the caller-supplied randomness, no global state and
// no imports of other tc packages. Higher layers decide which keys to trust; this
// package only seals, opens, signs and verifies exact bytes.
package e2ecrypto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

// Limits from §12.8.
const (
	MaxEncSize        = 64<<10 + 1<<10 // enc ≤ 64 KiB + 1 KiB
	MaxDeviceCertSize = 4 << 10
	MaxDevicesPerUser = 8
	MaxBundleSize     = 16 << 10
	MaxBundleBoxes    = 128
)

// Sizes of fixed binary fields.
const (
	KeySize       = 32
	IDSize        = 16 // server_id, device id, eh, mid
	NonceSize     = 24 // XChaCha20-Poly1305
	TagSize       = 16
	SigSize       = ed25519.SignatureSize
	BoxCTSize     = KeySize + TagSize             // 48
	BackSize      = NonceSize + KeySize + TagSize // 72
	PadBlock      = 256
	EncVersion    = 0x01
	EncHeaderSize = 1 + 4 + IDSize + IDSize + NonceSize // 61
	// MinEncSize is an empty plaintext: one padding block.
	MinEncSize = EncHeaderSize + PadBlock + TagSize + SigSize
	// MaxPlainSize is the largest plaintext whose enc fits in MaxEncSize
	// (pad256 always adds at least the 0x80 byte).
	MaxPlainSize = (MaxEncSize-EncHeaderSize-TagSize-SigSize)/PadBlock*PadBlock - 1
)

// Ed25519 signing contexts. Each signed message is context || b.
const (
	CtxEpoch     = "tc-epoch-v1\x00"
	CtxMsgSig    = "tc-sig-v1\x00"
	CtxRevokeDev = "tc-revoke-dev-v1\x00"
)

var (
	ErrMalformed    = errors.New("e2ecrypto: malformed input")
	ErrVersion      = errors.New("e2ecrypto: unsupported version")
	ErrTooLarge     = errors.New("e2ecrypto: input too large")
	ErrBadSignature = errors.New("e2ecrypto: bad signature")
	ErrDecrypt      = errors.New("e2ecrypto: decryption failed")
	ErrBadPadding   = errors.New("e2ecrypto: bad padding")
	ErrLowOrder     = errors.New("e2ecrypto: low-order public key")
	ErrBadKey       = errors.New("e2ecrypto: invalid key")
)

type (
	// Key is a 32-byte symmetric epoch key K.
	Key [KeySize]byte
	// ServerID identifies the server; bound into every AD so ciphertext can't move between servers.
	ServerID [IDSize]byte
	// DevID is a 16-byte device id.
	DevID [IDSize]byte
	// EpochHash is eh = H(bundle.b)[:16].
	EpochHash [IDSize]byte
	// MsgID is mid = H(enc)[:16].
	MsgID [IDSize]byte
)

// scope is server_id(16) || chan(u64 BE) || epoch(u32 BE).
func scope(sid ServerID, ch uint64, ep uint32) []byte {
	b := make([]byte, 0, IDSize+8+4)
	b = append(b, sid[:]...)
	b = binary.BigEndian.AppendUint64(b, ch)
	return binary.BigEndian.AppendUint32(b, ep)
}

// MID is the message id of an enc: sha256(enc)[:16].
func MID(enc []byte) MsgID {
	h := sha256.Sum256(enc)
	return MsgID(h[:IDSize])
}

// EH is the epoch hash of a bundle's exact b bytes: sha256(b)[:16].
func EH(bundle []byte) EpochHash {
	h := sha256.Sum256(bundle)
	return EpochHash(h[:IDSize])
}

// ChainHash is the full hash of a bundle's b bytes, used as the next bundle's prev.
func ChainHash(bundle []byte) [32]byte { return sha256.Sum256(bundle) }

func withContext(context string, b []byte) []byte {
	m := make([]byte, 0, len(context)+len(b))
	return append(append(m, context...), b...)
}

// SignEd25519 signs context || b.
func SignEd25519(priv ed25519.PrivateKey, context string, b []byte) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, ErrBadKey
	}
	return ed25519.Sign(priv, withContext(context, b)), nil
}

// VerifyEd25519 reports whether sig is a valid signature by pub over context || b.
func VerifyEd25519(pub ed25519.PublicKey, context string, b, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != SigSize {
		return false
	}
	return ed25519.Verify(pub, withContext(context, b), sig)
}
