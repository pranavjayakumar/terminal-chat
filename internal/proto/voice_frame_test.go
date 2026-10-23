package proto

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// TestVoiceFrame: a VOICE frame carries a datagram's bytes unchanged (raw,
// not MessagePack), and TryWriteRaw never waits behind another write.
func TestVoiceFrame(t *testing.T) {
	d := Datagram{Kind: DgAudio, Flags: DgFlagEnd, Src: 7, Seq: 42, Payload: []byte("opus")}.Marshal()
	var buf bytes.Buffer
	c := NewCodec(&buf)
	if ok, err := c.TryWriteRaw(TVoice, d); !ok || err != nil {
		t.Fatalf("idle stream: %v %v", ok, err)
	}
	f, err := c.Read()
	if err != nil || f.Type != TVoice || !bytes.Equal(f.Payload, d) {
		t.Fatalf("read back %#x %x %v", f.Type, f.Payload, err)
	}
	if got, err := ParseDatagram(f.Payload); err != nil || got.Src != 7 || got.Seq != 42 {
		t.Fatalf("parse: %+v %v", got, err)
	}

	// A write blocked on a peer that doesn't read holds the stream: voice
	// is refused at once instead of queueing behind it.
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	pc := NewCodec(a)
	go func() { _ = pc.WriteRaw(TEvent, []byte("chat")) }()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if ok, _ := pc.TryWriteRaw(TVoice, d); ok {
		t.Fatal("voice written past a blocked write")
	}
	if time.Since(start) > 10*time.Millisecond {
		t.Fatal("TryWriteRaw waited")
	}
	go func() { _, _ = io.Copy(io.Discard, b) }()
	if _, err := pc.TryWriteRaw(TVoice, make([]byte, MaxFrame)); err != ErrFrameTooLarge {
		t.Fatalf("oversized: %v", err)
	}
}
