package voice

import (
	"fmt"
	"strings"
)

// capDev is a capture device as the backend lists it: id is the backend's
// name for it (PulseAudio source name, CoreAudio UID, ALSA id), desc its
// human name.
type capDev struct {
	id, desc string
	def      bool
}

// isMonitor: a PulseAudio/PipeWire "Monitor of …" source records what the
// speakers play. As a microphone it sends everyone's voice back to them.
func (d capDev) isMonitor() bool {
	return strings.HasSuffix(strings.ToLower(d.id), ".monitor") ||
		strings.HasPrefix(strings.ToLower(d.desc), "monit") // "Monitor of", "Monitor von", "Moniteur de"
}

func (d capDev) label() string {
	if d.desc != "" {
		return d.desc
	}
	return d.id
}

// chooseCapture picks the microphone: the one TC_VOICE_INPUT names (a
// case-insensitive substring of its id or description), else the
// default, unless the default is a monitor, then the first real
// microphone. idx -1 means "the backend's default"; note is for the user.
func chooseCapture(devs []capDev, want string) (idx int, note string) {
	if want = strings.TrimSpace(want); want != "" {
		w := strings.ToLower(want)
		for i, d := range devs {
			if strings.Contains(strings.ToLower(d.id), w) || strings.Contains(strings.ToLower(d.desc), w) {
				return i, ""
			}
		}
		note = fmt.Sprintf("TC_VOICE_INPUT=%q matches no microphone; using the default", want)
	}
	for _, d := range devs {
		if !d.def || !d.isMonitor() {
			continue
		}
		for i, r := range devs {
			if !r.isMonitor() {
				return i, joinNote(note, fmt.Sprintf("default input %q records the speakers; using %q (set TC_VOICE_INPUT to choose)", d.label(), r.label()))
			}
		}
		return -1, joinNote(note, fmt.Sprintf("default input %q records the speakers and there is no other microphone", d.label()))
	}
	return -1, note
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + " · " + b
}
