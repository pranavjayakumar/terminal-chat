//go:build !linux && !darwin

package transport

const backlogLimit = 0

func unsentBytes(uintptr) (int, bool) { return 0, false }
