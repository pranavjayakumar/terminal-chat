package voice

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Echo control (docs/VOICE.md "Echo"). Speakers playing a remote voice into
// an open microphone send that voice back to its speaker. Where the OS has
// an echo canceller (macOS voice processing, a PipeWire/PulseAudio
// echo-cancel source) the device layer uses it; otherwise the engine gates
// an open mic while remote audio plays. A held talk key is never gated.

// AEC is the echo control in use for a huddle.
type AEC uint8

const (
	AECOff    AEC = iota // nothing: headphones, or TC_VOICE_AEC=off
	AECGate              // the engine's open-mic gate
	AECSystem            // the platform's echo canceller
)

func (a AEC) String() string {
	switch a {
	case AECGate:
		return "gate"
	case AECSystem:
		return "system"
	}
	return "off"
}

// AECPref is what the user asked for (TC_VOICE_AEC).
type AECPref uint8

const (
	AECAuto       AECPref = iota // system if available, else gate
	AECPrefSystem                // system if available, else off
	AECPrefGate                  // gate only, never the platform's
	AECPrefOff                   // neither
)

// ParseAECPref reads TC_VOICE_AEC; "" is auto.
func ParseAECPref(s string) (AECPref, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return AECAuto, nil
	case "system":
		return AECPrefSystem, nil
	case "gate":
		return AECPrefGate, nil
	case "off":
		return AECPrefOff, nil
	}
	return AECAuto, fmt.Errorf("TC_VOICE_AEC=%q: want auto, system, gate or off", s)
}

// wantsSystem: should the device layer try the platform canceller?
func (p AECPref) wantsSystem() bool { return p == AECAuto || p == AECPrefSystem }

// resolve is the mode in use given whether the device came up with the
// platform canceller.
func (p AECPref) resolve(system bool) AEC {
	switch {
	case system && p.wantsSystem():
		return AECSystem
	case p == AECAuto || p == AECPrefGate:
		return AECGate
	}
	return AECOff
}

// Gate tuning. Far-end "active" is a 20 ms playout frame whose RMS is above
// gateFarRMS (≈ −42 dBFS: well under speech at −30…−20 dBFS, above the
// noise floor of a quiet room sent through Opus). The gate stays shut for
// gateHang after the last such frame: device latency in and out plus the
// room's reverb tail. Gain ramps (attack 2.5 ms, release 20 ms) avoid
// clicks.
const (
	gateFarRMS     = 260
	gateHang       = SampleRate / 5 // 200 ms
	gateAttackStep = 1.0 / (SampleRate / 400)
	gateRelStep    = 1.0 / (SampleRate / 50)
)

// gate is the half-duplex fallback. far runs on Playout (under Engine.mu),
// apply on Capture (under Engine.capMu); hang is their only shared state.
// Neither allocates: both run on the audio thread.
type gate struct {
	hang atomic.Int64 // samples the gate stays shut for
	gain float32      // capture side only
}

func newGate() *gate { return &gate{gain: 1} }

// far measures one frame of remote audio (Engine.mix) about to be played.
func (g *gate) far(pcm []int32) {
	var sum int64
	for _, v := range pcm {
		sum += int64(v) * int64(v)
	}
	if len(pcm) > 0 && sum > int64(len(pcm))*gateFarRMS*gateFarRMS {
		g.hang.Store(gateHang)
		return
	}
	// Only Playout writes hang, so load-then-store doesn't race a writer.
	g.hang.Store(max(0, g.hang.Load()-int64(len(pcm))))
}

// shut reports whether remote audio is (or was just) playing.
func (g *gate) shut() bool { return g.hang.Load() > 0 }

// apply attenuates captured samples in place while the gate is shut, and
// ramps back to unity once it opens.
func (g *gate) apply(pcm []int16) {
	target, step := float32(1), float32(gateRelStep)
	if g.shut() {
		target, step = 0, gateAttackStep
	}
	if target == 1 && g.gain >= 1 {
		return // open and settled: the common case
	}
	for i, v := range pcm {
		switch {
		case g.gain < target:
			g.gain = min(target, g.gain+step)
		case g.gain > target:
			g.gain = max(target, g.gain-step)
		}
		pcm[i] = int16(float32(v) * g.gain)
	}
}

// reset opens the gate at once (the mic just opened with no far audio, or
// the held key took over).
func (g *gate) reset() { g.gain = 1 }

// echoCancelPair picks a PipeWire/PulseAudio echo-cancel source and sink
// from device names (pulse names or descriptions), or -1s if there isn't
// a pair. Both are needed: the canceller only sees the far end through its
// sink. Monitors of the echo-cancel sink are not sources of our voice.
func echoCancelPair(captures, playbacks []string) (ci, pi int) {
	ci, pi = -1, -1
	for i, n := range captures {
		if isEchoCancel(n) && !strings.HasSuffix(strings.ToLower(n), ".monitor") &&
			!strings.HasPrefix(strings.ToLower(n), "monitor of") {
			ci = i
			break
		}
	}
	for i, n := range playbacks {
		if isEchoCancel(n) {
			pi = i
			break
		}
	}
	if ci < 0 || pi < 0 {
		return -1, -1
	}
	return ci, pi
}

func isEchoCancel(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{"echo-cancel", "echo_cancel", "echo cancel", "echocancel"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}
