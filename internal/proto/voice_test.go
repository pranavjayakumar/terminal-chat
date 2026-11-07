package proto

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func TestDatagramRoundTrip(t *testing.T) {
	for _, d := range []Datagram{
		{Kind: DgAudio, Flags: DgFlagEnd, Src: 7, Seq: 1<<32 - 1, Payload: []byte{0xfc}},
		{Kind: DgAudioEnc, Src: 1, Seq: 3, Epoch: 9, TS: 1_800_000_000, Payload: bytes.Repeat([]byte{1}, 17)},
	} {
		b := d.Marshal()
		if len(b) != d.HeaderLen()+len(d.Payload) {
			t.Fatalf("len %d", len(b))
		}
		got, err := ParseDatagram(b)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, d) {
			t.Fatalf("got %+v want %+v", got, d)
		}
		k, src, err := RelayHeader(b)
		if err != nil || k != d.Kind || src != d.Src {
			t.Fatalf("relay header %v %v %v", k, src, err)
		}
	}
}

func TestDatagramRejects(t *testing.T) {
	enc := Datagram{Kind: DgAudioEnc, Src: 1, Payload: make([]byte, 17)}.Marshal()
	cases := map[string]struct {
		b    []byte
		want error
	}{
		"empty":        {nil, ErrDgShort},
		"header only":  {Datagram{Kind: DgAudio, Src: 1}.Marshal(), ErrDgShort},
		"enc no tag":   {enc[:len(enc)-1], ErrDgShort},
		"unknown kind": {append([]byte{0x03}, make([]byte, 20)...), ErrDgKind},
		"kind zero":    {make([]byte, 20), ErrDgKind},
		"too large":    {Datagram{Kind: DgAudio, Payload: make([]byte, MaxDatagram)}.Marshal(), ErrDgLarge},
	}
	for name, c := range cases {
		if _, err := ParseDatagram(c.b); !errors.Is(err, c.want) {
			t.Errorf("%s: err %v want %v", name, err, c.want)
		}
	}
	if _, err := ParseDatagram(Datagram{Kind: DgAudio, Payload: make([]byte, MaxDatagram-DgHeaderLen)}.Marshal()); err != nil {
		t.Errorf("max size: %v", err)
	}
}

func TestHuddleEphemeralRoundTrip(t *testing.T) {
	e := Ephemeral{Kind: EphHuddle, Chan: 4, Parts: []Part{
		{User: 1, Src: 2},
		{User: 3, Src: 5, Dev: bytes.Repeat([]byte{1}, 16), Stream: bytes.Repeat([]byte{2}, 16)},
	}}
	b, err := Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var got Ephemeral
	if err := Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, e) {
		t.Fatalf("got %+v", got)
	}
	// An ended huddle has no parts key at all.
	b, _ = Marshal(Ephemeral{Kind: EphHuddle, Chan: 4})
	if bytes.Contains(b, []byte("parts")) {
		t.Fatal("empty roster should omit parts")
	}
	a := HuddleJoinArgs{Chan: 9, Dev: []byte{1}, Stream: []byte{2}}
	b, _ = Marshal(a)
	var ga HuddleJoinArgs
	if err := Unmarshal(b, &ga); err != nil || !reflect.DeepEqual(ga, a) {
		t.Fatalf("args %+v %v", ga, err)
	}
}

func FuzzParseDatagram(f *testing.F) {
	f.Add(Datagram{Kind: DgAudio, Src: 1, Payload: []byte{1, 2}}.Marshal())
	f.Add(Datagram{Kind: DgAudioEnc, Src: 1, Epoch: 2, Payload: make([]byte, 20)}.Marshal())
	f.Add([]byte{})
	f.Add(Datagram{Kind: DgScreen, Src: 1, Payload: []byte{1, 2}}.Marshal())
	f.Add(Datagram{Kind: DgScreenEnc, Src: 1, Epoch: 2, Payload: make([]byte, 20)}.Marshal())
	f.Fuzz(func(t *testing.T, b []byte) {
		k, src, rerr := RelayHeader(b)
		d, err := ParseDatagram(b)
		sd, serr := ParseScreenDatagram(b)
		// The relay accepts exactly what one of the two parsers accepts,
		// and never both: a screen chunk is never audio.
		if (rerr == nil) != (err == nil || serr == nil) || err == nil && serr == nil {
			t.Fatalf("parsers disagree: relay %v audio %v screen %v", rerr, err, serr)
		}
		if rerr != nil {
			return
		}
		if serr == nil {
			d = sd
		}
		if k != d.Kind || src != d.Src {
			t.Fatal("header mismatch")
		}
		if !bytes.Equal(d.Marshal(), b) {
			t.Fatal("re-encode differs")
		}
	})
}
