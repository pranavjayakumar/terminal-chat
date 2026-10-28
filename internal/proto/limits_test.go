package proto

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// clientArgs are the types the server decodes from client frames (HELLO,
// REQ and its args).
func clientArgs() []func() any {
	return []func() any{
		func() any { return &Hello{} }, func() any { return &Req{} },
		func() any { return &AgentRegisterArgs{} }, func() any { return &AgentUpdateArgs{} },
		func() any { return &AgentAccessArgs{} }, func() any { return &AgentKeyArgs{} },
		func() any { return &HookCreateArgs{} }, func() any { return &HookArgs{} },
		func() any { return &KeyShareArgs{} }, func() any { return &KeyRotateArgs{} },
		func() any { return &KeyFetchArgs{} }, func() any { return &DeviceRegisterArgs{} },
		func() any { return &DeviceRevokeArgs{} }, func() any { return &DevicesListArgs{} },
		func() any { return &RemindCreateArgs{} }, func() any { return &RemindDeleteArgs{} },
		func() any { return &HuddleJoinArgs{} }, func() any { return &InviteRevokeArgs{} },
		func() any { return &UserUpdateArgs{} }, func() any { return &SyncArgs{} },
		func() any { return &SendArgs{} }, func() any { return &EditArgs{} },
		func() any { return &HistoryArgs{} }, func() any { return &SearchArgs{} },
		func() any { return &ChanCreateArgs{} }, func() any { return &ChanArgs{} },
		func() any { return &ChanAddArgs{} }, func() any { return &ChanTopicArgs{} },
		func() any { return &DMOpenArgs{} }, func() any { return &InviteCreateArgs{} },
		func() any { return &ReactArgs{} }, func() any { return &ReadArgs{} },
		func() any { return &Ephemeral{} },
	}
}

// msgpackKeys lists the map keys of struct type t (and its pointer fields).
func msgpackKeys(t reflect.Type) []string {
	var keys []string
	for i := range t.NumField() {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("msgpack"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		keys = append(keys, tag)
	}
	return keys
}

// listOfNils is a 1-key map {key: [nil × n]}: about n bytes, one item each.
func listOfNils(key string, n int) []byte {
	var b bytes.Buffer
	b.WriteByte(0x81)
	b.WriteByte(0xd9)
	b.WriteByte(byte(len(key)))
	b.WriteString(key)
	b.WriteByte(0xdd)
	binary.Write(&b, binary.BigEndian, uint32(n))
	b.Write(bytes.Repeat([]byte{0xc0}, n))
	return b.Bytes()
}

func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// A frame's worth of nil list items must not cost the server more than a
// few times the frame: a nil decodes into a whole zero element, so before
// the item cap a 1 MiB EPHEMERAL of nil huddle parts allocated ~128 MB.
func TestClientDecodeAllocationIsBounded(t *testing.T) {
	const budget = 16 << 20
	for _, mk := range clientArgs() {
		typ := reflect.TypeOf(mk()).Elem()
		for _, key := range msgpackKeys(typ) {
			payload := listOfNils(key, MaxFrame-64)
			n := allocated(func() { _ = UnmarshalLimit(payload, mk(), MaxClientItems) })
			if n > budget {
				t.Errorf("%s.%s: decoding %d B allocated %d MB", typ.Name(), key, len(payload), n>>20)
			}
			ok := listOfNils(key, MaxClientItems)
			n = allocated(func() { _ = UnmarshalLimit(ok, mk(), MaxClientItems) })
			if n > budget {
				t.Errorf("%s.%s: decoding %d items allocated %d MB", typ.Name(), key, MaxClientItems, n>>20)
			}
		}
	}
}

func TestItemCap(t *testing.T) {
	var v struct {
		L []uint64          `msgpack:"l"`
		M map[uint64]uint64 `msgpack:"m"`
	}
	if err := UnmarshalLimit(listOfNils("l", MaxClientItems), &v, MaxClientItems); err != nil {
		t.Fatalf("%d items refused: %v", MaxClientItems, err)
	}
	if err := UnmarshalLimit(listOfNils("l", MaxClientItems+1), &v, MaxClientItems); err == nil {
		t.Fatal("list over the client cap accepted")
	}
	if err := Unmarshal(listOfNils("l", MaxItems+1), &v); err == nil {
		t.Fatal("list over MaxItems accepted")
	}
	m := map[uint64]uint64{}
	for i := range uint64(MaxClientItems + 1) {
		m[i] = i
	}
	if err := UnmarshalLimit(MustRaw(map[string]any{"m": m}), &v, MaxClientItems); err == nil {
		t.Fatal("map over the client cap accepted")
	}
	delete(m, 0)
	if err := UnmarshalLimit(MustRaw(map[string]any{"m": m}), &v, MaxClientItems); err != nil {
		t.Fatalf("map at the client cap refused: %v", err)
	}
	// The cap reaches into nested REQ args.
	req := MustRaw(Req{ID: 1, Op: OpDevicesList, Args: listOfNils("users", MaxClientItems+1)})
	var r Req
	if err := UnmarshalLimit(req, &r, MaxClientItems); err == nil {
		t.Fatal("REQ with oversized args accepted")
	}
}

// FuzzClientArgs decodes arbitrary bytes, as the server does, into every
// type a client sends. It must never panic.
func FuzzClientArgs(f *testing.F) {
	f.Add([]byte(MustRaw(AgentAccessArgs{Add: []uint64{1, 2}, Temp: []uint64{3}})))
	f.Add([]byte(MustRaw(InviteCreateArgs{Keys: [][]byte{{1, 2}}})))
	f.Add([]byte(MustRaw(HuddleJoinArgs{Chan: 1})))
	f.Add([]byte(MustRaw(KeyShareArgs{Chan: 1, Epoch: 2, Dev: make([]byte, 16), Eph: make([]byte, 32)})))
	f.Add([]byte(MustRaw(RemindCreateArgs{Text: "x", At: 1})))
	f.Add([]byte(MustRaw(DevicesListArgs{Users: []uint64{1}})))
	f.Add(listOfNils("parts", 16))
	f.Add([]byte{0x81, 0xa3, 'a', 'd', 'd', 0xdc, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, mk := range clientArgs() {
			v := mk()
			if UnmarshalLimit(data, v, MaxClientItems) == nil {
				// Whatever decodes must encode again.
				if _, err := Marshal(v); err != nil {
					t.Fatalf("%T: re-encode: %v", v, err)
				}
			}
		}
	})
}
