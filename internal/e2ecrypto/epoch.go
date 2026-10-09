package e2ecrypto

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// NewEpochKey returns a fresh random epoch key K.
func NewEpochKey(rand io.Reader) (Key, error) {
	var k Key
	if _, err := io.ReadFull(rand, k[:]); err != nil {
		return Key{}, fmt.Errorf("generating epoch key: %w", err)
	}
	return k, nil
}

// KeyCommitment is kc = HMAC-SHA256(K, "tc-kc-v1" || server_id || chan || epoch).
// Every recipient checks it so a rotator can't hand different members different keys.
func KeyCommitment(k Key, sid ServerID, ch uint64, ep uint32) [32]byte {
	m := hmac.New(sha256.New, k[:])
	m.Write([]byte("tc-kc-v1"))
	m.Write(scope(sid, ch, ep))
	var kc [32]byte
	m.Sum(kc[:0])
	return kc
}

// probeScalar is any fixed X25519 private key. Clamping makes it a multiple
// of the cofactor, so its shared secret with every low-order point (in any
// encoding) is zero.
var probeScalar = func() *ecdh.PrivateKey {
	k, err := ecdh.X25519().NewPrivateKey(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		panic(err)
	}
	return k
}()

// ValidX25519 reports whether pub is usable as a box recipient: not a
// low-order point, whose shared secret with any ephemeral key is zero.
// Such a device could never be sealed to, so rotations including it fail.
func ValidX25519(pub [32]byte) bool {
	pk, err := ecdh.X25519().NewPublicKey(pub[:])
	if err != nil {
		return false
	}
	ss, err := probeScalar.ECDH(pk)
	return err == nil && subtle.ConstantTimeCompare(ss, make([]byte, len(ss))) == 0
}

// NewEphemeral returns a bundle's ephemeral X25519 key, shared by all of its boxes.
func NewEphemeral(rand io.Reader) (*ecdh.PrivateKey, error) {
	var b [32]byte
	if _, err := io.ReadFull(rand, b[:]); err != nil {
		return nil, fmt.Errorf("generating ephemeral key: %w", err)
	}
	return ecdh.X25519().NewPrivateKey(b[:])
}

// BoxAD are the fields authenticated by a key box.
type BoxAD struct {
	ServerID  ServerID
	Chan      uint64
	Epoch     uint32
	Recipient DevID // d.dev
	By        DevID // rotator device
}

func (a BoxAD) bytes() []byte {
	b := scope(a.ServerID, a.Chan, a.Epoch)
	b = append(b, a.Recipient[:]...)
	return append(b, a.By[:]...)
}

// boxKey is bk = HKDF-SHA256(ss, salt = eph_pub || recipient_pub, info "tc-box-v1").
func boxKey(priv *ecdh.PrivateKey, peer, ephPub, recipientPub [32]byte, info string) ([]byte, error) {
	pk, err := ecdh.X25519().NewPublicKey(peer[:])
	if err != nil {
		return nil, ErrBadKey
	}
	ss, err := priv.ECDH(pk)
	if err != nil {
		return nil, ErrLowOrder
	}
	// crypto/ecdh already rejects an all-zero output; checked again so the
	// guarantee doesn't depend on library internals.
	if subtle.ConstantTimeCompare(ss, make([]byte, len(ss))) == 1 {
		return nil, ErrLowOrder
	}
	salt := make([]byte, 0, 64)
	salt = append(append(salt, ephPub[:]...), recipientPub[:]...)
	return hkdf.Key(sha256.New, ss, salt, info, KeySize)
}

var zeroNonce [NonceSize]byte

// SealKey seals k to one recipient device. The zero nonce is safe because bk is
// unique per (eph, recipient) pair and each pair seals exactly one K.
func SealKey(eph *ecdh.PrivateKey, recipientPub [32]byte, ad BoxAD, k Key) ([BoxCTSize]byte, error) {
	var out [BoxCTSize]byte
	if eph == nil {
		return out, ErrBadKey
	}
	ephPub := [32]byte(eph.PublicKey().Bytes())
	bk, err := boxKey(eph, recipientPub, ephPub, recipientPub, "tc-box-v1")
	if err != nil {
		return out, err
	}
	aead, err := chacha20poly1305.NewX(bk)
	if err != nil {
		return out, err
	}
	aead.Seal(out[:0], zeroNonce[:], k[:], ad.bytes())
	return out, nil
}

