package voice

import (
	"math"
	"time"
)

// FrameDur is the duration of one Opus frame (PROTOCOL.md §14.3).
const FrameDur = 20 * time.Millisecond

// Jitter buffer bounds, in frames (docs/VOICE.md "Jitter buffer").
const (
	ringSize  = 16 // 320 ms: anything further ahead resyncs
	minTarget = 3  // 60 ms: device periods (10 ms, VPIO's 10.7 ms) on both ends jitter frames by ±10 ms
	maxTarget = 10 // 200 ms
	// initialTarget leaves room for the first spurt, before any jitter
	// has been measured.
	initialTarget = 4
	maxMissing    = 5 // then the stream goes quiet and restarts
	// maxExcess: frames buffered beyond the target before the oldest are
	// dropped. A stall followed by a burst (Wi-Fi, and always on the TCP
	// fallback, which delivers late rather than never) would otherwise add
	// its length to the delay for the rest of the spurt.
	maxExcess = 4 // 80 ms
)

// OutKind says what the decoder should do for one 20 ms slot.
type OutKind int

const (
	OutSilence OutKind = iota // nothing to play
	OutFrame                  // decode Data
	OutFEC                    // decode Data's in-band FEC (the frame after the lost one)
	OutConceal                // packet loss concealment
)

type slot struct {
	seq  uint32
	data []byte
	end  bool
	ok   bool
}

// jitter reorders one stream's frames and paces them out, one per 20 ms.
type jitter struct {
	ring    [ringSize]slot
	started bool
	next    uint32 // next seq to play while started
	missing int
	target  int
	hold    int // slots to stretch (play concealment without advancing)
	// played is the highest seq ever played or skipped: anything at or
	// below it is stale, even between talk spurts.
	played     uint32
	playedSome bool

	// Outcomes since the stream started (the engine's loss estimate).
	frames, lost, fec, trimmed uint64

	// RFC 3550 interarrival jitter estimate, in ms.
	jit       float64
	lastArr   time.Time
	lastSeq   uint32
	haveTrans bool
}

func newJitter() *jitter { return &jitter{target: initialTarget} }

func (j *jitter) at(seq uint32) *slot { return &j.ring[seq%ringSize] }

func (j *jitter) push(seq uint32, data []byte, end bool, now time.Time) {
	j.observe(seq, end, now)
	if j.playedSome && seq <= j.played {
		// Late (or a replay of a finished spurt). Arriving just behind the
		// playout point means the delay is too short: stretch it by one frame.
		if j.started && j.played-seq < 2 && j.target < maxTarget {
			j.target++
			j.hold++
		}
		return
	}
	if j.started {
		d := int64(seq) - int64(j.next)
		switch {
		case d >= ringSize:
			j.reset() // far ahead: the sender restarted or we stalled
		}
	} else if n, lowest := j.buffered(); n > 0 && int64(seq)-int64(lowest) >= ringSize {
		j.reset() // leftovers of an earlier spurt
	}
	s := j.at(seq)
	if s.ok && s.seq == seq {
		return
	}
	*s = slot{seq: seq, data: data, end: end, ok: true}
}

// maxTransit caps one sample of the jitter estimator, so a lost
// end-of-spurt flag (seq runs on across the silence) can't blow it up.
const maxTransit = float64(maxTarget) * 20

func (j *jitter) observe(seq uint32, end bool, now time.Time) {
	if j.haveTrans && seq > j.lastSeq {
		// D = (arrival delta) − (send delta), sends being seq × 20 ms.
		d := now.Sub(j.lastArr) - time.Duration(seq-j.lastSeq)*FrameDur
		j.jit += (min(maxTransit, math.Abs(float64(d)/float64(time.Millisecond))) - j.jit) / 16
	}
	if !j.haveTrans || seq > j.lastSeq {
		j.lastArr, j.lastSeq, j.haveTrans = now, seq, true
	}
	if end {
		// seq doesn't advance during silence; the next spurt starts afresh.
		j.haveTrans = false
	}
}

func (j *jitter) reset() {
	j.ring = [ringSize]slot{}
	j.started, j.missing, j.hold = false, 0, 0
}

func (j *jitter) buffered() (n int, lowest uint32) {
	n, lowest, _ = j.scan()
	return n, lowest
}

// scan also reports whether an end-of-spurt frame is buffered.
func (j *jitter) scan() (n int, lowest uint32, end bool) {
	first := true
	for _, s := range j.ring {
		if !s.ok {
			continue
		}
		n++
		end = end || s.end
		if first || s.seq < lowest {
			lowest, first = s.seq, false
		}
	}
	return n, lowest, end
}

// pop yields the next 20 ms of this stream.
func (j *jitter) pop() (OutKind, []byte) {
	if !j.started {
		n, lowest, end := j.scan()
		// Adapt only at talk-spurt starts so playout never stretches
		// mid-sentence, and shrink one frame per spurt at most: a delay that
		// a late frame just grew was too short, and dropping straight back
		// would conceal (and stretch) again at the next spurt's start.
		want := int(math.Ceil((3*j.jit + 20) / 20))
		j.target = min(maxTarget, max(minTarget, want, j.target-1))
		// A whole spurt shorter than the target plays at once.
		if n == 0 || n < j.target && !end {
			return OutSilence, nil
		}
		j.started, j.next, j.missing = true, lowest, 0
	}
	if j.hold > 0 {
		j.hold--
		return OutConceal, nil
	}
	j.trim()
	s := j.at(j.next)
	seq := j.next
	j.next++
	j.played, j.playedSome = seq, true
	if s.ok && s.seq == seq {
		data, end := s.data, s.end
		*s = slot{}
		j.missing = 0
		j.frames++
		if end {
			j.reset()
		}
		return OutFrame, data
	}
	j.missing++
	if j.missing > maxMissing {
		j.reset()
		return OutSilence, nil
	}
	j.lost++
	if n := j.at(j.next); n.ok && n.seq == j.next {
		j.fec++
		return OutFEC, n.data
	}
	return OutConceal, nil
}

// trim drops the oldest buffered frames while more than target+maxExcess
// are waiting: playing them would only be late.
func (j *jitter) trim() {
	var high uint32
	found := false
	for _, s := range j.ring {
		if s.ok && int32(s.seq-j.next) >= 0 && (!found || s.seq > high) {
			high, found = s.seq, true
		}
	}
	if !found {
		return
	}
	depth := int64(high) - int64(j.next) + 1
	if depth <= int64(j.target+maxExcess) {
		return
	}
	keep := uint32(j.target)
	for seq := j.next; seq != high-keep+1; seq++ {
		if s := j.at(seq); s.ok && s.seq == seq {
			*s = slot{}
			j.trimmed++
		}
	}
	j.next = high - keep + 1
	j.played = j.next - 1
}
