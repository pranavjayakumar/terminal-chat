package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/trust"
)

// Malicious-server tests for the client's e2e key handling: a scripted fake
// server feeds the real client certs, revokes and bundles, and the tests
// check what the client trusts.

const priv = 102 // a private e2e channel created by alice

// peer is another user's device: its SSH key, device keys and cert.
type peer struct {
	user uint64
	ssh  ssh.Signer
	dev  *e2ecrypto.DeviceKeys
	cert proto.Signed
}

func newSSH(t *testing.T) ssh.Signer {
	t.Helper()
	_, pk, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// newPeer makes a device of user signed by key (a fresh key if nil).
func newPeer(t *testing.T, user uint64, key ssh.Signer) peer {
	t.Helper()
	if key == nil {
		key = newSSH(t)
	}
	d, err := e2ecrypto.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := identity.NewDeviceCert(key, testSID(), user, d, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return peer{user, key, d, proto.Signed(sc)}
}

func rcpt(ds ...*e2ecrypto.DeviceKeys) []identity.Recipient {
	var out []identity.Recipient
	for _, d := range ds {
		out = append(out, identity.Recipient{Dev: d.ID, X25519: d.X25519Pub})
	}
	return out
}

// mkBundle builds a valid bundle for ch at epoch on top of prev (nil for
// epoch 1) with key k (prevK for the backlink), signed by by.
func mkBundle(t *testing.T, by *e2ecrypto.DeviceKeys, ch uint64, epoch uint32, prev *proto.Signed,
	roster []uint64, k e2ecrypto.Key, prevK *e2ecrypto.Key, to ...*e2ecrypto.DeviceKeys) proto.Signed {
	t.Helper()
	p := identity.EpochParams{ServerID: testSID(), Chan: ch, Epoch: epoch, Roster: roster,
		Time: time.Now().UnixMilli(), K: k, PrevK: prevK, Recipients: rcpt(to...)}
	if prev != nil {
		p.Prev = e2ecrypto.ChainHash(prev.B)
	}
	if epoch > 1 && prevK == nil {
		pk := newK(t)
		p.PrevK = &pk
	}
	s, err := identity.NewEpochBundle(rand.Reader, by, p)
	if err != nil {
		t.Fatal(err)
	}
	return proto.Signed(s)
}

func newK(t *testing.T) e2ecrypto.Key {
	t.Helper()
	k, err := e2ecrypto.NewEpochKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// rawBundle mirrors identity's bundle b, for tampering.
type rawBox struct {
	Dev []byte `msgpack:"dev"`
	CT  []byte `msgpack:"ct"`
}

type rawBundle struct {
	V        uint32   `msgpack:"v"`
	ServerID []byte   `msgpack:"server_id"`
	Chan     uint64   `msgpack:"chan"`
	Epoch    uint32   `msgpack:"epoch"`
	Prev     []byte   `msgpack:"prev"`
	Roster   []uint64 `msgpack:"roster"`
	By       []byte   `msgpack:"by"`
	Time     int64    `msgpack:"time"`
	KC       []byte   `msgpack:"kc"`
	Eph      []byte   `msgpack:"eph"`
	Boxes    []rawBox `msgpack:"boxes"`
	Back     []byte   `msgpack:"back,omitempty"`
}

// retamper decodes s's b, lets mod change it and re-signs it with by.
func retamper(t *testing.T, s proto.Signed, by *e2ecrypto.DeviceKeys, mod func(*rawBundle)) proto.Signed {
	t.Helper()
	var w rawBundle
	if err := proto.Unmarshal(s.B, &w); err != nil {
		t.Fatal(err)
	}
	mod(&w)
	b := proto.MustRaw(w)
	sig, err := e2ecrypto.SignEd25519(by.Ed25519, identity.CtxEpoch, b)
	if err != nil {
		t.Fatal(err)
	}
	return proto.Signed{B: b, Sig: sig}
}

// ---- scripted fake server ----

type e2eFake struct {
	t  *testing.T
	fc *fakeConn

	mu        sync.Mutex
	seq       uint64
	devs      proto.DevicesListRes
	bundles   map[uint64]map[uint32]proto.EpochKey // key.fetch answers
	onRotate  func(f *e2eFake, id uint32, a proto.KeyRotateArgs)
	onSend    func(f *e2eFake, id uint32, a proto.SendArgs) // nil: store and answer (encmsg_test.go)
	searchRes proto.MsgsRes
	searched  atomic.Int32

	registered chan proto.Signed
	sends      chan proto.SendArgs
	rotates    chan proto.KeyRotateArgs
	shares     chan proto.KeyShareArgs
	needs      chan proto.Ephemeral
}

func (f *e2eFake) write(t proto.FrameType, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fc.codec.Write(t, v)
}

// event appends an event at the next seq and returns the seq.
func (f *e2eFake) event(kind string, ch, actor uint64, data any) uint64 {
	f.mu.Lock()
	f.seq++
	seq := f.seq
	f.fc.codec.Write(proto.TEvent, proto.Event{Seq: seq, Kind: kind, Chan: ch, Actor: actor, Time: 1, Data: proto.MustRaw(data)})
	f.mu.Unlock()
	return seq
}

// post appends a msg.new for m (its id = the event's seq) and returns the id.
func (f *e2eFake) post(m proto.Message) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	m.ID = f.seq
	if m.Time == 0 {
		m.Time = 1
	}
	f.fc.codec.Write(proto.TEvent, proto.Event{Seq: m.ID, Kind: proto.KMsgNew, Chan: m.Chan, Actor: m.Author, Time: 1, Data: proto.MustRaw(m)})
	return m.ID
}

// msgNew stores a send by author like a server would and returns the id.
func (f *e2eFake) msgNew(a proto.SendArgs, author uint64) uint64 {
	return f.post(proto.Message{Chan: a.Chan, Author: author, Body: a.Body, Thread: a.Thread, Enc: a.Enc})
}

func (f *e2eFake) setOnSend(fn func(f *e2eFake, id uint32, a proto.SendArgs)) {
	f.mu.Lock()
	f.onSend = fn
	f.mu.Unlock()
}

func (f *e2eFake) ok(id uint32, data any) {
	f.write(proto.TRes, proto.Res{ID: id, OK: true, Data: proto.MustRaw(data)})
}

// addBundle makes b (at event seq) available to key.fetch.
func (f *e2eFake) addBundle(ch uint64, b proto.Signed, seq uint64) {
	eb, err := identity.PeekEpochBundle(identity.Signed(b))
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bundles[ch] == nil {
		f.bundles[ch] = map[uint32]proto.EpochKey{}
	}
	f.bundles[ch][eb.Epoch] = proto.EpochKey{Bundle: b, Seq: seq}
}

// keyEpoch sends a key.epoch event for b and makes it fetchable.
func (f *e2eFake) keyEpoch(ch, actor uint64, b proto.Signed) uint64 {
	f.mu.Lock()
	seq := f.seq + 1
	f.mu.Unlock()
	f.addBundle(ch, b, seq)
	return f.event(proto.KKeyEpoch, ch, actor, proto.KeyEpoch{Bundle: b})
}

// startE2E accepts the client's connection, answers sync with snapshot
// (a reset at head 20) and then serves e2e requests in the background.
func startE2E(t *testing.T, fs *fakeServer, sn proto.Snapshot, devs proto.DevicesListRes) *e2eFake {
	t.Helper()
	fc := fs.accept()
	f := &e2eFake{t: t, fc: fc, seq: 20, devs: devs, bundles: map[uint64]map[uint32]proto.EpochKey{},
		registered: make(chan proto.Signed, 4), rotates: make(chan proto.KeyRotateArgs, 16), sends: make(chan proto.SendArgs, 16),
		shares: make(chan proto.KeyShareArgs, 16), needs: make(chan proto.Ephemeral, 64)}
	for _, k := range sn.Keys {
		f.addBundle(0, k.Bundle, k.Seq)
	}
	r := fc.expectReq(proto.OpSync, nil)
	if sn.Users == nil {
		sn.Users = append(users, proto.User{ID: carol, Name: "carol"})
	}
	f.write(proto.TSnapshot, sn)
	f.ok(r.ID, proto.SyncRes{Head: 20, Reset: true})
	go f.serve()
	return f
}

func (f *e2eFake) serve() {
	for fr := range f.fc.in {
		switch fr.Type {
		case proto.TEphemeral:
			var e proto.Ephemeral
			if fr.Decode(&e) == nil && e.Kind == proto.EphKeyNeed {
				select {
				case f.needs <- e:
				default:
				}
			}
		case proto.TReq:
			var r proto.Req
			if fr.Decode(&r) != nil {
				continue
			}
			switch r.Op {
			case proto.OpDevicesList:
				f.mu.Lock()
				res := f.devs
				f.mu.Unlock()
				f.ok(r.ID, res)
			case proto.OpDeviceRegister:
				var a proto.DeviceRegisterArgs
				proto.Unmarshal(r.Args, &a)
				seq := f.event(proto.KDeviceNew, 0, alice, a)
				f.ok(r.ID, proto.SeqRes{Seq: seq})
				select {
				case f.registered <- a.Cert:
				default:
				}
			case proto.OpKeyFetch:
				var a proto.KeyFetchArgs
				proto.Unmarshal(r.Args, &a)
				res := proto.KeyFetchRes{Bundles: []proto.Signed{}, Seqs: []uint64{}}
				f.mu.Lock()
				for e := a.From; e <= a.To && e >= a.From; e++ {
					if k, ok := f.bundles[a.Chan][e]; ok {
						res.Bundles = append(res.Bundles, k.Bundle)
						res.Seqs = append(res.Seqs, k.Seq)
					}
				}
				f.mu.Unlock()
				f.ok(r.ID, res)
			case proto.OpKeyRotate:
				var a proto.KeyRotateArgs
				proto.Unmarshal(r.Args, &a)
				select {
				case f.rotates <- a:
				default:
				}
				f.mu.Lock()
				on := f.onRotate
				f.mu.Unlock()
				if on != nil {
					on(f, r.ID, a)
				} else {
					seq := f.keyEpoch(a.Chan, alice, a.Bundle)
					f.ok(r.ID, proto.SeqRes{Seq: seq})
				}
			case proto.OpSearch:
				f.searched.Add(1)
				f.mu.Lock()
				res := f.searchRes
				f.mu.Unlock()
				f.ok(r.ID, res)
			case proto.OpSend:
				var a proto.SendArgs
				proto.Unmarshal(r.Args, &a)
				select {
				case f.sends <- a:
				default:
				}
				f.mu.Lock()
				on := f.onSend
				f.mu.Unlock()
				if on != nil {
					on(f, r.ID, a)
				} else {
					f.ok(r.ID, proto.SendRes{Msg: f.msgNew(a, alice)})
				}
			case proto.OpKeyShare:
				var a proto.KeyShareArgs
				proto.Unmarshal(r.Args, &a)
				f.event(proto.KKeyShare, a.Chan, alice, a)
				f.ok(r.ID, proto.Empty{})
				select {
				case f.shares <- a:
				default:
				}
			default:
				f.ok(r.ID, proto.Empty{})
			}
		}
	}
}

// e2eClient logs alice in against fs with cap e2e1 and starts her client.
func e2eClient(t *testing.T) (*fakeServer, *Client) {
	t.Helper()
	testEnv(t)
	fs := newFakeServer(t)
	fs.caps.Store([]string{proto.CapE2E})
	c := open(t, login(t, fs))
	c.timing.keyNeed = 200 * time.Millisecond
	return fs, c
}

// myKeys waits for alice's device registration and returns her device keys.
func (f *e2eFake) myKeys(c *Client) *e2ecrypto.DeviceKeys {
	f.t.Helper()
	select {
	case <-f.registered:
	case <-time.After(5 * time.Second):
		f.t.Fatal("device not registered")
	}
	k, err := c.Trust().DeviceKeys(testSID(), alice)
	if err != nil {
		f.t.Fatal(err)
	}
	waitFor(f.t, "own device verified", func() bool {
		_, ok, _ := c.Trust().Device(testSID(), k.ID)
		return ok
	})
	return k
}

func status(c *Client, ch uint64, want E2EState) func() bool {
	return func() bool { return c.E2EStatus(ch).State == want }
}

func dmChan(epoch uint32) proto.Channel {
	return proto.Channel{ID: dm, Kind: proto.ChanDM, Members: []uint64{alice, bob}, E2E: true, Epoch: epoch, Creator: bob}
}

func headOf(t *testing.T, c *Client, ch uint64) uint32 {
	t.Helper()
	h, _, err := c.Trust().ChainHead(testSID(), ch)
	if err != nil {
		t.Fatal(err)
	}
	return h.Epoch
}

// ---- device trust ----

// bob's first key is pinned on first sight; a second key endorsed by it is
// accepted; a third, unendorsed key is a pending key change; forged certs
// and revokes count for nothing. The auto-created epoch 1 of alice's
// channel seals only to the devices that verified.
func TestE2EDeviceTrust(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	b2 := newPeer(t, bob, nil)
	endorse, err := identity.NewKeyEndorse(b1.ssh, testSID(), bob, b2.ssh.PublicKey(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b3 := newPeer(t, bob, nil) // unendorsed key: a key change
	// Forged: claims b1's key, signed by b3's.
	forged := newPeer(t, bob, b3.ssh)
	var fw struct {
		V        uint32 `msgpack:"v"`
		ServerID []byte `msgpack:"server_id"`
		User     uint64 `msgpack:"user"`
		Dev      []byte `msgpack:"dev"`
		X25519   []byte `msgpack:"x25519"`
		Ed25519  []byte `msgpack:"ed25519"`
		SSH      []byte `msgpack:"ssh"`
		Created  int64  `msgpack:"created"`
	}
	proto.Unmarshal(forged.cert.B, &fw)
	fw.SSH = b1.ssh.PublicKey().Marshal()
	forged.cert.B = proto.MustRaw(fw)
	b4 := newPeer(t, bob, b1.ssh) // later revoked by b1's key
	b5 := newPeer(t, bob, b1.ssh) // "revoked" by a stranger's key
	realRevoke, _ := identity.NewDeviceRevoke(b1.ssh, testSID(), bob, b4.dev.ID, time.Now())
	fakeRevoke, _ := identity.NewDeviceRevoke(b3.ssh, testSID(), bob, b5.dev.ID, time.Now())
	c1 := newPeer(t, carol, nil)

	f := startE2E(t, fs, proto.Snapshot{
		Chans: []proto.Channel{{ID: priv, Name: "sec", Kind: proto.ChanPrivate, Members: []uint64{alice, bob}, E2E: true, Creator: alice}},
		Devs: []proto.Device{{Cert: b1.cert}, {Cert: b2.cert, Endorse: (*proto.Signed)(&endorse)}, {Cert: b3.cert},
			{Cert: forged.cert}, {Cert: b4.cert}, {Cert: b5.cert}, {Cert: c1.cert}},
		Revoked:     []proto.Signed{proto.Signed(realRevoke), proto.Signed(fakeRevoke)},
		RevokedSeqs: []uint64{15, 16},
	}, proto.DevicesListRes{})
	me := f.myKeys(c)

	sid := testSID()
	ts := c.Trust()
	var a proto.KeyRotateArgs
	select {
	case a = <-f.rotates:
	case <-time.After(5 * time.Second):
		t.Fatal("epoch 1 of our channel not created")
	}
	devs, _ := ts.Devices(sid, bob)
	got := map[e2ecrypto.DevID]trust.Device{}
	for _, d := range devs {
		got[d.ID] = d
	}
	for name, want := range map[string]struct {
		p                peer
		verified, revoke bool
	}{
		"first key (TOFU)": {b1, true, false}, "endorsed key": {b2, true, false},
		"key change": {b3, false, false}, "forged": {forged, false, false},
		"revoked": {b4, true, true}, "forged revoke": {b5, true, false},
	} {
		d, ok := got[want.p.dev.ID]
		if ok != want.verified || d.Revoked != want.revoke {
			t.Errorf("%s: verified=%v revoked=%v, want %v %v", name, ok, d.Revoked, want.verified, want.revoke)
		}
	}
	if d := got[b4.dev.ID]; d.RevokedSeq != 15 {
		t.Errorf("revoke seq %d, want 15", d.RevokedSeq)
	}
	pend, _ := ts.PendingKeyChanges(sid)
	if len(pend) != 1 || pend[0].User != bob || pend[0].Fingerprint != ssh.FingerprintSHA256(b3.ssh.PublicKey()) {
		t.Fatalf("pending key changes = %+v", pend)
	}
	if pins, _ := ts.PinnedKeys(sid, alice); len(pins) != 1 || pins[0].How != trust.Verified {
		t.Fatalf("own key pin = %+v", pins)
	}

	// Epoch 1: alice's device and bob's verified, live devices only.
	eb, err := identity.PeekEpochBundle(identity.Signed(a.Bundle))
	if err != nil || eb.Epoch != 1 || !slices.Equal(eb.Roster, []uint64{alice, bob}) {
		t.Fatalf("epoch 1 = %+v %v", eb, err)
	}
	boxed := eb.BoxDevs()
	slices.SortFunc(boxed, func(x, y e2ecrypto.DevID) int { return slices.Compare(x[:], y[:]) })
	want := []e2ecrypto.DevID{me.ID, b1.dev.ID, b2.dev.ID, b5.dev.ID}
	slices.SortFunc(want, func(x, y e2ecrypto.DevID) int { return slices.Compare(x[:], y[:]) })
	if !slices.Equal(boxed, want) {
		t.Fatalf("boxed %x, want %x", boxed, want)
	}
	waitFor(t, "channel ok", status(c, priv, E2EOK))
	// bob's device gets the same K.
	k1, _, _ := ts.Epoch(sid, priv, 1)
	for _, bx := range eb.Boxes {
		if bx.Dev == b1.dev.ID {
			k, err := e2ecrypto.OpenKey(b1.dev.X25519, eb.Eph, e2ecrypto.BoxAD{ServerID: sid, Chan: priv, Epoch: 1, Recipient: b1.dev.ID, By: me.ID}, bx.CT[:])
			if err != nil || k1.Key == nil || k != *k1.Key {
				t.Fatalf("bob's box: %v", err)
			}
		}
	}

	// Acknowledging the key change makes b3 a verified device.
	if err := ts.AcknowledgeKeyChange(sid, bob, pend[0].Fingerprint); err != nil {
		t.Fatal(err)
	}
	f.event(proto.KUserUpdate, 0, bob, users[1]) // any kick
	waitFor(t, "b3 verified after ack", func() bool { _, ok, _ := ts.Device(sid, b3.dev.ID); return ok })
}

// A member whose devices we can't verify blocks a rotation, by name.
func TestE2ERotateRefusesUnverifiedMember(t *testing.T) {
	fs, c := e2eClient(t)
	// bob's key was pinned before; now the server shows only a device under
	// another key.
	old := newSSH(t)
	if err := c.Trust().PinKey(testSID(), bob, ssh.FingerprintSHA256(old.PublicKey()), old.PublicKey().Marshal(), trust.TOFU); err != nil {
		t.Fatal(err)
	}
	run(t, c)
	b := newPeer(t, bob, nil)
	f := startE2E(t, fs, proto.Snapshot{
		Chans: []proto.Channel{dmChan(0)},
		Devs:  []proto.Device{{Cert: b.cert}},
	}, proto.DevicesListRes{})
	f.myKeys(c)
	waitFor(t, "online", online(c))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.RotateKey(ctx, dm)
	var ue *UnverifiedMembersError
	if !errors.As(err, &ue) || !slices.Equal(ue.Users, []uint64{bob}) || ue.Error() != "can't verify any device of: @bob" {
		t.Fatalf("rotate = %v", err)
	}
	select {
	case a := <-f.rotates:
		t.Fatalf("rotated anyway: %+v", a)
	default:
	}
}

// ---- epoch chain ----

// Each bad bundle raises an alarm and leaves the head where it was.
func TestE2EBundleAlarms(t *testing.T) {
	type setup struct {
		b1, c1 peer
		me     *e2ecrypto.DeviceKeys
		e1     proto.Signed
		k1     e2ecrypto.Key
	}
	cases := []struct {
		name string
		// chans for the snapshot, and the bad bundle(s) to send once the
		// client verified epoch 1 of the DM (head 1).
		bad      func(t *testing.T, s setup) []proto.Signed
		wantHead uint32
	}{
		{"bad signature", func(t *testing.T, s setup) []proto.Signed {
			e2 := mkBundle(t, s.b1.dev, dm, 2, &s.e1, []uint64{alice, bob}, newK(t), &s.k1, s.me, s.b1.dev)
			e2.Sig[3] ^= 1
			return []proto.Signed{e2}
		}, 1},
		{"wrong prev", func(t *testing.T, s setup) []proto.Signed {
			other := mkBundle(t, s.b1.dev, dm, 1, nil, []uint64{alice, bob}, newK(t), nil, s.me)
			return []proto.Signed{mkBundle(t, s.b1.dev, dm, 2, &other, []uint64{alice, bob}, newK(t), &s.k1, s.me)}
		}, 1},
		{"rotator not in previous roster", func(t *testing.T, s setup) []proto.Signed {
			return []proto.Signed{mkBundle(t, s.c1.dev, dm, 2, &s.e1, []uint64{alice, bob, carol}, newK(t), &s.k1, s.me, s.c1.dev)}
		}, 1},
		{"fork", func(t *testing.T, s setup) []proto.Signed {
			return []proto.Signed{mkBundle(t, s.b1.dev, dm, 1, nil, []uint64{alice, bob}, newK(t), nil, s.me)}
		}, 1},
		{"two valid next bundles", func(t *testing.T, s setup) []proto.Signed {
			return []proto.Signed{
				mkBundle(t, s.b1.dev, dm, 2, &s.e1, []uint64{alice, bob}, newK(t), &s.k1, s.me),
				mkBundle(t, s.b1.dev, dm, 2, &s.e1, []uint64{alice, bob}, newK(t), &s.k1, s.me),
			}
		}, 1},
		{"sealed to a non-member", func(t *testing.T, s setup) []proto.Signed {
			return []proto.Signed{mkBundle(t, s.b1.dev, dm, 2, &s.e1, []uint64{alice, bob}, newK(t), &s.k1, s.me, s.c1.dev)}
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, c := e2eClient(t)
			run(t, c)
			b1, c1 := newPeer(t, bob, nil), newPeer(t, carol, nil)
			f := startE2E(t, fs, proto.Snapshot{
				Chans: []proto.Channel{dmChan(0)},
				Devs:  []proto.Device{{Cert: b1.cert}, {Cert: c1.cert}},
			}, proto.DevicesListRes{})
			me := f.myKeys(c)
			k1 := newK(t)
			e1 := mkBundle(t, b1.dev, dm, 1, nil, []uint64{alice, bob}, k1, nil, me, b1.dev)
			f.keyEpoch(dm, bob, e1)
			waitFor(t, "epoch 1 ok", status(c, dm, E2EOK))
			for _, b := range tc.bad(t, setup{b1, c1, me, e1, k1}) {
				seq := f.event(proto.KKeyEpoch, dm, bob, proto.KeyEpoch{Bundle: b})
				f.addBundle(dm, b, seq)
			}
			waitFor(t, "alarm", status(c, dm, E2EAlarm))
			if h := headOf(t, c, dm); h != tc.wantHead {
				t.Fatalf("head moved to %d", h)
			}
			// Sticky: a good epoch 2 afterwards doesn't clear it.
			f.keyEpoch(dm, bob, mkBundle(t, b1.dev, dm, 2, &e1, []uint64{alice, bob}, newK(t), &k1, me))
			time.Sleep(100 * time.Millisecond)
			if st := c.E2EStatus(dm); st.State != E2EAlarm || headOf(t, c, dm) != tc.wantHead {
				t.Fatalf("after a good bundle: %+v head %d", st, headOf(t, c, dm))
			}
		})
	}
}

// Epoch 1 of a non-DM channel must come from its creator.
func TestE2EEpoch1NotByCreator(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	e1 := mkBundle(t, b1.dev, priv, 1, nil, []uint64{alice, bob}, newK(t), nil, b1.dev)
	f := startE2E(t, fs, proto.Snapshot{
		Chans: []proto.Channel{{ID: priv, Name: "sec", Kind: proto.ChanPrivate, Members: []uint64{alice, bob}, E2E: true, Epoch: 1, Creator: alice}},
		Devs:  []proto.Device{{Cert: b1.cert}},
		Keys:  []proto.EpochKey{{Bundle: e1, Seq: 12}},
	}, proto.DevicesListRes{})
	f.myKeys(c)
	waitFor(t, "alarm", status(c, priv, E2EAlarm))
	if st := c.E2EStatus(priv); headOf(t, c, priv) != 0 || st.Detail != "epoch 1: not created by the channel's creator" {
		t.Fatalf("status %+v head %d", st, headOf(t, c, priv))
	}
}

// A bundle signed by a device revoked before it is an alarm; one from
// before the revoke stays valid history.
func TestE2ERevokedRotator(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	b2 := newPeer(t, bob, b1.ssh)
	f := startE2E(t, fs, proto.Snapshot{
		Chans: []proto.Channel{dmChan(0)},
		Devs:  []proto.Device{{Cert: b1.cert}, {Cert: b2.cert}},
	}, proto.DevicesListRes{})
	me := f.myKeys(c)
	k1 := newK(t)
	e1 := mkBundle(t, b2.dev, dm, 1, nil, []uint64{alice, bob}, k1, nil, me, b1.dev, b2.dev)
	f.keyEpoch(dm, bob, e1)
	waitFor(t, "epoch 1 ok", status(c, dm, E2EOK))
	rev, _ := identity.NewDeviceRevokeByDevice(b1.dev, testSID(), bob, b2.dev.ID, time.Now())
	f.event(proto.KDeviceRevoke, 0, bob, proto.DeviceRevokeArgs{Revoke: proto.Signed(rev)})
	waitFor(t, "revoked", func() bool { d, _, _ := c.Trust().Device(testSID(), b2.dev.ID); return d.Revoked })
	f.keyEpoch(dm, bob, mkBundle(t, b2.dev, dm, 2, &e1, []uint64{alice, bob}, newK(t), &k1, me, b1.dev))
	waitFor(t, "alarm", status(c, dm, E2EAlarm))
	if st := c.E2EStatus(dm); headOf(t, c, dm) != 1 || st.Detail != "epoch 2: signed by a revoked device" {
		t.Fatalf("status %+v", st)
	}
}

// A server that shows an older epoch than the one we verified (after a
// forced reset) is rolling us back.
func TestE2ERollback(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)}, Devs: []proto.Device{{Cert: b1.cert}}}, proto.DevicesListRes{})
	me := f.myKeys(c)
	k1, k2 := newK(t), newK(t)
	e1 := mkBundle(t, b1.dev, dm, 1, nil, []uint64{alice, bob}, k1, nil, me)
	f.keyEpoch(dm, bob, e1)
	e2 := mkBundle(t, b1.dev, dm, 2, &e1, []uint64{alice, bob}, k2, &k1, me)
	f.keyEpoch(dm, bob, e2)
	waitFor(t, "epoch 2", func() bool { return headOf(t, c, dm) == 2 && c.E2EStatus(dm).State == E2EOK })

	f.fc.conn.Close()
	f2 := startE2E(t, fs, proto.Snapshot{
		Chans: []proto.Channel{dmChan(1)}, Devs: []proto.Device{{Cert: b1.cert}},
		Keys: []proto.EpochKey{{Bundle: e1, Seq: 5}},
	}, proto.DevicesListRes{})
	_ = f2
	waitFor(t, "alarm", status(c, dm, E2EAlarm))
	if headOf(t, c, dm) != 2 {
		t.Fatal("head rolled back")
	}
}

