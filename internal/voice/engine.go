// Package voice is the client side of huddles (docs/VOICE.md): frame
// crypto, replay windows, jitter buffers, mixing and pacing in pure Go, plus
// audio I/O and Opus behind the build tag `voice` (stub.go otherwise).
package voice

import (
	"errors"
	"sync"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// Audio format: 48 kHz mono, 20 ms frames.
const (
	SampleRate   = 48000
	FrameSamples = SampleRate / 50
	maxOpus      = proto.MaxDatagram - proto.DgEncHeaderLen - e2ecrypto.TagSize

	// tsSkew is how far an encrypted frame's ts may be from our clock.
	tsSkew = 60 * time.Second
	// maxWins bounds remembered replay windows (srcs seen this participation).
	maxWins = 4096
	// speakingFor is how long after its last audible frame a stream counts as talking.
	speakingFor = 300 * time.Millisecond
)

// ErrNoVoice: this build has no audio (built without -tags voice).
var ErrNoVoice = errors.New("this tc build has no voice: reinstall with install.sh --voice")

// Encoder and Decoder are one Opus stream's codec state; not safe for
// concurrent use.
type Encoder interface {
	Encode(pcm []int16, out []byte) (int, error)
}

type Decoder interface {
	Decode(data []byte, pcm []int16) (int, error)
	// DecodeFEC recovers the frame before data from data's in-band FEC.
	DecodeFEC(data []byte, pcm []int16) (int, error)
	Conceal(pcm []int16) (int, error)
}

type Codec interface {
	NewEncoder() (Encoder, error)
	NewDecoder() (Decoder, error)
}

// Keys resolves frame keys for encrypted huddles. The client implements it
// from the trust store; it decides which epochs, rosters and devices count
// (PROTOCOL.md §14.3), so the engine only applies the answers.
type Keys interface {
	// Send returns the newest verified epoch and our sender for it.
	Send() (epoch uint32, s *e2ecrypto.VoiceSender, ok bool)
	// Recv returns src's sender for epoch, or false if src may not speak
	// under that epoch (unknown, unverified device, not in the roster, too old).
	Recv(src uint32, epoch uint32) (*e2ecrypto.VoiceSender, bool)
}

type Config struct {
	Src   uint32
	Enc   bool // the channel is encrypted: Keys required, plaintext frames dropped
	Keys  Keys
	Codec Codec
	Send  func([]byte) error
	Now   func() time.Time // nil = time.Now
}

type stream struct {
	user    uint64
	win     *window // outlives the stream: see Engine.wins
	jit     *jitter
	dec     Decoder
	audible time.Time // last frame that played
}

// Stats counts dropped frames by reason (tests, the TUI's warnings).
type Stats struct {
	Sent, Received, Played                     uint64
	BadFrame, Unknown, Replay, Skew, Key, Auth uint64
}

// Engine runs one participation: capture → encode → seal → send, and
// receive → verify → jitter → decode → mix.
type Engine struct {
	cfg Config
	enc Encoder

	capMu   sync.Mutex // capture side (one audio thread)
	capBuf  []int16
	seq     uint32
	talking bool
	ending  bool // send one more frame flagged end-of-spurt
	opusBuf []byte

	mu      sync.Mutex
	streams map[uint32]*stream
	// wins keeps every src's replay window for the whole participation, so
	// a roster that drops and re-adds a src can't reset it and let the
	// server replay that stream's recent frames (bounded by maxWins).
	wins  map[uint32]*window
	stats Stats
	mix   []int32
	pcm   []int16
}

func NewEngine(cfg Config) (*Engine, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Enc && cfg.Keys == nil {
		return nil, errors.New("voice: encrypted huddle without keys")
	}
	enc, err := cfg.Codec.NewEncoder()
	if err != nil {
		return nil, err
	}
	return &Engine{
		cfg: cfg, enc: enc, streams: map[uint32]*stream{}, wins: map[uint32]*window{},
		opusBuf: make([]byte, maxOpus), mix: make([]int32, FrameSamples), pcm: make([]int16, FrameSamples),
	}, nil
}

// SetTalking turns the microphone stream on or off. Turning it off sends
// one more frame flagged end-of-spurt so receivers drain cleanly.
func (e *Engine) SetTalking(on bool) {
	e.capMu.Lock()
	defer e.capMu.Unlock()
	if e.talking && !on {
		e.ending = true
	}
	if on {
		e.ending = false
	}
	e.talking = on
}

// Talking reports whether the mic stream is on.
func (e *Engine) Talking() bool {
	e.capMu.Lock()
	defer e.capMu.Unlock()
	return e.talking
}

// SetRoster replaces the remote participants (src → user), excluding us.
// Streams that left are dropped; new ones start empty.
func (e *Engine) SetRoster(parts map[uint32]uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for src := range e.streams {
		if _, ok := parts[src]; !ok {
			delete(e.streams, src)
		}
	}
	for src, user := range parts {
		if src == e.cfg.Src {
			continue
		}
		if s, ok := e.streams[src]; ok {
			s.user = user
			continue
		}
		dec, err := e.cfg.Codec.NewDecoder()
		if err != nil {
			return err
		}
		w := e.wins[src]
		if w == nil {
			if len(e.wins) >= maxWins {
				// srcs only grow: forget the oldest.
				oldest := src
				for s := range e.wins {
					oldest = min(oldest, s)
				}
				delete(e.wins, oldest)
			}
			w = &window{}
			e.wins[src] = w
		}
		e.streams[src] = &stream{user: user, win: w, jit: newJitter(), dec: dec}
	}
	return nil
}

// Capture feeds microphone samples (any count). Every full 20 ms frame is
// sent while talking.
func (e *Engine) Capture(pcm []int16) {
	e.capMu.Lock()
	defer e.capMu.Unlock()
	if !e.talking && !e.ending {
		e.capBuf = e.capBuf[:0]
		return
	}
	e.capBuf = append(e.capBuf, pcm...)
	for len(e.capBuf) >= FrameSamples && (e.talking || e.ending) {
		e.sendFrame(e.capBuf[:FrameSamples])
		e.capBuf = append(e.capBuf[:0], e.capBuf[FrameSamples:]...)
	}
}

func (e *Engine) sendFrame(pcm []int16) {
	if e.seq == ^uint32(0) {
		return // stream exhausted (~2.7 years); rejoin for a new one
	}
	n, err := e.enc.Encode(pcm, e.opusBuf)
	if err != nil || n == 0 {
		return
	}
	d := proto.Datagram{Kind: proto.DgAudio, Src: e.cfg.Src, Seq: e.seq}
	if e.ending {
		d.Flags |= proto.DgFlagEnd
		e.ending = false
	}
	var b []byte
	if e.cfg.Enc {
		ep, s, ok := e.cfg.Keys.Send()
		if !ok {
			return // never plaintext into an encrypted channel
		}
		d.Kind, d.Epoch, d.TS = proto.DgAudioEnc, ep, uint32(e.cfg.Now().Unix())
		b = s.Seal(d.Seq, d.AppendHeader(nil), e.opusBuf[:n])
	} else {
		d.Payload = e.opusBuf[:n]
		b = d.Marshal()
	}
	e.seq++
	if e.cfg.Send(b) == nil {
		e.mu.Lock()
		e.stats.Sent++
		e.mu.Unlock()
	}
}

// Receive handles one datagram from the relay. Call it from one goroutine:
// key lookups and decryption run outside the lock the audio thread needs.
func (e *Engine) Receive(b []byte) {
	now := e.cfg.Now()
	d, err := proto.ParseDatagram(b)
	if err != nil || (d.Kind == proto.DgAudioEnc) != e.cfg.Enc {
		e.count(func(s *Stats) { s.BadFrame++ })
		return
	}
	e.mu.Lock()
	s := e.streams[d.Src]
	fresh := s != nil && s.win.check(d.Seq)
	e.mu.Unlock()
	switch {
	case s == nil:
		e.count(func(s *Stats) { s.Unknown++ })
		return
	case !fresh:
		e.count(func(s *Stats) { s.Replay++ })
		return
	}
	payload := d.Payload
	if e.cfg.Enc {
		vs, ok := e.cfg.Keys.Recv(d.Src, d.Epoch)
		if !ok {
			e.count(func(s *Stats) { s.Key++ })
			return
		}
		if payload, err = vs.Open(d.Seq, b[:d.HeaderLen()], d.Payload); err != nil {
			e.count(func(s *Stats) { s.Auth++ })
			return
		}
		// ts is in the AD: checked only once authentic, so a forged ts
		// can't raise the UI's clock-skew warning.
		ts := time.Unix(int64(d.TS), 0)
		if ts.Before(now.Add(-tsSkew)) || ts.After(now.Add(tsSkew)) {
			e.count(func(s *Stats) { s.Skew++ })
			return
		}
	} else {
		payload = append([]byte(nil), payload...) // b belongs to the transport
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// The roster may have changed meanwhile; only this goroutine moves windows.
	if e.streams[d.Src] != s {
		return
	}
	s.win.accept(d.Seq)
	e.stats.Received++
	s.jit.push(d.Seq, payload, d.Flags&proto.DgFlagEnd != 0, now)
}

func (e *Engine) count(f func(*Stats)) {
	e.mu.Lock()
	f(&e.stats)
	e.mu.Unlock()
}

// Playout returns the next 20 ms of mixed remote audio. The audio device's
// clock drives it, so playout never drifts from the sound card.
func (e *Engine) Playout() []int16 {
	now := e.cfg.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	clear(e.mix)
	for _, s := range e.streams {
		kind, data := s.jit.pop()
		var n int
		var err error
		switch kind {
		case OutSilence:
			continue
		case OutFrame:
			n, err = s.dec.Decode(data, e.pcm)
			s.audible = now
			e.stats.Played++
		case OutFEC:
			n, err = s.dec.DecodeFEC(data, e.pcm)
		case OutConceal:
			n, err = s.dec.Conceal(e.pcm)
		}
		if err != nil {
			continue
		}
		for i := range min(n, FrameSamples) {
			e.mix[i] += int32(e.pcm[i])
		}
	}
	out := make([]int16, FrameSamples)
	for i, v := range e.mix {
		out[i] = int16(max(-32768, min(32767, v)))
	}
	return out
}

// Speaking returns the users heard in the last 300 ms.
func (e *Engine) Speaking() map[uint64]bool {
	now := e.cfg.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[uint64]bool{}
	for _, s := range e.streams {
		if !s.audible.IsZero() && now.Sub(s.audible) < speakingFor {
			out[s.user] = true
		}
	}
	return out
}

func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stats
}