// OpenKey opens a box sealed to recipient's X25519 key under the bundle's ephPub.
func OpenKey(recipient *ecdh.PrivateKey, ephPub [32]byte, ad BoxAD, ct []byte) (Key, error) {
	if recipient == nil {
		return Key{}, ErrBadKey
	}
	if len(ct) != BoxCTSize {
		return Key{}, ErrMalformed
	}
	recipientPub := [32]byte(recipient.PublicKey().Bytes())
	bk, err := boxKey(recipient, ephPub, ephPub, recipientPub, "tc-box-v1")
	if err != nil {
		return Key{}, err
	}
	aead, err := chacha20poly1305.NewX(bk)
	if err != nil {
		return Key{}, err
	}
	var k Key
	if _, err := aead.Open(k[:0], zeroNonce[:], ct, ad.bytes()); err != nil {
		return Key{}, ErrDecrypt
	}
	return k, nil
}

func backKey(kNew Key) ([]byte, error) {
	return hkdf.Key(sha256.New, kNew[:], nil, "tc-back-v1", KeySize)
}

// SealBack is back = nonce || XChaCha20-Poly1305(HKDF(K_new, "tc-back-v1"), nonce, K_prev,
// AD = server_id || chan || epoch), letting holders of K_new walk back through history.
func SealBack(rand io.Reader, kNew, kPrev Key, sid ServerID, ch uint64, ep uint32) ([BackSize]byte, error) {
	var out [BackSize]byte
	if _, err := io.ReadFull(rand, out[:NonceSize]); err != nil {
		return out, fmt.Errorf("generating nonce: %w", err)
	}
	bk, err := backKey(kNew)
	if err != nil {
		return out, err
	}
	aead, err := chacha20poly1305.NewX(bk)
	if err != nil {
		return out, err
	}
	aead.Seal(out[NonceSize:NonceSize], out[:NonceSize], kPrev[:], scope(sid, ch, ep))
	return out, nil
}

// OpenBack recovers K_prev from a backlink in the epoch-ep bundle.
func OpenBack(kNew Key, sid ServerID, ch uint64, ep uint32, back []byte) (Key, error) {
	if len(back) != BackSize {
		return Key{}, ErrMalformed
	}
	bk, err := backKey(kNew)
	if err != nil {
		return Key{}, err
	}
	aead, err := chacha20poly1305.NewX(bk)
	if err != nil {
		return Key{}, err
	}
	var k Key
	if _, err := aead.Open(k[:0], back[:NonceSize], back[NonceSize:], scope(sid, ch, ep)); err != nil {
		return Key{}, ErrDecrypt
	}
	return k, nil
}

// ShareAD are the fields authenticated by a key.share box (PROTOCOL §13.7).
type ShareAD struct {
	ServerID  ServerID
	Chan      uint64
	Epoch     uint32
	Recipient DevID
}

func (a ShareAD) bytes() []byte {
	b := append([]byte("tc-share-v1"), scope(a.ServerID, a.Chan, a.Epoch)...)
	return append(b, a.Recipient[:]...)
}

// SealShare seals k to one device for key.share, under a fresh ephemeral
// key (returned as eph): ss = X25519(eph, recipient), bk = HKDF-SHA256(ss,
// salt = eph || recipient, info "tc-share-v1"), ct = XChaCha20-Poly1305(bk,
// 0^24, k, AD = "tc-share-v1" || server_id || chan || epoch || recipient dev).
// The zero nonce is safe because eph is never reused.
func SealShare(rand io.Reader, recipientPub [32]byte, ad ShareAD, k Key) (eph [32]byte, ct [BoxCTSize]byte, err error) {
	priv, err := NewEphemeral(rand)
	if err != nil {
		return eph, ct, err
	}
	eph = [32]byte(priv.PublicKey().Bytes())
	bk, err := boxKey(priv, recipientPub, eph, recipientPub, "tc-share-v1")
	if err != nil {
		return eph, ct, err
	}
	aead, err := chacha20poly1305.NewX(bk)
	if err != nil {
		return eph, ct, err
	}
	aead.Seal(ct[:0], zeroNonce[:], k[:], ad.bytes())
	return eph, ct, nil
}

// OpenShare opens a key.share box sealed to recipient. Anyone can seal one:
// the caller must check the key against the epoch's kc.
func OpenShare(recipient *ecdh.PrivateKey, eph [32]byte, ad ShareAD, ct []byte) (Key, error) {
	if recipient == nil {
		return Key{}, ErrBadKey
	}
	if len(ct) != BoxCTSize {
		return Key{}, ErrMalformed
	}
	recipientPub := [32]byte(recipient.PublicKey().Bytes())
	bk, err := boxKey(recipient, eph, eph, recipientPub, "tc-share-v1")
	if err != nil {
		return Key{}, err
	}
	aead, err := chacha20poly1305.NewX(bk)
	if err != nil {
		return Key{}, err
	}
	var k Key
	if _, err := aead.Open(k[:0], zeroNonce[:], ct, ad.bytes()); err != nil {
		return Key{}, ErrDecrypt
	}
	return k, nil
}
