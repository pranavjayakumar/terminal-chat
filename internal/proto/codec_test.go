package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	c := NewCodec(&buf)
	c.SetMaxFrame(MaxFrame)

	msg := Message{ID: 42, Chan: 7, Author: 3, Body: "hi 👋\nthere", Time: 1700000000000,
		Reactions: map[string][]uint64{"👍": {1, 2}}}
	ev := Event{Seq: 42, Kind: KMsgNew, Chan: 7, Time: 1, Actor: 3, Data: MustRaw(msg)}
	if err := c.Write(TEvent, ev); err != nil {
		t.Fatal(err)
	}
	req := Req{ID: 1, Op: OpSend, Args: MustRaw(SendArgs{Chan: 7, Body: "x", Nonce: make([]byte, 16)})}
	if err := c.Write(TReq, req); err != nil {
		t.Fatal(err)
	}

	f, err := c.Read()
	if err != nil || f.Type != TEvent {
		t.Fatalf("read event: %v %v", f.Type, err)
	}
	var gotEv Event
	if err := f.Decode(&gotEv); err != nil {
		t.Fatal(err)
	}
	var gotMsg Message
	if err := Unmarshal(gotEv.Data, &gotMsg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotMsg, msg) || gotEv.Seq != 42 || gotEv.Kind != KMsgNew {
		t.Fatalf("event mismatch: %+v %+v", gotEv, gotMsg)
	}

	f, err = c.Read()
	if err != nil || f.Type != TReq {
		t.Fatalf("read req: %v %v", f.Type, err)
	}
	var gotReq Req
	if err := f.Decode(&gotReq); err != nil {
		t.Fatal(err)
	}
	var args SendArgs
	if err := Unmarshal(gotReq.Args, &args); err != nil || args.Chan != 7 || len(args.Nonce) != 16 {
		t.Fatalf("args mismatch: %+v %v", args, err)
	}
}

func TestUnknownFieldsIgnored(t *testing.T) {
	b, _ := Marshal(map[string]any{"id": 5, "name": "a", "future_field": []int{1, 2, 3}})
	var u User
	if err := Unmarshal(b, &u); err != nil || u.ID != 5 || u.Name != "a" {
		t.Fatalf("got %+v, %v", u, err)
	}
}

func frameBytes(n uint32, rest []byte) []byte {
	b := make([]byte, 4, 4+len(rest))
	binary.BigEndian.PutUint32(b, n)
	return append(b, rest...)
}

func TestReadLimits(t *testing.T) {
	c := NewCodec(bytes.NewBuffer(frameBytes(MaxPreAuthFrame+1, nil)))
	if _, err := c.Read(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("pre-auth oversize: %v", err)
	}
	c = NewCodec(bytes.NewBuffer(frameBytes(0, nil)))
	if _, err := c.Read(); !errors.Is(err, ErrEmptyFrame) {
		t.Fatalf("empty: %v", err)
	}
	c = NewCodec(bytes.NewBuffer(frameBytes(100, []byte{0x10, 1, 2})))
	if _, err := c.Read(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated: %v", err)
	}
	c = NewCodec(bytes.NewBuffer(frameBytes(MaxFrame, nil)))
	c.SetMaxFrame(MaxFrame)
	if _, err := c.Read(); errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("max-size frame rejected after auth")
	}
}

func TestUnknownTypePassesThrough(t *testing.T) {
	c := NewCodec(bytes.NewBuffer(frameBytes(3, []byte{0x99, 0xc0, 0xc0})))
	f, err := c.Read()
	if err != nil || f.Type != 0x99 {
		t.Fatalf("%v %v", f.Type, err)
	}
}

