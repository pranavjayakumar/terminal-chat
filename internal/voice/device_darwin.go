//go:build voice

package voice

// openPlatform prefers the voice-processing unit (Apple's echo canceller,
// AGC and noise suppression); if it won't start, miniaudio's plain
// CoreAudio duplex with the engine's gate.
func openPlatform(cb func(out, in []int16), pref AECPref) (Duplex, error) {
	if pref.wantsSystem() {
		if d, err := openVPIO(cb); err == nil {
			return d, nil
		}
	}
	return openDuplex(cb)
}
