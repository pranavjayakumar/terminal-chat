//go:build voice

package voice

import (
	"errors"
	"sync"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// Available reports whether this build can capture and play audio.
func Available() bool { return true }

// Native returns libopus and the default sound card via miniaudio
// (CoreAudio on macOS; PulseAudio/ALSA, loaded at runtime, on Linux).
func Native() (Platform, error) {
	return Platform{Codec: opusCodec{}, Open: openDuplex}, nil
}

type duplex struct {
	ctx  *malgo.AllocatedContext
	dev  *malgo.Device
	once sync.Once
}

func openDuplex(cb func(out, in []int16)) (Duplex, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, err
	}
	cfg := malgo.DefaultDeviceConfig(malgo.Duplex)
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
		ctx.Uninit()
		ctx.Free()
		return nil, err
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		ctx.Uninit()
		ctx.Free()
		return nil, errors.Join(errors.New("starting audio device"), err)
	}
	return &duplex{ctx: ctx, dev: dev}, nil
}

func (d *duplex) Close() error {
	d.once.Do(func() {
		d.dev.Uninit() // stops the device, so the mic is released
		d.ctx.Uninit()
		d.ctx.Free()
	})
	return nil
}