// ---- keys ----

// A box that doesn't match kc leaves the chain accepted but no key: the
// client asks with key.need and takes only a share that matches kc.
func TestE2EKCMismatchWaitsForShare(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)}, Devs: []proto.Device{{Cert: b1.cert}}}, proto.DevicesListRes{})
	me := f.myKeys(c)
	boxed, committed := newK(t), newK(t)
	e1 := mkBundle(t, b1.dev, dm, 1, nil, []uint64{alice, bob}, boxed, nil, me, b1.dev)
	e1 = retamper(t, e1, b1.dev, func(w *rawBundle) {
		kc := e2ecrypto.KeyCommitment(committed, testSID(), dm, 1)
		w.KC = kc[:]
	})
	f.keyEpoch(dm, bob, e1)
	var need proto.Ephemeral
	select {
	case need = <-f.needs:
	case <-time.After(5 * time.Second):
		t.Fatal("no key.need")
	}
	if need.Chan != dm || need.Epoch != 1 || e2ecrypto.DevID(need.Dev) != me.ID {
		t.Fatalf("key.need = %+v", need)
	}
	if st := c.E2EStatus(dm); st.State != E2EWaitingKey || st.Epoch != 1 {
		t.Fatalf("status %+v", st)
	}
	share := func(k e2ecrypto.Key) {
		eph, ct, err := e2ecrypto.SealShare(rand.Reader, me.X25519Pub, e2ecrypto.ShareAD{ServerID: testSID(), Chan: dm, Epoch: 1, Recipient: me.ID}, k)
		if err != nil {
			t.Fatal(err)
		}
		f.event(proto.KKeyShare, dm, bob, proto.KeyShareArgs{Chan: dm, Epoch: 1, Dev: me.ID[:], Eph: eph[:], CT: ct[:]})
	}
	share(boxed) // the K from the box: doesn't match kc either
	time.Sleep(150 * time.Millisecond)
	if st := c.E2EStatus(dm); st.State != E2EWaitingKey {
		t.Fatalf("bad share accepted: %+v", st)
	}
	// It keeps asking (rate limited).
	select {
	case <-f.needs:
	case <-time.After(5 * time.Second):
		t.Fatal("no repeated key.need")
	}
	share(committed)
	waitFor(t, "ok after a matching share", status(c, dm, E2EOK))
	if e, _, _ := c.Trust().Epoch(testSID(), dm, 1); e.Key == nil || *e.Key != committed {
		t.Fatal("wrong K stored")
	}
}

