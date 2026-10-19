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
	if strings.HasSuffix(strings.ToLower(d.id), ".monitor") {
		return true
	}
	desc := strings.ToLower(d.desc)
	for _, p := range []string{"monitor of ", "monitor von ", "moniteur de ", "monitor de ", "monitor di ", "monitor van "} {
		if strings.HasPrefix(desc, p) {
			return true
		}
	}
	return false
}

func (d capDev) label() string {
	if d.desc != "" {
		return d.desc
	}
	return d.id
}

// noMic: chooseCapture found nothing safe to record; the device runs
// listen-only (silence captured).
const noMic = -2

// chooseCapture picks the microphone: the one TC_VOICE_INPUT names (a
// case-insensitive substring of its id or description; never a monitor,
// which would send everyone's voice back to them), else the default,
// unless the default is a monitor, then the first real microphone, else
// none (noMic). idx -1 means "the backend's default". note is for the
// user, the result first: notes get truncated in narrow panes.
func chooseCapture(devs []capDev, want string) (idx int, note string) {
	if want = strings.TrimSpace(want); want != "" {
		w := strings.ToLower(want)
		for i, d := range devs {
			if !d.isMonitor() && (strings.Contains(strings.ToLower(d.id), w) || strings.Contains(strings.ToLower(d.desc), w)) {
				return i, ""
			}
		}
		note = fmt.Sprintf("TC_VOICE_INPUT %q matched no microphone", want)
	}
	for _, d := range devs {
		if !d.def || !d.isMonitor() {
			continue
		}
		for i, r := range devs {
			if !r.isMonitor() {
				return i, joinNote("mic: "+r.label(), "default input is a speaker monitor", note, "TC_VOICE_INPUT picks another")
			}
		}
		return noMic, joinNote("no microphone, listening only", "default input is a speaker monitor", note)
	}
	if note != "" {
		return -1, joinNote("mic: default", note)
	}
	return -1, ""
}

func joinNote(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}
