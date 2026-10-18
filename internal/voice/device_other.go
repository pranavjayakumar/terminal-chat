//go:build voice && !darwin && !linux

package voice

func openPlatform(cb func(out, in []int16), _ AECPref) (Duplex, error) { return openDuplex(cb) }