// Without a box in older epochs, backlinks from the newest K recover them.
func TestE2EBacklinksRecoverOlderKeys(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)}, Devs: []proto.Device{{Cert: b1.cert}}}, proto.DevicesListRes{})
	me := f.myKeys(c)
	k1, k2, k3 := newK(t), newK(t), newK(t)
	e1 := mkBundle(t, b1.dev, dm, 1, nil, []uint64{alice, bob}, k1, nil, b1.dev)
	e2 := mkBundle(t, b1.dev, dm, 2, &e1, []uint64{alice, bob}, k2, &k1, b1.dev)
	e3 := mkBundle(t, b1.dev, dm, 3, &e2, []uint64{alice, bob}, k3, &k2, b1.dev, me)
	f.addBundle(dm, e1, 30)
	f.addBundle(dm, e2, 31)
	// Only the head arrives (as after a snapshot); 1 and 2 come by key.fetch.
	f.keyEpoch(dm, bob, e3)
	waitFor(t, "ok", status(c, dm, E2EOK))
	waitFor(t, "older keys", func() bool { m, _ := c.Trust().MissingKeys(testSID(), dm); return len(m) == 0 })
	for e, want := range map[uint32]e2ecrypto.Key{1: k1, 2: k2, 3: k3} {
		got, ok, _ := c.Trust().Epoch(testSID(), dm, e)
		if !ok || got.Key == nil || *got.Key != want {
			t.Fatalf("epoch %d key not recovered", e)
		}
	}
	select {
	case n := <-f.needs:
		t.Fatalf("asked for a key it could recover: %+v", n)
	default:
	}
}

