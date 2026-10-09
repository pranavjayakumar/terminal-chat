package proto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"
	"sync/atomic"

	"github.com/vmihailenco/msgpack/v5"
)

var (
	// ErrFrameTooLarge means a peer announced a frame over the current limit.
	ErrFrameTooLarge = errors.New("frame too large")
	// ErrEmptyFrame means a peer sent a zero-length frame.
	ErrEmptyFrame = errors.New("empty frame")
)

// Frame is one decoded frame header plus its raw msgpack payload.
type Frame struct {
	Type    FrameType
	Payload []byte
}

// Decode unmarshals the frame payload into v.
func (f Frame) Decode(v any) error { return Unmarshal(f.Payload, v) }

// Marshal encodes v as msgpack with compact ints.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := msgpack.GetEncoder()
	defer msgpack.PutEncoder(enc)
	enc.Reset(&buf)
	enc.UseCompactInts(true)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Unmarshal decodes msgpack into v. Unknown map keys are ignored.
func Unmarshal(b []byte, v any) error {
	if err := checkMsgpack(b); err != nil {
		return fmt.Errorf("decoding payload: %w", err)
	}
	if err := msgpack.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decoding payload: %w", err)
	}
	return nil
}

// MustRaw marshals v and panics on failure; only for values that can't fail
// (plain structs of known types).
func MustRaw(v any) Raw {
	b, err := Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Codec reads and writes frames on a stream. Reads must come from a single
// goroutine; writes are serialized internally and may come from any.
type Codec struct {
	r   *bufio.Reader
	wmu sync.Mutex
	w   *bufio.Writer
	max atomic.Int64
}

// NewCodec wraps rw. The read limit starts at MaxPreAuthFrame; call
// SetMaxFrame(MaxFrame) after authentication.
func NewCodec(rw io.ReadWriter) *Codec {
	c := &Codec{r: bufio.NewReaderSize(rw, 16<<10), w: bufio.NewWriterSize(rw, 16<<10)}
	c.max.Store(MaxPreAuthFrame)
	return c
}

// SetMaxFrame changes the largest frame Read accepts.
func (c *Codec) SetMaxFrame(n int) { c.max.Store(int64(n)) }

// Read returns the next frame. Unknown types are returned too; callers skip them.
func (c *Codec) Read() (Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return Frame{}, ErrEmptyFrame
	}
	if int64(n) > c.max.Load() {
		return Frame{}, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return Frame{Type: FrameType(buf[0]), Payload: buf[1:]}, nil
}

// Write encodes v and writes it as one frame of type t, flushing immediately.
func (c *Codec) Write(t FrameType, v any) error {
	payload, err := Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding frame %#x: %w", t, err)
	}
	return c.WriteRaw(t, payload)
}

// WriteRaw writes an already-encoded payload as one frame.
func (c *Codec) WriteRaw(t FrameType, payload []byte) error {
	if len(payload)+1 > MaxFrame {
		return ErrFrameTooLarge
	}
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(payload)+1))
	hdr[4] = byte(t)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := c.w.Write(payload); err != nil {
		return err
	}
	return c.w.Flush()
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidName reports whether s is a valid user or channel name (PROTOCOL.md §9).
func ValidName(s string) bool { return nameRE.MatchString(s) }
