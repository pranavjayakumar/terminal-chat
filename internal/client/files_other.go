//go:build !darwin

package client

// markDownloaded: nothing to mark outside macOS (saved files are 0600,
// never executable).
func markDownloaded(string) {}