// key.need from a verified device of a roster member is answered with a
// share that opens to K; from anyone else it isn't.
func TestE2EAnswersKeyNeed(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1, b2 := newPeer(t, bob, nil), newPeer(t, bob, nil) // b2: unendorsed key
	c1 := newPeer(t, carol, nil)
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)},
		Devs: []proto.Device{{Cert: b1.cert}, {Cert: b2.cert}, {Cert: c1.cert}}}, proto.DevicesListRes{})
	me := f.myKeys(c)
	k1 := newK(t)
	f.keyEpoch(dm, bob, mkBundle(t, b1.dev, dm, 1, nil, []uint64{alice, bob}, k1, nil, me, b1.dev))
	waitFor(t, "ok", status(c, dm, E2EOK))

	need := func(user uint64, dev e2ecrypto.DevID, epoch uint32) {
		f.write(proto.TEphemeral, proto.Ephemeral{Kind: proto.EphKeyNeed, Chan: dm, User: user, Epoch: epoch, Dev: dev[:]})
	}
	need(carol, c1.dev.ID, 1) // verified device, not in the roster
	need(bob, b2.dev.ID, 1)   // in the roster, device not verified
	need(carol, b1.dev.ID, 1) // device of someone else than the asker
	need(bob, b1.dev.ID, 2)   // epoch we don't have
	select {
	case s := <-f.shares:
		t.Fatalf("answered a bad key.need: %+v", s)
	case <-time.After(300 * time.Millisecond):
	}
	need(bob, b1.dev.ID, 1)
	var s proto.KeyShareArgs
	select {
	case s = <-f.shares:
	case <-time.After(5 * time.Second):
		t.Fatal("no key.share")
	}
	k, err := e2ecrypto.OpenShare(b1.dev.X25519, [32]byte(s.Eph), e2ecrypto.ShareAD{ServerID: testSID(), Chan: dm, Epoch: 1, Recipient: b1.dev.ID}, s.CT)
	if err != nil || k != k1 || s.Epoch != 1 || e2ecrypto.DevID(s.Dev) != b1.dev.ID {
		t.Fatalf("share: %v", err)
	}
	// Not twice in a row.
	need(bob, b1.dev.ID, 1)
	select {
	case s := <-f.shares:
		t.Fatalf("answered again at once: %+v", s)
	case <-time.After(100 * time.Millisecond):
	}
}

