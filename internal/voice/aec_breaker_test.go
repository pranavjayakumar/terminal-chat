package voice

import "testing"

// A remote participant on an open mic in a noisy room (fan, café: ≈ −40
// dBFS RMS of steady noise, not speech) keeps the gate shut for as long as
// their mic is open. Our open mic is then silently muted for the whole
// conversation while the UI still shows it open: we never shut the
// remote's gate in return, because we send nothing.
func TestGate_BreakFarEndNoiseMutesOpenMicForever(t *testing.T) {
	r := newRig(t, AECAuto, false)
	r.s.SetTalking(true)
	r.s.SetOpenMic(true)
	const farNoise = 330 // ≈ −40 dBFS: background noise, nobody talking
	for range 10 {       // their noise reaches our speaker (jitter buffer fills)
		r.period(farNoise, 3000)
	}
	heard := 0
	for range 250 { // then 5 s of us talking over their room noise
		if v, ok := r.period(farNoise, 3000); ok && v >= 3000 {
			heard++
		}
	}
	if heard == 0 {
		t.Fatal("5 s of near-end speech fully gated by far-end background noise (no speech from the far end)")
	}
}

// Muting while the gate is shut sends the end-of-spurt frame with the gate
// bypassed (SetTalking(false) clears openMic before that frame is
// captured): 20 ms of full-level echo, with a click from gain 0 to 1.
func TestGate_BreakEndOfSpurtFrameLeaksEcho(t *testing.T) {
	r := newRig(t, AECAuto, false)
	if leaked := r.echoSent(); len(leaked) > 0 {
		t.Fatalf("control: echo sent before mute: %v", leaked)
	}
	r.s.SetTalking(false) // mute while the far end is still talking
	n := len(r.w.got)
	r.period(4000, 0)
	for _, b := range r.w.got[n:] {
		if v := sentLevel(t, b); v != 0 {
			t.Fatalf("end-of-spurt frame carried ungated echo at level %d", v)
		}
	}
}

// An echo-cancel source and an unrelated device whose name happens to
// contain "echo cancel" (USB speakerphones advertise themselves as "Echo
// Cancelling Speakerphone") are paired: the canceller's sink is never
// used, so it has no far-end reference, and with sys=true the gate is off
// too. Full echo with the UI saying "system".
func TestEchoCancelPair_BreakMismatchedSpeakerphone(t *testing.T) {
	caps := []string{"alsa_input.pci-0000_00_1f.3.analog-stereo", "echo-cancel-source"}
	plays := []string{
		"alsa_output.usb-0b0e_Echo_Cancelling_Speakerphone_123-00.analog-stereo",
		"echo-cancel-sink",
	}
	ci, pi := echoCancelPair(caps, plays)
	if ci == 1 && pi != 1 {
		t.Fatalf("paired the echo-cancel source with %q (%d), not its own sink", plays[pi], pi)
	}
}

// The description pass excludes monitors only by the English "Monitor of"
// prefix; PulseAudio/pipewire-pulse translate it. A localized monitor of
// the echo-cancel sink listed first is taken as the microphone, so we'd
// send everyone's voice straight back to them.
func TestEchoCancelPair_BreakLocalizedMonitor(t *testing.T) {
	caps := []string{"Monitor von Echo-Cancel Sink", "Echo-Cancel Source"}
	plays := []string{"Echo-Cancel Sink"}
	ci, _ := echoCancelPair(caps, plays)
	if ci == 0 {
		t.Fatalf("picked the sink monitor %q as the microphone", caps[ci])
	}
}
