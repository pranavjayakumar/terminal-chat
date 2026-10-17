package e2ecrypto

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"

	"golang.org/x/crypto/chacha20poly1305"
)

// Voice frame keys (PROTOCOL.md §14.3, docs/VOICE.md).

// VoiceStreamSize is the random per-join stream id.
const VoiceStreamSize = 16

// VoiceKey is vk = HKDF-SHA256(K, info "tc-voice-v1" || server_id || chan || epoch).
func VoiceKey(k Key, sid ServerID, ch uint64, ep uint32) (Key, error) {
	b, err := hkdf.Key(sha256.New, k[:], nil, "tc-voice-v1"+string(scope(sid, ch, ep)), KeySize)
	if err != nil {
		return Key{}, err
	}
	return Key(b), nil
}

// VoiceSender seals or opens one sender stream's frames under
// sk = HKDF-SHA256(vk, info "tc-voice-send-v1" || dev || stream).
// Per-sender keys mean two senders never share a (key, nonce), even when
// one copies the other's stream id.
type VoiceSender struct{ aead cipher.AEAD }

func NewVoiceSender(vk Key, dev DevID, stream [VoiceStreamSize]byte) (*VoiceSender, error) {
	info := make([]byte, 0, 16+IDSize+VoiceStreamSize)
	info = append(info, "tc-voice-send-v1"...)
	info = append(info, dev[:]...)
	info = append(info, stream[:]...)
	sk, err := hkdf.Key(sha256.New, vk[:], nil, string(info), KeySize)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(sk)
	if err != nil {
		return nil, err
	}
	return &VoiceSender{aead}, nil
}

func voiceNonce(seq uint32) []byte {
	n := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(n[4:], uint64(seq))
	return n
}

// Seal appends the ciphertext of opus to header (the frame's AD) and
// returns the whole datagram. seq must never repeat for this sender.
func (v *VoiceSender) Seal(seq uint32, header, opus []byte) []byte {
	out := make([]byte, len(header), len(header)+len(opus)+TagSize)
	copy(out, header)
	return v.aead.Seal(out, voiceNonce(seq), opus, header)
}

// Open authenticates header and decrypts ct.
func (v *VoiceSender) Open(seq uint32, header, ct []byte) ([]byte, error) {
	if len(ct) < TagSize {
		return nil, ErrMalformed
	}
	p, err := v.aead.Open(nil, voiceNonce(seq), ct, header)
	if err != nil {
		return nil, ErrDecrypt
	}
	return p, nil
}