// RotateKey builds on the verified head; a stale_epoch (bob rotated first)
// is retried once on top of bob's epoch.
func TestE2ERotateStaleRetry(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)}, Devs: []proto.Device{{Cert: b1.cert}}}, proto.DevicesListRes{})
	me := f.myKeys(c)
	k1, k2 := newK(t), newK(t)
	e1 := mkBundle(t, b1.dev, dm, 1, nil, []uint64{alice, bob}, k1, nil, me, b1.dev)
	f.keyEpoch(dm, bob, e1)
	waitFor(t, "ok", status(c, dm, E2EOK))
	e2 := mkBundle(t, b1.dev, dm, 2, &e1, []uint64{alice, bob}, k2, &k1, me, b1.dev)
	f.mu.Lock()
	f.onRotate = func(f *e2eFake, id uint32, a proto.KeyRotateArgs) {
		eb, _ := identity.PeekEpochBundle(identity.Signed(a.Bundle))
		if eb.Epoch == 2 { // lose the race: bob's epoch 2 first, then stale
			f.keyEpoch(dm, bob, e2)
			f.write(proto.TRes, proto.Res{ID: id, Err: proto.Errf(proto.ErrStaleEpoch, "channel is at epoch 2")})
			return
		}
		seq := f.keyEpoch(a.Chan, alice, a.Bundle)
		f.ok(id, proto.SeqRes{Seq: seq})
	}
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.RotateKey(ctx, dm); err != nil {
		t.Fatal(err)
	}
	<-f.rotates
	a := <-f.rotates
	eb, err := identity.Signed(a.Bundle).VerifyEpochBundle(testSID(), me.Ed25519Pub)
	if err != nil || eb.Epoch != 3 || eb.Prev != e2ecrypto.ChainHash(e2.B) || eb.By != me.ID {
		t.Fatalf("retry bundle %+v %v", eb, err)
	}
	// Its backlink is bob's K2, and bob can open his box.
	k3, ok, _ := c.Trust().Epoch(testSID(), dm, 3)
	if !ok || k3.Key == nil {
		t.Fatal("own epoch 3 not verified")
	}
	if back, err := e2ecrypto.OpenBack(*k3.Key, testSID(), dm, 3, eb.Back[:]); err != nil || back != k2 {
		t.Fatalf("backlink: %v", err)
	}
	if st := c.E2EStatus(dm); st.State != E2EOK || st.Epoch != 3 {
		t.Fatalf("status %+v", st)
	}
	// A second stale_epoch is not retried forever.
	f.mu.Lock()
	f.onRotate = func(f *e2eFake, id uint32, a proto.KeyRotateArgs) {
		f.write(proto.TRes, proto.Res{ID: id, Err: proto.Errf(proto.ErrStaleEpoch, "nope")})
	}
	f.mu.Unlock()
	var pe *proto.Error
	if err := c.RotateKey(ctx, dm); !errors.As(err, &pe) || pe.Code != proto.ErrStaleEpoch {
		t.Fatalf("double stale: %v", err)
	}
	if n := len(f.rotates); n != 2 {
		t.Fatalf("%d attempts, want 2", n)
	}
}

