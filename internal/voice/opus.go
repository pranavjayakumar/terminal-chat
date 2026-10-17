//go:build voice

package voice

/*
#cgo pkg-config: opus
#include <opus.h>

// opus_*_ctl are variadic, which cgo can't call: wrap what we use.
static int tc_enc_setup(OpusEncoder *e) {
	int err;
	if ((err = opus_encoder_ctl(e, OPUS_SET_BITRATE(24000))) != OPUS_OK) return err;
	if ((err = opus_encoder_ctl(e, OPUS_SET_VBR(1))) != OPUS_OK) return err;
	if ((err = opus_encoder_ctl(e, OPUS_SET_INBAND_FEC(1))) != OPUS_OK) return err;
	if ((err = opus_encoder_ctl(e, OPUS_SET_PACKET_LOSS_PERC(10))) != OPUS_OK) return err;
	if ((err = opus_encoder_ctl(e, OPUS_SET_COMPLEXITY(5))) != OPUS_OK) return err;
	return opus_encoder_ctl(e, OPUS_SET_SIGNAL(OPUS_SIGNAL_VOICE));
}
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

type opusCodec struct{}

type opusEnc struct{ p *C.OpusEncoder }
type opusDec struct{ p *C.OpusDecoder }

func opusErr(code C.int) error {
	return fmt.Errorf("opus: %s", C.GoString(C.opus_strerror(code)))
}

func (opusCodec) NewEncoder() (Encoder, error) {
	var code C.int
	p := C.opus_encoder_create(SampleRate, 1, C.OPUS_APPLICATION_VOIP, &code)
	if code != C.OPUS_OK || p == nil {
		return nil, opusErr(code)
	}
	if code = C.tc_enc_setup(p); code != C.OPUS_OK {
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
