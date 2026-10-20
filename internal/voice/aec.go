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
// gateFarRMS (≈ −42 dBFS: under speech at −30…−20 dBFS) and 6 dB above the
// far end's noise floor, so a remote open mic in a noisy room (fan, café)
// doesn't keep ours shut for the whole call. The floor follows quieter
// frames at once and rises gateFloorRise per frame (≈ 3 dB/s), so the
// pauses between words keep it at the noise, not the voice. It is capped
// at gateFloorMaxRMS, and anything above gateLoudRMS is always active:
// continuous loud audio (music, a talker who never pauses) can't teach
// the floor to let it through. The gate stays shut for gateHang after the
// last active frame: device latency in and out plus the room's reverb
// tail. Gain ramps (attack 2.5 ms, release 20 ms) avoid clicks.
const (
	gateFarRMS      = 260  // ≈ −42 dBFS
	gateFloorMaxRMS = 1000 // ≈ −30 dBFS
	gateLoudRMS     = 1600 // ≈ −26 dBFS
	gateOverFloor   = 4    // 6 dB
	gateFloorRise   = 1.015
	// gateFloorMin keeps 6 dB over the floor at gateFarRMS when quiet.
	gateFloorMin   = gateFarRMS * gateFarRMS / gateOverFloor
	gateHang       = SampleRate / 5 // 200 ms
	gateAttackStep = 1.0 / (SampleRate / 400)
	gateRelStep    = 1.0 / (SampleRate / 50)
)

// gate is the half-duplex fallback. far runs on Playout (under Engine.mu),
// apply on Capture (under Engine.capMu); hang is their only shared state.
// Neither allocates: both run on the audio thread.
type gate struct {
	hang  atomic.Int64 // samples the gate stays shut for
	floor float64      // far end's noise floor (mean square); Playout side only
	gain  float32      // capture side only
}

func newGate() *gate { return &gate{gain: 1, floor: gateFloorMin} }

// far measures one frame of remote audio (Engine.mix) about to be played.
func (g *gate) far(pcm []int32) {
	if len(pcm) == 0 {
		return
	}
	var sum float64
	for _, v := range pcm {
		sum += float64(v) * float64(v)
	}
	ms := sum / float64(len(pcm))
	active := ms > gateFarRMS*gateFarRMS && (ms > gateOverFloor*g.floor || ms > gateLoudRMS*gateLoudRMS)
	g.floor = min(gateFloorMaxRMS*gateFloorMaxRMS, max(gateFloorMin, min(ms, g.floor*gateFloorRise)))
	if active {
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
	if g.gain == target {
		if target == 0 {
			clear(pcm)
		}
		return // settled: the common case
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

// reset settles the gain where the gate is now: the mic just became
// gateable, and must neither leak an attack ramp of echo nor fade in a
// talker when nothing is playing.
func (g *gate) reset() {
	g.gain = 1
	if g.shut() {
		g.gain = 0
	}
}

// echoCancelPair picks a PipeWire/PulseAudio echo-cancel source and sink
// from device names (pulse names or descriptions), or -1s unless exactly
// one pair is clear. Both are needed: the canceller only hears the far end
// through its sink, so a source paired with some other sink cancels
// nothing (and the gate would be off). Several cancellers: only a source
// and sink with the same stem pair up (PipeWire's echo-cancel-source and
// echo-cancel-sink); otherwise we'd rather gate than guess.
func echoCancelPair(captures, playbacks []string) (ci, pi int) {
	var cs, ps []int
	for i, n := range captures {
		// Monitors (of the canceller's sink, say) aren't microphones: by
		// name "….monitor", by description "Monitor of …" ("Monitor von",
		// "Moniteur de" …). A canceller whose master is a monitor is fine.
		l := strings.ToLower(n)
		if isEchoCancel(n) && !strings.HasSuffix(l, ".monitor") && !strings.HasPrefix(l, "monit") {
			cs = append(cs, i)
		}
	}
	for i, n := range playbacks {
		if isEchoCancel(n) {
			ps = append(ps, i)
		}
	}
	if len(cs) == 1 && len(ps) == 1 {
		return cs[0], ps[0]
	}
	ci, pi = -1, -1
	for _, c := range cs {
		for _, p := range ps {
			if ecStem(captures[c]) != ecStem(playbacks[p]) {
				continue
			}
			if ci >= 0 {
				return -1, -1 // ambiguous
			}
			ci, pi = c, p
		}
	}
	return ci, pi
}

// isEchoCancel matches what module-echo-cancel and
// libpipewire-module-echo-cancel call their nodes by default, by name
// ("echo-cancel-source", "<master>.echo-cancel") or description
// ("Echo-Cancel Source", "… (echo cancelled with …)"). Not hardware that
// merely advertises it ("Echo Cancelling Speakerphone"): that has no sink
// of ours to hear the far end through.
func isEchoCancel(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "echo-cancel") && !strings.Contains(n, "echo-cancelling") ||
		strings.Contains(n, "echo cancelled")
}

// ecStem is a node name without its direction, so a canceller's source and
// sink compare equal.
func ecStem(name string) string {
	n := strings.ToLower(name)
	for _, s := range []string{"-source", "-sink", " source", " sink", "_source", "_sink"} {
		n = strings.ReplaceAll(n, s, "")
	}
	return n
}