// A bundle is invalid if EITHER the server's seq order OR its own signed
// time puts it after a signed revoke of its rotator. The server controls seqs,
// so a revoke it orders after the bundle must not rescue a bundle that the
// signed times show was made after the revoke.
func TestE2EBundleAfterRevokeBySignedTime(t *testing.T) {
	const revokeSeqAfterBundle, revokeSeqBeforeBundle = 30, 10 // the bundle's key.epoch gets seq 21
	cases := []struct {
		name      string
		revokedAt time.Duration // revoke's signed time relative to the bundle's
		revokeSeq uint64
		wantOK    bool
		detail    string
	}{
		{"server misorders seqs, signed times say after the revoke", -time.Hour, revokeSeqAfterBundle, false,
			"epoch 1: signed after its device was revoked"},
		{"seq says after the revoke, signed times don't", time.Hour, revokeSeqBeforeBundle, false,
			"epoch 1: signed by a revoked device"},
		{"seq and signed time both before the revoke", time.Hour, revokeSeqAfterBundle, true, ""},
		// Seq 0 = unknown: it must not count as "before everything"; only the time rule applies.
		{"unknown seq, signed time after the bundle", time.Hour, 0, true, ""},
		{"unknown seq, signed time before the bundle", -time.Hour, 0, false,
			"epoch 1: signed after its device was revoked"},
		// Clocks differ: a bundle signed up to 5 min after the revoke's time still passes the time rule.
		{"bundle signed 4 min after the revoke (skew)", -4 * time.Minute, revokeSeqAfterBundle, true, ""},
		{"bundle signed 6 min after the revoke", -6 * time.Minute, revokeSeqAfterBundle, false,
			"epoch 1: signed after its device was revoked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, c := e2eClient(t)
			run(t, c)
			b1 := newPeer(t, bob, nil)
			b2 := newPeer(t, bob, b1.ssh)
			rev, err := identity.NewDeviceRevoke(b1.ssh, testSID(), bob, b2.dev.ID, time.Now().Add(tc.revokedAt))
			if err != nil {
				t.Fatal(err)
			}
			f := startE2E(t, fs, proto.Snapshot{
				Chans:       []proto.Channel{dmChan(0)},
				Devs:        []proto.Device{{Cert: b1.cert}, {Cert: b2.cert}},
				Revoked:     []proto.Signed{proto.Signed(rev)},
				RevokedSeqs: []uint64{tc.revokeSeq},
			}, proto.DevicesListRes{})
			me := f.myKeys(c)
			waitFor(t, "revoke known", func() bool { d, _, _ := c.Trust().Device(testSID(), b2.dev.ID); return d.Revoked })
			f.keyEpoch(dm, bob, mkBundle(t, b2.dev, dm, 1, nil, []uint64{alice, bob}, newK(t), nil, me, b1.dev, b2.dev))
			if tc.wantOK {
				waitFor(t, "epoch 1 ok", status(c, dm, E2EOK))
				return
			}
			waitFor(t, "alarm", status(c, dm, E2EAlarm))
			if st := c.E2EStatus(dm); headOf(t, c, dm) != 0 || st.Detail != tc.detail {
				t.Fatalf("status %+v head %d, want alarm %q and no head", st, headOf(t, c, dm), tc.detail)
			}
		})
	}
}

// Members of my e2e channels whose keys are only TOFU-pinned (or endorsed
// by a TOFU key) are reported as unverified; verified ones, myself, members
// of plaintext channels and users with no devices aren't.
func TestE2EUnverifiedUsers(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	c1 := newPeer(t, carol, nil)
	const dave = 4
	f := startE2E(t, fs, proto.Snapshot{
		Users: append(users, proto.User{ID: carol, Name: "carol"}, proto.User{ID: dave, Name: "dave"}),
		Chans: []proto.Channel{dmChan(0),
			{ID: priv, Name: "sec", Kind: proto.ChanPrivate, Members: []uint64{alice, carol, dave}, E2E: true, Creator: carol},
			{ID: 103, Name: "plain", Kind: proto.ChanPrivate, Members: []uint64{alice, 5}}},
		Devs: []proto.Device{{Cert: b1.cert}, {Cert: c1.cert}, {Cert: newPeer(t, 5, nil).cert}},
	}, proto.DevicesListRes{})
	f.myKeys(c)
	waitFor(t, "bob and carol pinned", func() bool { return slices.Equal(c.UnverifiedUsers(), []uint64{bob, carol}) })
	fp := ssh.FingerprintSHA256(c1.ssh.PublicKey())
	if err := c.Trust().PinKey(testSID(), carol, fp, c1.ssh.PublicKey().Marshal(), trust.Verified); err != nil {
		t.Fatal(err)
	}
	if got := c.UnverifiedUsers(); !slices.Equal(got, []uint64{bob}) {
		t.Fatalf("after verifying carol: %v", got)
	}
}

