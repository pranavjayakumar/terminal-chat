//go:build voice

package voice

import (
	"github.com/gen2brain/malgo"
)

// openPlatform uses an echo-cancel source and sink when the user's
// PipeWire or PulseAudio has one loaded (docs/VOICE.md "Echo"): tc never
// loads modules or changes audio settings itself. Otherwise the default
// devices on miniaudio's default backends, with the engine's gate.
func openPlatform(cb func(out, in []int16), pref AECPref) (Duplex, error) {
	if pref.wantsSystem() {
		if ctx, devs, ok := echoCancelDevices(); ok {
			if d, err := startDuplex(ctx, devs, cb); err == nil {
				return d, nil
			}
		}
	}
	return openDuplex(cb)
}

// echoCancelDevices looks for an echo-cancel pair on the PulseAudio API
// (PipeWire serves it through pipewire-pulse). On a match it returns the
// open context for startDuplex to own.
func echoCancelDevices() (*malgo.AllocatedContext, devices, bool) {
	ctx, err := malgo.InitContext([]malgo.Backend{malgo.BackendPulseaudio}, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, devices{}, false
	}
	caps, err1 := ctx.Devices(malgo.Capture)
	plays, err2 := ctx.Devices(malgo.Playback)
	if err1 == nil && err2 == nil {
		ci, pi := echoCancelPair(pulseNames(caps), pulseNames(plays))
		if ci < 0 {
			// Descriptions, in case a config renamed the nodes.
			ci, pi = echoCancelPair(descriptions(caps), descriptions(plays))
		}
		if ci >= 0 {
			return ctx, devices{capture: &caps[ci].ID, playback: &plays[pi].ID, sys: true}, true
		}
	}
	_ = ctx.Uninit()
	ctx.Free()
	return nil, devices{}, false
}

// pulseNames are the devices' PulseAudio names (a pulse ma_device_id is
// the NUL-terminated name).
func pulseNames(ds []malgo.DeviceInfo) []string {
	out := make([]string, len(ds))
	for i := range ds {
		id := ds[i].ID[:]
		n := 0
		for n < len(id) && id[n] != 0 {
			n++
		}
		out[i] = string(id[:n])
	}
	return out
}

func descriptions(ds []malgo.DeviceInfo) []string {
	out := make([]string, len(ds))
	for i := range ds {
		out[i] = ds[i].Name()
	}
	return out
}
