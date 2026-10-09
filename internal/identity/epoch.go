package identity

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// Epoch bundles (PROTOCOL.md §13.3): the signed, hash-chained roster of an
// encrypted channel plus its epoch key K sealed to every recipient device.

// CtxEpoch is the Ed25519 context of an EpochBundle signature.
const CtxEpoch = e2ecrypto.CtxEpoch

// Bundle limits (§13.8).
const (
	MaxBundleSize  = e2ecrypto.MaxBundleSize // len(b)+len(sig)
	MaxBundleBoxes = e2ecrypto.MaxBundleBoxes
)

var (
	// ErrDuplicateBox means two boxes go to the same device.
	ErrDuplicateBox = errors.New("identity: bundle seals twice to one device")
	// ErrDuplicateKey means two boxes go to devices with the same X25519 key.
	// One ephemeral key and a zero nonce per box: a duplicate would reuse key and nonce.
	ErrDuplicateKey = errors.New("identity: bundle seals twice to one X25519 key")
	// ErrUnknownRecipient means a box goes to a device the caller doesn't know.
	ErrUnknownRecipient = errors.New("identity: bundle seals to an unknown device")
)

// EpochBox is K sealed to one device.
type EpochBox struct {
	Dev e2ecrypto.DevID
	CT  [e2ecrypto.BoxCTSize]byte
}

// EpochBundle is a decoded bundle b. Time is Unix ms.
type EpochBundle struct {
	ServerID e2ecrypto.ServerID
	Chan     uint64
	Epoch    uint32   // first = 1
	Prev     [32]byte // ChainHash of the previous bundle's b; zero for epoch 1
	Roster   []uint64 // sorted, distinct
	By       e2ecrypto.DevID
	Time     int64
	KC       [32]byte
	Eph      [32]byte
	Boxes    []EpochBox
	Back     *[e2ecrypto.BackSize]byte // K_{epoch-1} under K; never on epoch 1
}

type epochBoxB struct {
	Dev []byte `msgpack:"dev"`
	CT  []byte `msgpack:"ct"`
}

type epochBundleB struct {
	V        uint32      `msgpack:"v"`
	ServerID []byte      `msgpack:"server_id"`
	Chan     uint64      `msgpack:"chan"`
	Epoch    uint32      `msgpack:"epoch"`
	Prev     []byte      `msgpack:"prev"`
	Roster   []uint64    `msgpack:"roster"`
	By       []byte      `msgpack:"by"`
	Time     int64       `msgpack:"time"`
	KC       []byte      `msgpack:"kc"`
	Eph      []byte      `msgpack:"eph"`
	Boxes    []epochBoxB `msgpack:"boxes"`
	Back     []byte      `msgpack:"back,omitempty"`
}

// Recipient is a device K is sealed to.
type Recipient struct {
	Dev    e2ecrypto.DevID
	X25519 [32]byte
}

// EpochParams describe a new bundle. PrevK (for the backlink) is required
// when Epoch > 1 and must be nil for epoch 1.
type EpochParams struct {
	ServerID   e2ecrypto.ServerID
	Chan       uint64
	Epoch      uint32
	Prev       [32]byte
	Roster     []uint64
	Time       int64
	K          e2ecrypto.Key
	PrevK      *e2ecrypto.Key
	Recipients []Recipient
}

// NewEpochBundle seals p.K to every recipient under one fresh ephemeral key
// and signs the bundle with the rotator device by.
func NewEpochBundle(rand io.Reader, by *e2ecrypto.DeviceKeys, p EpochParams) (Signed, error) {
	if by == nil {
		return Signed{}, e2ecrypto.ErrBadKey
	}
	if p.Epoch == 0 || len(p.Recipients) == 0 || len(p.Recipients) > MaxBundleBoxes || (p.Epoch > 1) != (p.PrevK != nil) {
		return Signed{}, e2ecrypto.ErrMalformed
	}
	keys := map[e2ecrypto.DevID][32]byte{}
	for _, r := range p.Recipients {
		keys[r.Dev] = r.X25519
	}
	eph, err := e2ecrypto.NewEphemeral(rand)
	if err != nil {
		return Signed{}, err
	}
	kc := e2ecrypto.KeyCommitment(p.K, p.ServerID, p.Chan, p.Epoch)
	w := epochBundleB{
		V: signedVersion, ServerID: p.ServerID[:], Chan: p.Chan, Epoch: p.Epoch, Prev: p.Prev[:],
		Roster: slices.Clone(p.Roster), By: by.ID[:], Time: p.Time, KC: kc[:], Eph: eph.PublicKey().Bytes(),
	}
	slices.Sort(w.Roster)
	w.Roster = slices.Compact(w.Roster)
	for _, r := range p.Recipients {
		ct, err := e2ecrypto.SealKey(eph, r.X25519, e2ecrypto.BoxAD{
			ServerID: p.ServerID, Chan: p.Chan, Epoch: p.Epoch, Recipient: r.Dev, By: by.ID,
		}, p.K)
		if err != nil {
			return Signed{}, err
		}
		w.Boxes = append(w.Boxes, epochBoxB{Dev: r.Dev[:], CT: ct[:]})
	}
	if p.PrevK != nil {
		back, err := e2ecrypto.SealBack(rand, p.K, *p.PrevK, p.ServerID, p.Chan, p.Epoch)
		if err != nil {
			return Signed{}, err
		}
		w.Back = back[:]
	}
	b, err := proto.Marshal(w)
	if err != nil {
		return Signed{}, fmt.Errorf("encoding epoch bundle: %w", err)
	}
	// Decode what we built, so a bundle we sign always passes our own checks.
	eb, err := decodeEpochBundle(b)
	if err != nil {
		return Signed{}, err
	}
	if err := eb.CheckBoxKeys(keys); err != nil {
		return Signed{}, err
	}
	sig, err := e2ecrypto.SignEd25519(by.Ed25519, CtxEpoch, b)
	if err != nil {
		return Signed{}, err
	}
	if len(b)+len(sig) > MaxBundleSize {
		return Signed{}, e2ecrypto.ErrTooLarge
	}
	return Signed{B: b, Sig: sig}, nil
}

