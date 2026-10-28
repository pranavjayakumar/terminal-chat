package proto

import (
	"encoding/binary"
	"errors"
)

// MaxDepth is the deepest container nesting accepted in a payload.
const MaxDepth = 16

// Container item caps (PROTOCOL.md §3): a decoder allocates per declared item
// (a nil array item becomes a whole zero struct), so a declared length is
// bounded by more than the bytes left. MaxItems applies to every payload;
// MaxClientItems, above every protocol limit on a list a client sends (the
// largest is devices.list users, 4096, whose excess must still get a clean
// bad_request), to what the server reads.
const (
	MaxItems       = 1 << 16
	MaxClientItems = 8192
)

var (
	errMalformed    = errors.New("malformed msgpack")
	errTooManyItems = errors.New("msgpack container has too many items")
)

// checkMsgpack walks one msgpack value in b without allocating and verifies
// that every declared length fits in the remaining bytes and nesting stays
// within MaxDepth, and that no array or map declares more than maxItems items
// (pairs, for a map). The msgpack library preallocates from declared
// lengths, so this must run before decoding untrusted input (PROTOCOL.md §3).
func checkMsgpack(b []byte, maxItems int) error {
	// pending[i] = items still to read in the container at depth i.
	var pending [MaxDepth + 1]int
	depth := 0
	pending[0] = 1
	p := 0

	for {
		for depth > 0 && pending[depth] == 0 {
			depth--
		}
		if pending[depth] == 0 {
			return nil // trailing bytes are tolerated, like the decoder does
		}
		pending[depth]--

		if p >= len(b) {
			return errMalformed
		}
		c := b[p]
		p++

		var skip, items int
		switch {
		case c <= 0x7f, c >= 0xe0, c == 0xc0, c == 0xc2, c == 0xc3:
		case c >= 0x80 && c <= 0x8f:
			items = 2 * int(c&0x0f)
		case c >= 0x90 && c <= 0x9f:
			items = int(c & 0x0f)
		case c >= 0xa0 && c <= 0xbf:
			skip = int(c & 0x1f)
		case c == 0xc1:
			return errMalformed
		case c == 0xc4, c == 0xd9:
			n, ok := readLen(b, &p, 1)
			if !ok {
				return errMalformed
			}
			skip = n
		case c == 0xc5, c == 0xda:
			n, ok := readLen(b, &p, 2)
			if !ok {
				return errMalformed
			}
			skip = n
		case c == 0xc6, c == 0xdb:
			n, ok := readLen(b, &p, 4)
			if !ok {
				return errMalformed
			}
			skip = n
		case c == 0xc7, c == 0xc8, c == 0xc9:
			n, ok := readLen(b, &p, 1<<(c-0xc7))
			if !ok {
				return errMalformed
			}
			skip = n + 1
		case c == 0xca:
			skip = 4
		case c == 0xcb:
			skip = 8
		case c >= 0xcc && c <= 0xcf:
			skip = 1 << (c - 0xcc)
		case c >= 0xd0 && c <= 0xd3:
			skip = 1 << (c - 0xd0)
		case c >= 0xd4 && c <= 0xd8:
			skip = 1 + 1<<(c-0xd4)
		case c == 0xdc, c == 0xde:
			n, ok := readLen(b, &p, 2)
			if !ok {
				return errMalformed
			}
			items = n
			if c == 0xde {
				items *= 2
			}
		case c == 0xdd, c == 0xdf:
			n, ok := readLen(b, &p, 4)
			if !ok {
				return errMalformed
			}
			items = n
			if c == 0xdf {
				items *= 2
			}
		}

		if skip > len(b)-p {
			return errMalformed
		}
		p += skip

		if c >= 0x80 && c <= 0x8f || c == 0xde || c == 0xdf {
			if items/2 > maxItems {
				return errTooManyItems
			}
		} else if items > maxItems {
			return errTooManyItems
		}
		if items > 0 {
			// Each item needs at least one byte.
			if items > len(b)-p {
				return errMalformed
			}
			if depth == MaxDepth {
				return errors.New("msgpack nested too deeply")
			}
			depth++
			pending[depth] = items
		}
	}
}

func readLen(b []byte, p *int, size int) (int, bool) {
	if len(b)-*p < size {
		return 0, false
	}
	var n uint64
	switch size {
	case 1:
		n = uint64(b[*p])
	case 2:
		n = uint64(binary.BigEndian.Uint16(b[*p:]))
	case 4:
		n = uint64(binary.BigEndian.Uint32(b[*p:]))
	}
	*p += size
	if n > uint64(len(b)) {
		return 0, false
	}
	return int(n), true
}
