package e2ecrypto

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

func voiceFixture(t *testing.T) (Key, ServerID, DevID, [VoiceStreamSize]byte) {
	t.Helper()
	var k Key
	var sid ServerID
	var dev DevID
	var st [VoiceStreamSize]byte
	for i := range k {
		k[i] = byte(i)
	}
	sid[0], dev[0], st[0] = 1, 2, 3
	return k, sid, dev, st
}

// TestVoiceKeyDerivation recomputes §14.3 from its spec text, so a change
// to the derivation (labels, field order, nonce layout) fails here.
func TestVoiceKeyDerivation(t *testing.T) {
	k, sid, dev, st := voiceFixture(t)
	vk, err := VoiceKey(k, sid, 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	info := []byte("tc-voice-v1")
	info = append(info, sid[:]...)
	info = binary.BigEndian.AppendUint64(info, 7)
	info = binary.BigEndian.AppendUint32(info, 9)
	want, _ := hkdf.Key(sha256.New, k[:], nil, string(info), 32)
	if !bytes.Equal(vk[:], want) {
		t.Fatal("vk derivation drifted from PROTOCOL.md §14.3")
	}
	sinfo := append(append([]byte("tc-voice-send-v1"), dev[:]...), st[:]...)
	sk, _ := hkdf.Key(sha256.New, vk[:], nil, string(sinfo), 32)
	aead, _ := chacha20poly1305.New(sk)
	hdr := []byte("header-bytes-18xx")
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], 42)
	wantCT := aead.Seal(append([]byte{}, hdr...), nonce, []byte("opus"), hdr)

	s, err := NewVoiceSender(vk, dev, st)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Seal(42, hdr, []byte("opus")); !bytes.Equal(got, wantCT) {
		t.Fatal("frame sealing drifted from PROTOCOL.md §14.3")
	}
}

func TestVoiceSealOpen(t *testing.T) {
	k, sid, dev, st := voiceFixture(t)
	vk, _ := VoiceKey(k, sid, 7, 1)
	s, _ := NewVoiceSender(vk, dev, st)
	hdr := []byte{2, 0, 0, 0, 0, 1, 0, 0, 0, 5, 0, 0, 0, 1, 0, 0, 0, 9}
	dg := s.Seal(5, hdr, []byte("hello opus"))
	ct := dg[len(hdr):]

	r, _ := NewVoiceSender(vk, dev, st)
	p, err := r.Open(5, hdr, ct)
	if err != nil || string(p) != "hello opus" {
		t.Fatalf("open: %q %v", p, err)
	}

	// Forgeries: every input is bound.
	other := func(mut func(*Key, *DevID, *[VoiceStreamSize]byte, *uint32)) error {
		k2, dev2, st2, ep := k, dev, st, uint32(1)
		mut(&k2, &dev2, &st2, &ep)
		vk2, _ := VoiceKey(k2, sid, 7, ep)
		r2, _ := NewVoiceSender(vk2, dev2, st2)
		_, err := r2.Open(5, hdr, ct)
		return err
	}
	for name, mut := range map[string]func(*Key, *DevID, *[VoiceStreamSize]byte, *uint32){
		"epoch key": func(k *Key, _ *DevID, _ *[VoiceStreamSize]byte, _ *uint32) { k[0] ^= 1 },
		"epoch":     func(_ *Key, _ *DevID, _ *[VoiceStreamSize]byte, ep *uint32) { *ep = 2 },
		"dev":       func(_ *Key, d *DevID, _ *[VoiceStreamSize]byte, _ *uint32) { d[0] ^= 1 },
		"stream":    func(_ *Key, _ *DevID, s *[VoiceStreamSize]byte, _ *uint32) { s[15] ^= 1 },
	} {
		if err := other(mut); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := r.Open(6, hdr, ct); !errors.Is(err, ErrDecrypt) {
		t.Error("seq not bound")
	}
	bad := bytes.Clone(hdr)
	bad[1] = 1
	if _, err := r.Open(5, bad, ct); !errors.Is(err, ErrDecrypt) {
		t.Error("header not bound")
	}
	flip := bytes.Clone(ct)
	flip[0] ^= 1
	if _, err := r.Open(5, hdr, flip); !errors.Is(err, ErrDecrypt) {
		t.Error("ciphertext not authenticated")
	}
	if _, err := r.Open(5, hdr, ct[:10]); !errors.Is(err, ErrMalformed) {
		t.Error("short ct")
	}
}
