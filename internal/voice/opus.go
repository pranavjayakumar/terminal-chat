//go:build voice

package voice

/*
#cgo pkg-config: opus
#include <opus.h>

// opus_*_ctl are variadic, which cgo can't call: wrap what we use.
static int tc_enc_setup(OpusEncoder *e, int bitrate, int complexity, int loss) {
	int err;
	if ((err = opus_encoder_ctl(e, OPUS_SET_BITRATE(bitrate))) != OPUS_OK) return err;
	if ((err = opus_encoder_ctl(e, OPUS_SET_VBR(1))) != OPUS_OK) return err;
	if ((err = opus_encoder_ctl(e, OPUS_SET_INBAND_FEC(1))) != OPUS_OK) return err;
	if ((err = opus_encoder_ctl(e, OPUS_SET_PACKET_LOSS_PERC(loss))) != OPUS_OK) return err;
	if ((err = opus_encoder_ctl(e, OPUS_SET_COMPLEXITY(complexity))) != OPUS_OK) return err;
	return opus_encoder_ctl(e, OPUS_SET_SIGNAL(OPUS_SIGNAL_VOICE));
}
static int tc_enc_loss(OpusEncoder *e, int loss) {
	return opus_encoder_ctl(e, OPUS_SET_PACKET_LOSS_PERC(loss));
}
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

// Encoder settings (docs/VOICE.md "Codec"). 32 kbit/s keeps Opus in
// fullband with in-band FEC on; at 24 kbit/s FEC pushed it down to
// super-wideband and took bits from the primary frame. Complexity 9 costs
// well under a millisecond a frame (TestOpusQuality logs it).
const (
	opusBitrate    = 32000
	opusComplexity = 9
	// opusLossInit is the loss FEC protects against until the engine has
	// measured some (setLoss).
	opusLossInit = 5
)

// opusCodec is libopus; zero value = the settings above. The other fields
// exist so tests can compare against older settings.
type opusCodec struct{ bitrate, complexity, loss int }

func (c opusCodec) settings() (bitrate, complexity, loss int) {
	if c.bitrate == 0 {
		return opusBitrate, opusComplexity, opusLossInit
	}
	return c.bitrate, c.complexity, c.loss
}

type opusEnc struct{ p *C.OpusEncoder }
type opusDec struct{ p *C.OpusDecoder }

func opusErr(code C.int) error {
	return fmt.Errorf("opus: %s", C.GoString(C.opus_strerror(code)))
}

func (c opusCodec) NewEncoder() (Encoder, error) {
	br, cx, loss := c.settings()
	var code C.int
	p := C.opus_encoder_create(SampleRate, 1, C.OPUS_APPLICATION_VOIP, &code)
	if code != C.OPUS_OK || p == nil {
		return nil, opusErr(code)
	}
	if code = C.tc_enc_setup(p, C.int(br), C.int(cx), C.int(loss)); code != C.OPUS_OK {
		C.opus_encoder_destroy(p)
		return nil, opusErr(code)
	}
	e := &opusEnc{p}
	runtime.AddCleanup(e, func(p *C.OpusEncoder) { C.opus_encoder_destroy(p) }, p)
	return e, nil
}

func (opusCodec) NewDecoder() (Decoder, error) {
	var code C.int
	p := C.opus_decoder_create(SampleRate, 1, &code)
	if code != C.OPUS_OK || p == nil {
		return nil, opusErr(code)
	}
	d := &opusDec{p}
	runtime.AddCleanup(d, func(p *C.OpusDecoder) { C.opus_decoder_destroy(p) }, p)
	return d, nil
}

func (e *opusEnc) Encode(pcm []int16, out []byte) (int, error) {
	if len(pcm) < FrameSamples || len(out) == 0 {
		return 0, errors.New("opus: short buffer")
	}
	n := C.opus_encode(e.p, (*C.opus_int16)(unsafe.Pointer(&pcm[0])), FrameSamples,
		(*C.uchar)(unsafe.Pointer(&out[0])), C.opus_int32(len(out)))
	runtime.KeepAlive(e)
	if n < 0 {
		return 0, opusErr(n)
	}
	return int(n), nil
}

// SetLoss tells FEC the packet loss to protect against, in percent (0
// turns redundancy off: a clean link spends no bits on it).
func (e *opusEnc) SetLoss(pct int) {
	C.tc_enc_loss(e.p, C.int(max(0, min(100, pct))))
	runtime.KeepAlive(e)
}

// packetBandwidth names the audio bandwidth an Opus packet was coded with.
func packetBandwidth(p []byte) string {
	if len(p) == 0 {
		return "?"
	}
	switch C.opus_packet_get_bandwidth((*C.uchar)(unsafe.Pointer(&p[0]))) {
	case C.OPUS_BANDWIDTH_NARROWBAND:
		return "NB"
	case C.OPUS_BANDWIDTH_MEDIUMBAND:
		return "MB"
	case C.OPUS_BANDWIDTH_WIDEBAND:
		return "WB"
	case C.OPUS_BANDWIDTH_SUPERWIDEBAND:
		return "SWB"
	case C.OPUS_BANDWIDTH_FULLBAND:
		return "FB"
	}
	return "?"
}

func (d *opusDec) decode(data []byte, pcm []int16, fec C.int) (int, error) {
	if len(pcm) < FrameSamples {
		return 0, errors.New("opus: short buffer")
	}
	var dp *C.uchar
	if len(data) > 0 {
		dp = (*C.uchar)(unsafe.Pointer(&data[0]))
	}
	n := C.opus_decode(d.p, dp, C.opus_int32(len(data)), (*C.opus_int16)(unsafe.Pointer(&pcm[0])), FrameSamples, fec)
	runtime.KeepAlive(d)
	if n < 0 {
		return 0, opusErr(n)
	}
	return int(n), nil
}

func (d *opusDec) Decode(data []byte, pcm []int16) (int, error)    { return d.decode(data, pcm, 0) }
func (d *opusDec) DecodeFEC(data []byte, pcm []int16) (int, error) { return d.decode(data, pcm, 1) }
func (d *opusDec) Conceal(pcm []int16) (int, error)                { return d.decode(nil, pcm, 0) }
