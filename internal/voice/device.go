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
	input := os.Getenv("TC_VOICE_INPUT")
	return Platform{Codec: opusCodec{}, AEC: pref, Open: func(cb func(out, in []int16)) (Duplex, error) {
		return openPlatform(cb, pref, input)
	}}, nil
}

type duplex struct {
	ctx  *malgo.AllocatedContext
	dev  *malgo.Device
	ids  []unsafe.Pointer // C copies of chosen device ids
	sys  bool             // both directions go through an echo canceller
	note string           // device choice worth telling the user
	once sync.Once
}

func (d *duplex) SystemAEC() bool { return d.sys }
func (d *duplex) Note() string    { return d.note }

// devices names non-default devices on a context (nil: the default).
type devices struct {
	capture, playback *malgo.DeviceID
	sys               bool
	note              string
}

// openDuplex opens miniaudio's default backends: the default speaker and
// the microphone chooseCapture picks (TC_VOICE_INPUT, never a monitor).
func openDuplex(cb func(out, in []int16), input string) (Duplex, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, err
	}
	var devs devices
	if caps, err := ctx.Devices(malgo.Capture); err == nil {
		list := make([]capDev, len(caps))
		for i := range caps {
			list[i] = capDev{id: idString(&caps[i].ID), desc: caps[i].Name(), def: caps[i].IsDefault != 0}
		}
		i, note := chooseCapture(list, input)
		if i >= 0 {
			devs.capture = &caps[i].ID
		}
		devs.note = note
	}
	return startDuplex(ctx, devs, cb)
}

// idString is a device id as text: the backend's name for the device
// (PulseAudio source name, CoreAudio UID, ALSA id), NUL-terminated.
func idString(id *malgo.DeviceID) string {
	b := id[:]
	n := 0
	for n < len(b) && b[n] != 0 {
		n++
	}
	return string(b[:n])
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
	d := &duplex{ctx: ctx, sys: devs.sys, note: devs.note}
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