// A flood of junk key.share events (another member's, for epochs we haven't
// verified, or for other devices) never pushes out a legitimate share.
func TestE2EShareFloodKeepsLegitShare(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	group := proto.Channel{ID: dm, Kind: proto.ChanDM, Members: []uint64{alice, bob, carol}, E2E: true, Creator: bob}
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{group}, Devs: []proto.Device{{Cert: b1.cert}}}, proto.DevicesListRes{})
	me := f.myKeys(c)
	k1 := newK(t)
	f.keyEpoch(dm, bob, mkBundle(t, b1.dev, dm, 1, nil, []uint64{alice, bob, carol}, k1, nil, b1.dev)) // no box for us
	waitFor(t, "waiting for key", status(c, dm, E2EWaitingKey))
	junk := func(epoch uint32, dev []byte) proto.KeyShareArgs {
		return proto.KeyShareArgs{Chan: dm, Epoch: epoch, Dev: dev, Eph: make([]byte, 32), CT: make([]byte, e2ecrypto.BoxCTSize)}
	}
	for i := range uint32(maxCachedShares + 50) {
		f.event(proto.KKeyShare, dm, carol, junk(i+2, me.ID[:]))
		other := e2ecrypto.DevID{byte(i), byte(i >> 8), 1}
		f.event(proto.KKeyShare, dm, carol, junk(1, other[:]))
	}
	eph, ct, err := e2ecrypto.SealShare(rand.Reader, me.X25519Pub, e2ecrypto.ShareAD{ServerID: testSID(), Chan: dm, Epoch: 1, Recipient: me.ID}, k1)
	if err != nil {
		t.Fatal(err)
	}
	f.event(proto.KKeyShare, dm, bob, proto.KeyShareArgs{Chan: dm, Epoch: 1, Dev: me.ID[:], Eph: eph[:], CT: ct[:]})
	waitFor(t, "ok from bob's share", status(c, dm, E2EOK))
	if n := count(t, c, "key_shares"); n > maxCachedShares {
		t.Fatalf("%d shares cached, cap %d", n, maxCachedShares)
	}
}

// Unverified bundles and shares are cached only for channels known as
// encrypted that we're a member of, at most maxEpochAhead above the head
// and under a global cap; they never raise the cached channel epoch.
func TestE2EBundleCacheBounds(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	stranger := newPeer(t, bob, nil) // never verified: its bundles can't be judged
	const plain, notMine = 103, 104
	f := startE2E(t, fs, proto.Snapshot{
		Chans: []proto.Channel{dmChan(0),
			{ID: plain, Name: "plain", Kind: proto.ChanPrivate, Members: []uint64{alice, bob}, Creator: bob},
			{ID: notMine, Name: "theirs", Kind: proto.ChanPrivate, Members: []uint64{bob, carol}, E2E: true, Creator: bob}},
		Devs: []proto.Device{{Cert: b1.cert}},
	}, proto.DevicesListRes{})
	me := f.myKeys(c)
	cached := func(ch uint64) (n int) {
		c.db.QueryRow(`SELECT COUNT(*) FROM epochs WHERE chan = ?`, ch).Scan(&n)
		return n
	}
	for _, ch := range []uint64{plain, notMine} {
		f.event(proto.KKeyEpoch, ch, bob, proto.KeyEpoch{Bundle: mkBundle(t, b1.dev, ch, 1, nil, []uint64{alice, bob}, newK(t), nil, me)})
		f.event(proto.KKeyShare, ch, bob, proto.KeyShareArgs{Chan: ch, Epoch: 1, Dev: me.ID[:], Eph: make([]byte, 32), CT: make([]byte, e2ecrypto.BoxCTSize)})
	}
	far := mkBundle(t, stranger.dev, dm, maxEpochAhead+1, nil, []uint64{alice, bob}, newK(t), nil, me)
	near := mkBundle(t, stranger.dev, dm, maxEpochAhead, nil, []uint64{alice, bob}, newK(t), nil, me)
	f.event(proto.KKeyEpoch, dm, bob, proto.KeyEpoch{Bundle: far})
	f.event(proto.KKeyEpoch, dm, bob, proto.KeyEpoch{Bundle: near})
	waitFor(t, "near bundle cached", func() bool {
		var n int
		c.db.QueryRow(`SELECT COUNT(*) FROM epochs WHERE chan = ? AND epoch = ?`, dm, maxEpochAhead).Scan(&n)
		return n == 1
	})
	if cached(dm) != 1 {
		t.Fatalf("%d bundles cached for the DM: epoch %d (> head + %d) kept", cached(dm), maxEpochAhead+1, maxEpochAhead)
	}
	if cached(plain) != 0 || cached(notMine) != 0 || count(t, c, "key_shares") != 0 {
		t.Fatalf("cached for plain %d, non-member %d, shares %d", cached(plain), cached(notMine), count(t, c, "key_shares"))
	}
	if cv, _ := c.Channel(dm); cv.Epoch != 0 {
		t.Fatalf("unverified bundle raised the channel epoch to %d", cv.Epoch)
	}

	// Global cap: with the cache full of other channels' bundles, nothing more is kept.
	for i := range maxCachedBundlesAll {
		if _, err := c.db.Exec(`INSERT INTO epochs (chan, epoch, hash, bundle, seq) VALUES (?, 1, ?, x'00', 1)`,
			1000+i/maxCachedBundles, []byte{byte(i), byte(i >> 8)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.tx(func(tx *sql.Tx) error {
		return c.storeBundle(tx, dm, mkBundle(t, stranger.dev, dm, 3, nil, []uint64{alice, bob}, newK(t), nil, me), 99)
	}); err != nil {
		t.Fatal(err)
	}
	if cached(dm) != 1 {
		t.Fatalf("stored over the global cap: %d", cached(dm))
	}
}

// A forged cert is checked once, not on every pass.
func TestE2EForgedCertNegativeCache(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1 := newPeer(t, bob, nil)
	forged := newPeer(t, carol, nil)
	forged.cert.Sig[5] ^= 1
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)},
		Devs: []proto.Device{{Cert: b1.cert}, {Cert: forged.cert}}}, proto.DevicesListRes{})
	f.myKeys(c)
	time.Sleep(300 * time.Millisecond) // passes run every timing.keyNeed (200 ms)
	n := c.certVerifies.Load()
	time.Sleep(700 * time.Millisecond)
	if got := c.certVerifies.Load(); got != n {
		t.Fatalf("%d more cert checks over idle passes", got-n)
	}
}

