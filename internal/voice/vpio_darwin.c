//go:build voice

// macOS voice-processing I/O (kAudioUnitSubType_VoiceProcessingIO): the
// same echo canceller FaceTime uses. miniaudio's CoreAudio backend only
// opens the plain HAL unit, so this is a small shim of our own.
//
// The unit delivers microphone audio to an input callback and asks for
// speaker audio in a render callback. Both run on the unit's I/O thread;
// a single-producer/single-consumer ring carries the mic samples from the
// first to the second, which hands both to Go as one duplex period.

#include <AudioToolbox/AudioToolbox.h>
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>

#include "vpio_darwin.h"

extern void tcVPIOCallback(uintptr_t handle, int16_t *out, uint32_t nout, int16_t *in, uint32_t nin);

#define RING 32768            // ~680 ms of mic audio; a power of two, so (w + i) % RING survives uint32 wrap
#define MAX_LAG (48000 / 10)  // drop mic backlog beyond 100 ms
#define MAX_FRAMES 8192

struct tc_vpio {
	AudioComponentInstance au;
	uintptr_t handle;
	int16_t ring[RING];
	_Atomic uint32_t w, r; // free-running sample counts
	int16_t scratch[MAX_FRAMES];
	int16_t in[MAX_FRAMES];
};

static OSStatus input_cb(void *ref, AudioUnitRenderActionFlags *flags, const AudioTimeStamp *ts,
                         UInt32 bus, UInt32 n, AudioBufferList *unused) {
	(void)unused;
	tc_vpio *v = ref;
	if (n > MAX_FRAMES) return noErr;
	AudioBufferList abl;
	abl.mNumberBuffers = 1;
	abl.mBuffers[0].mNumberChannels = 1;
	abl.mBuffers[0].mDataByteSize = n * sizeof(int16_t);
	abl.mBuffers[0].mData = v->scratch;
	OSStatus st = AudioUnitRender(v->au, flags, ts, bus, n, &abl);
	if (st != noErr) return st;
	uint32_t w = atomic_load_explicit(&v->w, memory_order_relaxed);
	uint32_t r = atomic_load_explicit(&v->r, memory_order_acquire);
	uint32_t room = RING - (w - r);
	if (n > room) n = room; // full: drop the newest (the reader trims lag)
	for (uint32_t i = 0; i < n; i++) v->ring[(w + i) % RING] = v->scratch[i];
	atomic_store_explicit(&v->w, w + n, memory_order_release);
	return noErr;
}

static OSStatus render_cb(void *ref, AudioUnitRenderActionFlags *flags, const AudioTimeStamp *ts,
                          UInt32 bus, UInt32 n, AudioBufferList *io) {
	(void)flags; (void)ts; (void)bus;
	tc_vpio *v = ref;
	int16_t *out = io->mBuffers[0].mData;
	uint32_t frames = io->mBuffers[0].mDataByteSize / sizeof(int16_t);
	if (frames > n) frames = n;
	if (frames > MAX_FRAMES) {
		memset(out, 0, io->mBuffers[0].mDataByteSize);
		return noErr;
	}
	uint32_t r = atomic_load_explicit(&v->r, memory_order_relaxed);
	uint32_t w = atomic_load_explicit(&v->w, memory_order_acquire);
	if (w - r > MAX_LAG + frames) r = w - MAX_LAG; // keep mouth-to-ear short
	uint32_t have = w - r;
	if (have > frames) have = frames;
	for (uint32_t i = 0; i < have; i++) v->in[i] = v->ring[(r + i) % RING];
	atomic_store_explicit(&v->r, r + have, memory_order_release);
	// Only the samples the mic delivered: padding a short read with zeros
	// would cut gaps into the voice whenever the input and output periods
	// differ (a Bluetooth headset mic, a resampled device), heard as crackle.
	tcVPIOCallback(v->handle, out, frames, v->in, have);
	return noErr;
}

void tc_vpio_close(tc_vpio *v) {
	if (!v) return;
	if (v->au) {
		AudioOutputUnitStop(v->au);
		AudioUnitUninitialize(v->au);
		AudioComponentInstanceDispose(v->au);
	}
	free(v);
}

