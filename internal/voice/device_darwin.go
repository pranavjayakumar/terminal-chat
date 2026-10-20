//go:build voice

package voice

// openPlatform prefers the voice-processing unit (Apple's echo canceller,
// AGC and noise suppression) on the default devices; if it won't start, or
// TC_VOICE_INPUT picks a microphone, miniaudio's plain CoreAudio duplex
// with the engine's gate.
func openPlatform(cb func(out, in []int16), pref AECPref, input string) (Duplex, error) {
	if pref.wantsSystem() && input == "" {
		if d, err := openVPIO(cb); err == nil {
			return d, nil
		}
	}
	return openDuplex(cb, input)
}
