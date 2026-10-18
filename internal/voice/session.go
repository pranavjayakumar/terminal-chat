package voice

import "sync"

// Duplex is an open full-duplex audio device.
type Duplex interface{ Close() error }

// Platform is what a build offers for audio: Opus and a sound card.
// Native() returns it (stub.go: ErrNoVoice without -tags voice).
type Platform struct {
	Codec Codec
	// Open starts the device; cb gets each period's microphone samples
	// and fills the speaker buffer (same length, mono int16).
	Open func(cb func(out, in []int16)) (Duplex, error)
	// AEC is the user's echo-control preference (TC_VOICE_AEC). Open
	// honours its system part; the engine its gate part.
	AEC AECPref
}

// systemAEC is a Duplex that may run the platform's echo canceller.
type systemAEC interface{ SystemAEC() bool }

// Session is an Engine attached to an audio device.
type Session struct {
	*Engine
	dev Duplex

	mu   sync.Mutex // device callback vs Close
	fifo []int16
}

// Start opens the device and runs cfg's engine on it.
func Start(cfg Config, p Platform) (*Session, error) {
	cfg.Codec = p.Codec
	e, err := NewEngine(cfg)
	if err != nil {
		return nil, err
	}
	s := &Session{Engine: e}
	// Gate until we know better: the device may call back before Open
	// returns, and an open mic must not leak echo meanwhile.
	e.setAEC(p.AEC.resolve(false))
	if s.dev, err = p.Open(s.callback); err != nil {
		return nil, err
	}
	sys := false
	if a, ok := s.dev.(systemAEC); ok {
		sys = a.SystemAEC()
	}
	e.setAEC(p.AEC.resolve(sys))
	return s, nil
}

func (s *Session) callback(out, in []int16) {
	s.Capture(in)
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.fifo) < len(out) {
		s.fifo = append(s.fifo, s.Playout()...)
	}
	n := copy(out, s.fifo)
	s.fifo = append(s.fifo[:0], s.fifo[n:]...)
}

// Close stops the device (and with it the microphone).
func (s *Session) Close() error { return s.dev.Close() }
