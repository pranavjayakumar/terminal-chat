//go:build !voice

package voice

// Available reports whether this build can capture and play audio.
func Available() bool { return false }

// Native returns this build's audio platform. Without -tags voice there is
// none: tc stays a static CGO_ENABLED=0 binary.
func Native() (Platform, error) { return Platform{}, ErrNoVoice }