// key.need for devices we don't know triggers device verification at most
// once per needVerifyEvery.
func TestE2EKeyNeedVerifyThrottled(t *testing.T) {
	fs, c := e2eClient(t)
	c.timing.keyNeed = time.Hour // no periodic passes
	run(t, c)
	b1 := newPeer(t, bob, nil)
	b3 := newPeer(t, bob, nil) // unendorsed: checked on every verification run
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)},
		Devs: []proto.Device{{Cert: b1.cert}, {Cert: b3.cert}}}, proto.DevicesListRes{})
	f.myKeys(c)
	waitFor(t, "key change pending", func() bool { p, _ := c.Trust().PendingKeyChanges(testSID()); return len(p) == 1 })
	time.Sleep(200 * time.Millisecond)
	n := c.certVerifies.Load()
	for i := range 5 {
		unknown := e2ecrypto.DevID{0xee, byte(i)}
		f.write(proto.TEphemeral, proto.Ephemeral{Kind: proto.EphKeyNeed, Chan: dm, User: bob, Epoch: 1, Dev: unknown[:]})
	}
	time.Sleep(300 * time.Millisecond)
	if got := c.certVerifies.Load() - n; got > 1 {
		t.Fatalf("%d verification runs for 5 key.need, want ≤ 1", got)
	}
}

// A key.need from someone in the epoch's roster who is no longer a member
// isn't answered.
func TestE2EKeyNeedFormerMember(t *testing.T) {
	fs, c := e2eClient(t)
	run(t, c)
	b1, c1 := newPeer(t, bob, nil), newPeer(t, carol, nil)
	group := proto.Channel{ID: dm, Kind: proto.ChanDM, Members: []uint64{alice, bob, carol}, E2E: true, Creator: bob}
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{group},
		Devs: []proto.Device{{Cert: b1.cert}, {Cert: c1.cert}}}, proto.DevicesListRes{})
	me := f.myKeys(c)
	f.keyEpoch(dm, bob, mkBundle(t, b1.dev, dm, 1, nil, []uint64{alice, bob, carol}, newK(t), nil, me, b1.dev))
	waitFor(t, "ok", status(c, dm, E2EOK))
	f.event(proto.KChanLeave, dm, carol, proto.UserRef{User: carol})
	waitFor(t, "carol left", func() bool { cv, _ := c.Channel(dm); return !slices.Contains(cv.Members, carol) })
	f.write(proto.TEphemeral, proto.Ephemeral{Kind: proto.EphKeyNeed, Chan: dm, User: carol, Epoch: 1, Dev: c1.dev.ID[:]})
	select {
	case s := <-f.shares:
		t.Fatalf("answered a former member: %+v", s)
	case <-time.After(300 * time.Millisecond):
	}
	// A current roster member still is.
	f.write(proto.TEphemeral, proto.Ephemeral{Kind: proto.EphKeyNeed, Chan: dm, User: bob, Epoch: 1, Dev: b1.dev.ID[:]})
	select {
	case <-f.shares:
	case <-time.After(5 * time.Second):
		t.Fatal("current member not answered")
	}
}

// A verified bundle stays cached until we have its K: without device keys
// (or with a box that doesn't open) the head moves but the bundle is kept,
// and a later pass with our keys opens our box from it.
func TestE2EBundleKeptUntilKey(t *testing.T) {
	fs, c := e2eClient(t)
	c.timing.keyNeed = time.Hour // no periodic passes: we drive syncChannel
	run(t, c)
	b1 := newPeer(t, bob, nil)
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)}, Devs: []proto.Device{{Cert: b1.cert}}}, proto.DevicesListRes{})
	me := f.myKeys(c)
	waitFor(t, "online", online(c))
	k1 := newK(t)
	e1 := mkBundle(t, b1.dev, dm, 1, nil, []uint64{alice, bob}, k1, nil, me, b1.dev)
	// Hold e2eMu throughout so the real worker can't take the bundle in between.
	c.e2eMu.Lock()
	defer c.e2eMu.Unlock()
	if err := c.tx(func(tx *sql.Tx) error { return c.storeBundle(tx, dm, e1, 30) }); err != nil {
		t.Fatal(err)
	}
	sid := testSID()
	ch, _, _ := getChan(c.db, dm)
	noKeys := &session{welcome: proto.Welcome{ServerID: serverID}, done: make(chan struct{}), devReady: make(chan struct{})}
	pass := func(s *session) { c.syncChannel(context.Background(), s, sid, ch) }
	pass(noKeys)
	if headOf(t, c, dm) != 1 {
		t.Fatal("head didn't move")
	}
	if ep, _, _ := c.Trust().Epoch(sid, dm, 1); ep.Key != nil {
		t.Fatal("K without device keys?")
	}
	if n := count(t, c, "epochs"); n != 1 {
		t.Fatalf("bundle dropped before we had K (%d cached)", n)
	}
	withKeys := &session{welcome: proto.Welcome{ServerID: serverID}, done: make(chan struct{}), devReady: make(chan struct{})}
	withKeys.dev.Store(me)
	pass(withKeys)
	if ep, _, _ := c.Trust().Epoch(sid, dm, 1); ep.Key == nil || *ep.Key != k1 {
		t.Fatal("K not recovered from the kept bundle")
	}
	pass(withKeys)
	if n := count(t, c, "epochs"); n != 0 {
		t.Fatalf("bundle kept after K was obtained (%d cached)", n)
	}
}

// Names in UnverifiedMembersError come from the server and are sanitized.
func TestE2EUnverifiedMembersErrorSanitized(t *testing.T) {
	fs, c := e2eClient(t)
	old := newSSH(t)
	if err := c.Trust().PinKey(testSID(), bob, ssh.FingerprintSHA256(old.PublicKey()), old.PublicKey().Marshal(), trust.TOFU); err != nil {
		t.Fatal(err)
	}
	run(t, c)
	b := newPeer(t, bob, nil)
	evil := "bob\x1b]52;c;cHduZWQ=\x07\x1b[2J\n"
	f := startE2E(t, fs, proto.Snapshot{
		Users: []proto.User{users[0], {ID: bob, Name: evil}},
		Chans: []proto.Channel{dmChan(0)},
		Devs:  []proto.Device{{Cert: b.cert}},
	}, proto.DevicesListRes{})
	f.myKeys(c)
	waitFor(t, "online", online(c))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ue *UnverifiedMembersError
	if err := c.RotateKey(ctx, dm); !errors.As(err, &ue) {
		t.Fatalf("rotate = %v", err)
	}
	msg := ue.Error()
	if strings.ContainsAny(msg, "\x1b\x07\n") || msg != "can't verify any device of: @"+sanitize.Line(evil) {
		t.Fatalf("unsanitized: %q", msg)
	}
}
