package voice

import (
	"testing"

	"github.com/skailama/tc/internal/proto"
)

// The clock-skew counter (Stats.Skew) drives a user-visible warning ("a
// clock is over a minute off — check NTP", client.ErrClockSkew, 25 drops in
// a second). The ts check runs before the AEAD opens, so frames nobody with
// the key produced (garbage ciphertext from the server, or under a src/epoch
// we hold no key for at all) are counted as skew: the warning fires on
// forgeries, with every clock correct. ts is in the AD, so it can be trusted
// only once the frame opens.
func TestEngine_BreakSkewCountedBeforeAuth(t *testing.T) {
	keys := newKeys()
	_, b, _, c := newPair(t, true, keys)
	far := uint32(c.t.Unix() - 3600) // an hour off: well past ±60 s
	for seq := range uint32(30) {
		d := proto.Datagram{Kind: proto.DgAudioEnc, Src: 1, Seq: seq, Epoch: 1, TS: far}
		frame := append(d.AppendHeader(nil), make([]byte, 32)...) // not sealed by anyone
		b.Receive(frame)
	}
	// Same with an epoch no one holds a key for.
	for seq := range uint32(30) {
		d := proto.Datagram{Kind: proto.DgAudioEnc, Src: 1, Seq: 1000 + seq, Epoch: 9, TS: far}
		b.Receive(append(d.AppendHeader(nil), make([]byte, 32)...))
	}
	if s := b.Stats(); s.Skew != 0 {
		t.Fatalf("%d unauthenticated frames counted as clock skew (enough to raise ErrClockSkew): %+v", s.Skew, s)
	}
}