tc_vpio *tc_vpio_open(uintptr_t handle, int agc, int32_t *status) {
	OSStatus st;
	tc_vpio *v = calloc(1, sizeof *v);
	if (!v) { *status = -1; return NULL; }
	v->handle = handle;

	AudioComponentDescription desc = {
		.componentType = kAudioUnitType_Output,
		.componentSubType = kAudioUnitSubType_VoiceProcessingIO,
		.componentManufacturer = kAudioUnitManufacturer_Apple,
	};
	AudioComponent comp = AudioComponentFindNext(NULL, &desc);
	if (!comp) { *status = -2; free(v); return NULL; }
	if ((st = AudioComponentInstanceNew(comp, &v->au)) != noErr) { v->au = NULL; goto fail; }

	UInt32 one = 1;
	if ((st = AudioUnitSetProperty(v->au, kAudioOutputUnitProperty_EnableIO, kAudioUnitScope_Input, 1, &one, sizeof one)) != noErr) goto fail;
	if ((st = AudioUnitSetProperty(v->au, kAudioOutputUnitProperty_EnableIO, kAudioUnitScope_Output, 0, &one, sizeof one)) != noErr) goto fail;

	AudioStreamBasicDescription f = {
		.mSampleRate = 48000,
		.mFormatID = kAudioFormatLinearPCM,
		.mFormatFlags = kAudioFormatFlagIsSignedInteger | kAudioFormatFlagIsPacked,
		.mBytesPerPacket = 2,
		.mFramesPerPacket = 1,
		.mBytesPerFrame = 2,
		.mChannelsPerFrame = 1,
		.mBitsPerChannel = 16,
	};
	// What we read from the mic (output side of bus 1) and give the speaker
	// (input side of bus 0).
	if ((st = AudioUnitSetProperty(v->au, kAudioUnitProperty_StreamFormat, kAudioUnitScope_Output, 1, &f, sizeof f)) != noErr) goto fail;
	if ((st = AudioUnitSetProperty(v->au, kAudioUnitProperty_StreamFormat, kAudioUnitScope_Input, 0, &f, sizeof f)) != noErr) goto fail;

	AURenderCallbackStruct in = {.inputProc = input_cb, .inputProcRefCon = v};
	if ((st = AudioUnitSetProperty(v->au, kAudioOutputUnitProperty_SetInputCallback, kAudioUnitScope_Global, 1, &in, sizeof in)) != noErr) goto fail;
	AURenderCallbackStruct out = {.inputProc = render_cb, .inputProcRefCon = v};
	if ((st = AudioUnitSetProperty(v->au, kAudioUnitProperty_SetRenderCallback, kAudioUnitScope_Input, 0, &out, sizeof out)) != noErr) goto fail;

	// Apple's AGC pumps the level and dulls the voice; Opus and the far
	// end's volume cope with a steady mic level better. Echo cancellation
	// stays on. Best effort: an old unit without the property keeps AGC.
	UInt32 agcOn = agc ? 1 : 0;
	AudioUnitSetProperty(v->au, kAUVoiceIOProperty_VoiceProcessingEnableAGC, kAudioUnitScope_Global, 0, &agcOn, sizeof agcOn);

#if defined(MAC_OS_VERSION_14_0) && MAC_OS_X_VERSION_MAX_ALLOWED >= MAC_OS_VERSION_14_0
	// By default voice processing ducks every other app's audio hard while
	// a call is up; ask for the least ducking (best effort).
	if (__builtin_available(macOS 14.0, *)) {
		AUVoiceIOOtherAudioDuckingConfiguration dc = {
			.mEnableAdvancedDucking = false,
			.mDuckingLevel = kAUVoiceIOOtherAudioDuckingLevelMin,
		};
		AudioUnitSetProperty(v->au, kAUVoiceIOProperty_OtherAudioDuckingConfiguration, kAudioUnitScope_Global, 0, &dc, sizeof dc);
	}
#endif

	if ((st = AudioUnitInitialize(v->au)) != noErr) goto fail;
	if ((st = AudioOutputUnitStart(v->au)) != noErr) goto fail;
	return v;

fail:
	*status = st;
	tc_vpio_close(v);
	return NULL;
}