// VerifyEpochBundle checks s as signed by the Ed25519 key of device by, then
// decodes it. The caller must check that by is the bundle's By and a device
// it trusts to rotate, and everything else about the chain.
func (s Signed) VerifyEpochBundle(serverID e2ecrypto.ServerID, by ed25519.PublicKey) (EpochBundle, error) {
	if len(s.B)+len(s.Sig) > MaxBundleSize {
		return EpochBundle{}, e2ecrypto.ErrTooLarge
	}
	if !e2ecrypto.VerifyEd25519(by, CtxEpoch, s.B, s.Sig) {
		return EpochBundle{}, e2ecrypto.ErrBadSignature
	}
	eb, err := decodeEpochBundle(s.B)
	if err != nil {
		return EpochBundle{}, err
	}
	if eb.ServerID != serverID {
		return EpochBundle{}, ErrWrongServer
	}
	return eb, nil
}

// PeekEpochBundle decodes an unverified bundle (see the Peek note in certs.go).
func PeekEpochBundle(s Signed) (EpochBundle, error) {
	if len(s.B)+len(s.Sig) > MaxBundleSize {
		return EpochBundle{}, e2ecrypto.ErrTooLarge
	}
	return decodeEpochBundle(s.B)
}

// CheckBoxKeys checks that every box goes to a device in keys (dev → X25519
// public key) and that no two boxes share an X25519 key.
func (eb EpochBundle) CheckBoxKeys(keys map[e2ecrypto.DevID][32]byte) error {
	seen := make(map[[32]byte]bool, len(eb.Boxes))
	for _, bx := range eb.Boxes {
		k, ok := keys[bx.Dev]
		if !ok {
			return ErrUnknownRecipient
		}
		if seen[k] {
			return ErrDuplicateKey
		}
		seen[k] = true
	}
	return nil
}

// BoxDevs returns the recipient devices in box order.
func (eb EpochBundle) BoxDevs() []e2ecrypto.DevID {
	out := make([]e2ecrypto.DevID, len(eb.Boxes))
	for i, bx := range eb.Boxes {
		out[i] = bx.Dev
	}
	return out
}

func decodeEpochBundle(b []byte) (EpochBundle, error) {
	var w epochBundleB
	if err := decodeB(b, &w, &w.V); err != nil {
		return EpochBundle{}, err
	}
	if len(w.Boxes) > MaxBundleBoxes {
		return EpochBundle{}, e2ecrypto.ErrTooLarge
	}
	eb := EpochBundle{Chan: w.Chan, Epoch: w.Epoch, Roster: w.Roster, Time: w.Time}
	if !copyFixed(eb.ServerID[:], w.ServerID) || !copyFixed(eb.Prev[:], w.Prev) || !copyFixed(eb.By[:], w.By) ||
		!copyFixed(eb.KC[:], w.KC) || !copyFixed(eb.Eph[:], w.Eph) {
		return EpochBundle{}, e2ecrypto.ErrMalformed
	}
	if eb.Epoch == 0 || len(eb.Roster) == 0 || len(w.Boxes) == 0 {
		return EpochBundle{}, e2ecrypto.ErrMalformed
	}
	// Every e2e channel shares history (docs/E2E.md): epoch n > 1 always
	// links back to K_{n-1}, and epoch 1 has nothing to link to.
	if eb.Epoch == 1 && (eb.Prev != [32]byte{} || w.Back != nil) || eb.Epoch > 1 && w.Back == nil {
		return EpochBundle{}, e2ecrypto.ErrMalformed
	}
	for i := 1; i < len(eb.Roster); i++ {
		if eb.Roster[i] <= eb.Roster[i-1] {
			return EpochBundle{}, e2ecrypto.ErrMalformed
		}
	}
	if w.Back != nil {
		eb.Back = new([e2ecrypto.BackSize]byte)
		if !copyFixed(eb.Back[:], w.Back) {
			return EpochBundle{}, e2ecrypto.ErrMalformed
		}
	}
	devs := make(map[e2ecrypto.DevID]bool, len(w.Boxes))
	eb.Boxes = make([]EpochBox, len(w.Boxes))
	for i, bx := range w.Boxes {
		if !copyFixed(eb.Boxes[i].Dev[:], bx.Dev) || !copyFixed(eb.Boxes[i].CT[:], bx.CT) {
			return EpochBundle{}, e2ecrypto.ErrMalformed
		}
		if devs[eb.Boxes[i].Dev] {
			return EpochBundle{}, ErrDuplicateBox
		}
		devs[eb.Boxes[i].Dev] = true
	}
	return eb, nil
}
