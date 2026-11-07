package proto

import (
	"encoding/binary"
	"errors"
)

// Voice huddles (PROTOCOL.md §14).

// CapVoice gates huddle ops, huddle ephemerals and datagrams.
const CapVoice = "voice1"

// CapVoiceTCP: voice frames may travel on the frame stream (frame type
// VOICE) when the session has no datagrams, i.e. on the TLS-over-TCP
// fallback (§14.4).
const CapVoiceTCP = "voice_tcp"

const (
	OpHuddleJoin  = "huddle.join"
	OpHuddleLeave = "huddle.leave"

	EphHuddle = "huddle"
)

// Huddle limits (§14.1, §14.3).
const (
	MaxHuddleParts  = 16
	MaxHuddles      = 1024
	MaxDatagram     = 512
	VoiceStreamSize = 16
)

type HuddleJoinArgs struct {
	Chan   uint64 `msgpack:"chan"`
	Dev    []byte `msgpack:"dev,omitempty"`
	Stream []byte `msgpack:"stream,omitempty"`
}

type HuddleJoinRes struct {
	Src uint32 `msgpack:"src"`
}

// Part is one participant of a huddle roster (§14.2).
type Part struct {
	User   uint64 `msgpack:"user"`
	Src    uint32 `msgpack:"src"`
	Dev    []byte `msgpack:"dev,omitempty"`
	Stream []byte `msgpack:"stream,omitempty"`
	// Live: this participant is sharing a pane (cap watch1, §14.5);
	// Watch: it asked for the shared pane's chunks.
	Live  bool `msgpack:"live,omitempty"`
	Watch bool `msgpack:"watch,omitempty"`
}

// Datagram kinds (§14.3, §14.5).
const (
	DgAudio     byte = 0x01
	DgAudioEnc  byte = 0x02
	DgScreen    byte = 0x03 // a shared-pane chunk in a plaintext channel
	DgScreenEnc byte = 0x04 // a sealed shared-pane chunk
)

// DgFlagEnd marks the last frame of a talk spurt.
const DgFlagEnd byte = 0x01

// Header sizes: the relay only ever reads DgHeaderLen bytes.
const (
	DgHeaderLen    = 10
	DgEncHeaderLen = 18
	dgTagLen       = 16
)

var (
	ErrDgShort = errors.New("datagram too short")
	ErrDgLarge = errors.New("datagram too large")
	ErrDgKind  = errors.New("unknown datagram kind")
)

// Datagram is a decoded voice datagram. Payload aliases the input of
// ParseDatagram; for DgAudioEnc it is the ciphertext including the tag.
type Datagram struct {
	Kind    byte
	Flags   byte
	Src     uint32
	Seq     uint32
	Epoch   uint32 // sealed kinds (DgAudioEnc, DgScreenEnc) only
	TS      uint32 // sealed kinds only, Unix seconds
	Payload []byte
}

// EncKind reports whether kind carries the encrypted header (epoch, ts).
func EncKind(kind byte) bool { return kind == DgAudioEnc || kind == DgScreenEnc }

// ScreenKind reports whether kind is a shared-pane chunk (§14.5).
func ScreenKind(kind byte) bool { return kind == DgScreen || kind == DgScreenEnc }

// RelayHeader is what the server needs: kind and src, read without
// looking past byte DgHeaderLen. It accepts audio and screen kinds.
func RelayHeader(b []byte) (kind byte, src uint32, err error) {
	if len(b) > MaxDatagram {
		return 0, 0, ErrDgLarge
	}
	if len(b) < DgHeaderLen+1 {
		return 0, 0, ErrDgShort
	}
	kind = b[0]
	switch kind {
	case DgAudio, DgScreen:
	case DgAudioEnc, DgScreenEnc:
		if len(b) < DgEncHeaderLen+dgTagLen+1 {
			return 0, 0, ErrDgShort
		}
	default:
		return 0, 0, ErrDgKind
	}
	return kind, binary.BigEndian.Uint32(b[2:6]), nil
}

// ParseDatagram decodes a full audio datagram (receivers). Screen kinds
// are ErrDgKind here, so a pane chunk can never reach an audio decoder.
func ParseDatagram(b []byte) (Datagram, error) { return parseDatagram(b, false) }

// ParseScreenDatagram decodes a shared-pane chunk (§14.5); audio kinds are
// ErrDgKind.
func ParseScreenDatagram(b []byte) (Datagram, error) { return parseDatagram(b, true) }

func parseDatagram(b []byte, screen bool) (Datagram, error) {
	kind, src, err := RelayHeader(b)
	if err != nil {
		return Datagram{}, err
	}
	if ScreenKind(kind) != screen {
		return Datagram{}, ErrDgKind
	}
	d := Datagram{Kind: kind, Flags: b[1], Src: src, Seq: binary.BigEndian.Uint32(b[6:10])}
	if EncKind(kind) {
		d.Epoch = binary.BigEndian.Uint32(b[10:14])
		d.TS = binary.BigEndian.Uint32(b[14:18])
		d.Payload = b[DgEncHeaderLen:]
	} else {
		d.Payload = b[DgHeaderLen:]
	}
	return d, nil
}

// HeaderLen is the size of d's header (the AD of an encrypted frame).
func (d Datagram) HeaderLen() int {
	if EncKind(d.Kind) {
		return DgEncHeaderLen
	}
	return DgHeaderLen
}

// AppendHeader appends d's header (not the payload) to dst.
func (d Datagram) AppendHeader(dst []byte) []byte {
	dst = append(dst, d.Kind, d.Flags)
	dst = binary.BigEndian.AppendUint32(dst, d.Src)
	dst = binary.BigEndian.AppendUint32(dst, d.Seq)
	if EncKind(d.Kind) {
		dst = binary.BigEndian.AppendUint32(dst, d.Epoch)
		dst = binary.BigEndian.AppendUint32(dst, d.TS)
	}
	return dst
}

// Marshal encodes d (header + payload).
func (d Datagram) Marshal() []byte {
	b := d.AppendHeader(make([]byte, 0, d.HeaderLen()+len(d.Payload)))
	return append(b, d.Payload...)
}
