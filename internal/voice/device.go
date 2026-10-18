//go:build voice

package voice

/*
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"errors"
	"os"
	"sync"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// Available reports whether this build can capture and play audio.
func Available() bool { return true }

// Native returns libopus and the sound card, with echo control per
// TC_VOICE_AEC (aec.go): macOS's voice-processing unit, or miniaudio
// (CoreAudio on macOS; PulseAudio/ALSA, loaded at runtime, on Linux).
// Nothing is opened until a huddle calls Open.
func Native() (Platform, error) {
	pref, err := ParseAECPref(os.Getenv("TC_VOICE_AEC"))
	if err != nil {
		return Platform{}, err
	}
	return Platform{Codec: opusCodec{}, AEC: pref, Open: func(cb func(out, in []int16)) (Duplex, error) {
		return openPlatform(cb, pref)
	}}, nil
}

type duplex struct {
	ctx  *malgo.AllocatedContext
	dev  *malgo.Device
	ids  []unsafe.Pointer // C copies of chosen device ids
	sys  bool             // both directions go through an echo canceller
	once sync.Once
}

func (d *duplex) SystemAEC() bool { return d.sys }

// devices names non-default devices on a context (nil: the default).
type devices struct {
	capture, playback *malgo.DeviceID
	sys               bool
}

// openDuplex opens miniaudio's default devices on its default backends.
func openDuplex(cb func(out, in []int16)) (Duplex, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, err
	}
	return startDuplex(ctx, devices{}, cb)
}

// cID copies a device id into C memory: miniaudio's config may not hold Go
// pointers.
func cID(id *malgo.DeviceID) unsafe.Pointer {
	p := C.malloc(C.size_t(len(id)))
	C.memcpy(p, unsafe.Pointer(&id[0]), C.size_t(len(id)))
	return p
}

// startDuplex starts a device on ctx, which it then owns (freed on error).
func startDuplex(ctx *malgo.AllocatedContext, devs devices, cb func(out, in []int16)) (Duplex, error) {
	d := &duplex{ctx: ctx, sys: devs.sys}
	cfg := malgo.DefaultDeviceConfig(malgo.Duplex)
	if devs.capture != nil {
		cfg.Capture.DeviceID = cID(devs.capture)
		d.ids = append(d.ids, cfg.Capture.DeviceID)
	}
	if devs.playback != nil {
		cfg.Playback.DeviceID = cID(devs.playback)
		d.ids = append(d.ids, cfg.Playback.DeviceID)
	}
	cfg.Capture.Format, cfg.Capture.Channels = malgo.FormatS16, 1
	cfg.Playback.Format, cfg.Playback.Channels = malgo.FormatS16, 1
	cfg.SampleRate = SampleRate
	cfg.PeriodSizeInMilliseconds = 10
	data := func(out, in []byte, frames uint32) {
		n := int(frames)
		if len(out) < 2*n || len(in) < 2*n {
			clear(out)
			return
		}
		o := unsafe.Slice((*int16)(unsafe.Pointer(&out[0])), n)
		i := unsafe.Slice((*int16)(unsafe.Pointer(&in[0])), n)
		cb(o, i)
	}
	dev, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{Data: data})
	if err != nil {
		d.free()
		return nil, err
	}
	d.dev = dev
	if err := dev.Start(); err != nil {
		d.Close()
		return nil, errors.Join(errors.New("starting audio device"), err)
	}
	return d, nil
}

func (d *duplex) free() {
	_ = d.ctx.Uninit()
	d.ctx.Free()
	for _, p := range d.ids {
		C.free(p)
	}
}

func (d *duplex) Close() error {
	d.once.Do(func() {
		if d.dev != nil {
			d.dev.Uninit() // stops the device, so the mic is released
		}
		d.free()
	})
	return nil
}
