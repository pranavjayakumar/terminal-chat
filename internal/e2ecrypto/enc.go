package e2ecrypto

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// Enc is a parsed enc. CT aliases the input slice.
type Enc struct {
	Epoch uint32
	EH    EpochHash
	Dev   DevID
	Nonce [NonceSize]byte
	CT    []byte
	Sig   [SigSize]byte
}

// pad256 appends 0x80 then zeros up to a multiple of PadBlock.
func pad256(plain []byte) []byte {
	n := (len(plain) + 1 + PadBlock - 1) / PadBlock * PadBlock
	out := make([]byte, n)
	copy(out, plain)
	out[len(plain)] = 0x80
	return out
}

// unpad accepts only canonical pad256 output: the 0x80 marker must be in the final block.
func unpad(b []byte) ([]byte, error) {
	if len(b) == 0 || len(b)%PadBlock != 0 {
		return nil, ErrBadPadding
	}
	i := len(b) - 1
	for i >= 0 && b[i] == 0 {
		i--
	}
	if i < 0 || b[i] != 0x80 || len(b)-i > PadBlock {
		return nil, ErrBadPadding
	}
	return b[:i], nil
}

// msgKey is mk = HKDF-SHA256(K, info "tc-msg-v1" || server_id || chan || epoch).
func msgKey(k Key, sid ServerID, ch uint64, ep uint32) ([]byte, error) {
	return hkdf.Key(sha256.New, k[:], nil, "tc-msg-v1"+string(scope(sid, ch, ep)), KeySize)
}

// msgAD is "tc-msg-v1" || server_id || chan || enc[0:61].
func msgAD(sid ServerID, ch uint64, hdr []byte) []byte {
	ad := make([]byte, 0, 9+IDSize+8+EncHeaderSize)
	ad = append(ad, "tc-msg-v1"...)
	ad = append(ad, sid[:]...)
	ad = binary.BigEndian.AppendUint64(ad, ch)
	return append(ad, hdr...)
}

// sigBody is server_id || chan || enc[0:61] || ct; signed under CtxMsgSig.
func sigBody(sid ServerID, ch uint64, hdrAndCT []byte) []byte {
	b := make([]byte, 0, IDSize+8+len(hdrAndCT))
	b = append(b, sid[:]...)
	b = binary.BigEndian.AppendUint64(b, ch)
	return append(b, hdrAndCT...)
}

// Encrypt builds enc = 0x01 || epoch || eh || dev || nonce || ct || sig (§13.4).
func Encrypt(rand io.Reader, k Key, signer ed25519.PrivateKey, sid ServerID, ch uint64, ep uint32,
	eh EpochHash, dev DevID, plain []byte) ([]byte, error) {
	if len(plain) > MaxPlainSize {
		return nil, ErrTooLarge
	}
	if len(signer) != ed25519.PrivateKeySize {
		return nil, ErrBadKey
	}
	padded := pad256(plain)
	enc := make([]byte, EncHeaderSize, EncHeaderSize+len(padded)+TagSize+SigSize)
	enc[0] = EncVersion
	binary.BigEndian.PutUint32(enc[1:5], ep)
	copy(enc[5:21], eh[:])
	copy(enc[21:37], dev[:])
	if _, err := io.ReadFull(rand, enc[37:EncHeaderSize]); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	mk, err := msgKey(k, sid, ch, ep)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(mk)
	if err != nil {
		return nil, err
	}
	enc = aead.Seal(enc, enc[37:EncHeaderSize], padded, msgAD(sid, ch, enc[:EncHeaderSize]))
	sig, err := SignEd25519(signer, CtxMsgSig, sigBody(sid, ch, enc))
	if err != nil {
		return nil, err
	}
	return append(enc, sig...), nil
}

// ParseEnc validates enc's framing (version, size bounds, whole padding blocks) without keys.
func ParseEnc(enc []byte) (Enc, error) {
	if len(enc) < MinEncSize {
		return Enc{}, ErrMalformed
	}
	if len(enc) > MaxEncSize {
		return Enc{}, ErrTooLarge
	}
	if enc[0] != EncVersion {
		return Enc{}, ErrVersion
	}
	ct := enc[EncHeaderSize : len(enc)-SigSize]
	if (len(ct)-TagSize)%PadBlock != 0 {
		return Enc{}, ErrMalformed
	}
	e := Enc{
		Epoch: binary.BigEndian.Uint32(enc[1:5]),
		EH:    EpochHash(enc[5:21]),
		Dev:   DevID(enc[21:37]),
		Nonce: [NonceSize]byte(enc[37:EncHeaderSize]),
		CT:    ct,
		Sig:   [SigSize]byte(enc[len(enc)-SigSize:]),
	}
	return e, nil
}

// VerifyEnc checks enc's framing and its signature by pub without any key
// (what a server can check: the enc came from that device, for this channel).
func VerifyEnc(pub ed25519.PublicKey, sid ServerID, ch uint64, enc []byte) (Enc, error) {
	e, err := ParseEnc(enc)
	if err != nil {
		return e, err
	}
	if !VerifyEd25519(pub, CtxMsgSig, sigBody(sid, ch, enc[:len(enc)-SigSize]), e.Sig[:]) {
		return e, ErrBadSignature
	}
	return e, nil
}

// Decrypt verifies enc's signature by pub, then decrypts with the epoch key k
// (the caller picks k by ParseEnc(enc).Epoch). The signature is checked first so
// a member holding K still can't pass off a message as another device's.
func Decrypt(k Key, pub ed25519.PublicKey, sid ServerID, ch uint64, enc []byte) ([]byte, error) {
	e, err := VerifyEnc(pub, sid, ch, enc)
	if err != nil {
		return nil, err
	}
	mk, err := msgKey(k, sid, ch, e.Epoch)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(mk)
	if err != nil {
		return nil, err
	}
	padded, err := aead.Open(nil, e.Nonce[:], e.CT, msgAD(sid, ch, enc[:EncHeaderSize]))
	if err != nil {
		return nil, ErrDecrypt
	}
	return unpad(padded)
}

// ReactTagPrefix starts every encrypted reaction tag (§13.4).
const ReactTagPrefix = "~"

// ReactTagLen is len(ReactTag(...)): "~" and 16 base64url characters.
const ReactTagLen = 1 + 16

// ReactTag is what an encrypted channel's server sees instead of a reaction
// emoji: "~" || base64url(HMAC-SHA256(rk, "tc-react" || mid || emoji)[:12]),
// rk = HKDF-SHA256(mk, info "tc-react-v1"), with mk the message key of the
// reacted message's epoch (a subkey of its own, so mk is only ever an AEAD
// key). Equal emoji on one message give equal tags (set semantics keep
// working) without revealing it.
func ReactTag(k Key, sid ServerID, ch uint64, ep uint32, mid MsgID, emoji string) (string, error) {
	mk, err := msgKey(k, sid, ch, ep)
	if err != nil {
		return "", err
	}
	rk, err := hkdf.Key(sha256.New, mk, nil, "tc-react-v1", KeySize)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, rk)
	m.Write([]byte("tc-react"))
	m.Write(mid[:])
	m.Write([]byte(emoji))
	return ReactTagPrefix + base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:12]), nil
}

// ValidReactTag reports whether s has the shape of a ReactTag.
func ValidReactTag(s string) bool {
	if len(s) != ReactTagLen || s[:1] != ReactTagPrefix {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s[1:])
	return err == nil && len(b) == 12
}