func TestValidName(t *testing.T) {
	for _, s := range []string{"a", "general", "dev-ops", "x_1", "abcdefghijabcdefghijabcdefghijab"} {
		if !ValidName(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "-a", "A", "has space", "abcdefghijabcdefghijabcdefghijabc", "é"} {
		if ValidName(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

// FuzzDecode feeds arbitrary bytes through the frame reader and every payload
// decoder. It must never panic or allocate beyond the frame limit.
func FuzzDecode(f *testing.F) {
	seed := func(t FrameType, v any) {
		var buf bytes.Buffer
		NewCodec(&buf).Write(t, v)
		f.Add(buf.Bytes())
	}
	seed(THello, Hello{Client: "tc", Auth: Auth{Pubkey: []byte{1}, Sig: []byte{2}}})
	seed(TReq, Req{ID: 1, Op: OpSend, Args: MustRaw(SendArgs{Chan: 1, Body: "x"})})
	seed(TEvent, Event{Seq: 1, Kind: KMsgNew, Data: MustRaw(Message{ID: 1})})
	seed(TSnapshot, Snapshot{Users: []User{{ID: 1}}, Read: map[uint64]uint64{1: 2}})
	sig := Signed{B: []byte{0x81, 0xa1, 'v', 1}, Sig: []byte{1, 2}}
	seed(TReq, Req{ID: 2, Op: OpDeviceRegister, Args: MustRaw(DeviceRegisterArgs{Cert: sig, Endorse: &sig})})
	seed(TEvent, Event{Seq: 2, Kind: KDeviceRevoke, Data: MustRaw(DeviceRevokeArgs{Revoke: sig})})
	seed(TSnapshot, Snapshot{Devs: []Device{{Cert: sig}}, Revoked: []Signed{sig}})
	seed(TRes, Res{ID: 3, OK: true, Data: MustRaw(DevicesListRes{Certs: []Signed{sig}, Next: 4})})
	f.Add([]byte{0, 0, 0, 1, 0x30})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})

	targets := []func() any{
		func() any { return &Hello{} }, func() any { return &Welcome{} },
		func() any { return &Req{} }, func() any { return &Res{} },
		func() any { return &Event{} }, func() any { return &Ephemeral{} },
		func() any { return &Snapshot{} }, func() any { return &Bye{} },
		func() any { return &Message{} }, func() any { return &SendArgs{} },
		func() any { return &HistoryRes{} }, func() any { return &DMOpenArgs{} },
		func() any { return &Device{} }, func() any { return &DeviceRevokeArgs{} },
		func() any { return &DevicesListArgs{} }, func() any { return &DevicesListRes{} },
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		c := NewCodec(bytes.NewBuffer(data))
		c.SetMaxFrame(MaxFrame)
		for i := 0; i < 8; i++ {
			fr, err := c.Read()
			if err != nil {
				return
			}
			for _, mk := range targets {
				_ = fr.Decode(mk())
			}
			// Op args, RES data and event data are nested Raw values.
			var env struct {
				Args Raw `msgpack:"args"`
				Data Raw `msgpack:"data"`
			}
			if fr.Decode(&env) == nil {
				for _, raw := range []Raw{env.Args, env.Data} {
					for _, mk := range targets {
						_ = Unmarshal(raw, mk())
					}
				}
			}
		}
	})
}

func TestCheckRejectsLyingLengths(t *testing.T) {
	bad := [][]byte{
		{0xc6, 0xcc, 0x69, 0xa5, 0x75},       // bin32 claiming 3.4 GB
		{0xdd, 0xff, 0xff, 0xff, 0xff, 0x00}, // array32 of 4G items
		{0xdf, 0x00, 0x10, 0x00, 0x00},       // map32 with no items present
		{0x92, 0x01},                         // fixarray(2) with 1 item
		{0xc1},                               // never-used byte
	}
	for _, b := range bad {
		var v any
		if err := Unmarshal(b, &v); err == nil {
			t.Errorf("accepted %x", b)
		}
	}
	deep := bytes.Repeat([]byte{0x91}, MaxDepth+1)
	deep = append(deep, 0x01)
	var v any
	if err := Unmarshal(deep, &v); err == nil {
		t.Error("accepted over-deep nesting")
	}
	ok := append(bytes.Repeat([]byte{0x91}, MaxDepth), 0x01)
	if err := Unmarshal(ok, &v); err != nil {
		t.Errorf("rejected max-depth nesting: %v", err)
	}
}

func TestDeviceTypesRoundTrip(t *testing.T) {
	sig := Signed{B: []byte{1, 2, 3}, Sig: []byte{4, 5}}
	in := Snapshot{Devs: []Device{{Cert: sig}, {Cert: sig, Endorse: &sig}}, Revoked: []Signed{sig}}
	var out Snapshot
	if err := Unmarshal(MustRaw(in), &out); err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("snapshot: %+v %v", out, err)
	}
	// Absent optional fields cost nothing for clients without e2e1.
	if b := MustRaw(Snapshot{}); len(b) != 1 {
		t.Fatalf("empty snapshot is %d bytes", len(b))
	}
	var dev Device
	if err := Unmarshal(MustRaw(Device{Cert: sig}), &dev); err != nil || dev.Endorse != nil {
		t.Fatalf("device without endorse: %+v %v", dev, err)
	}
	var lr DevicesListRes
	if err := Unmarshal(MustRaw(DevicesListRes{Certs: []Signed{sig}, Next: 9}), &lr); err != nil || lr.Next != 9 || len(lr.Certs) != 1 {
		t.Fatalf("list res: %+v %v", lr, err)
	}
}

// FuzzDevices decodes arbitrary msgpack as every §12 device type. Signed.B
// must survive a decode/encode/decode cycle byte for byte: signatures cover
// it verbatim.
func FuzzDevices(f *testing.F) {
	sig := Signed{B: []byte{0x81, 0xa1, 'v', 1}, Sig: bytes.Repeat([]byte{7}, 64)}
	f.Add([]byte(MustRaw(Device{Cert: sig, Endorse: &sig})))
	f.Add([]byte(MustRaw(DeviceRevokeArgs{Revoke: sig})))
	f.Add([]byte(MustRaw(DevicesListArgs{Users: []uint64{1, 2}, After: 1})))
	f.Add([]byte(MustRaw(DevicesListRes{Certs: []Signed{sig}, Endorse: []Signed{sig}, Revoked: []Signed{sig}, Next: 3})))
	f.Add([]byte(MustRaw(Snapshot{Devs: []Device{{Cert: sig}}, Revoked: []Signed{sig}})))
	f.Add([]byte{0xc1})
	f.Fuzz(func(t *testing.T, data []byte) {
		var d Device
		if Unmarshal(data, &d) == nil {
			var again Device
			if err := Unmarshal(MustRaw(d), &again); err != nil {
				t.Fatalf("re-decode: %v", err)
			}
			if !bytes.Equal(again.Cert.B, d.Cert.B) || !bytes.Equal(again.Cert.Sig, d.Cert.Sig) ||
				(d.Endorse == nil) != (again.Endorse == nil) {
				t.Fatalf("device changed across round trip: %+v vs %+v", d, again)
			}
		}
		_ = Unmarshal(data, &DeviceRevokeArgs{})
		_ = Unmarshal(data, &DevicesListArgs{})
		_ = Unmarshal(data, &DevicesListRes{})
		_ = Unmarshal(data, &Snapshot{})
	})
}
