package voice

import "testing"

// TC_VOICE_INPUT is a substring match and the first hit wins. PulseAudio
// lists a USB headset's sink monitor next to (often before) its mic, and
// both carry the product name, so the natural TC_VOICE_INPUT=logitech picks
// the monitor: we transmit the speakers (everyone's voice back to them),
// silently — no note, and the gate can't help (it's not acoustic echo).
func TestChooseCapture_BreakEnvSubstringPicksMonitor(t *testing.T) {
	devs := []capDev{
		{id: "alsa_output.usb-Logitech_USB_Headset-00.analog-stereo.monitor", desc: "Monitor of Logitech USB Headset Analog Stereo"},
		{id: "alsa_input.usb-Logitech_USB_Headset-00.mono-fallback", desc: "Logitech USB Headset Mono", def: true},
	}
	i, note := chooseCapture(devs, "logitech")
	if i >= 0 && devs[i].isMonitor() {
		t.Fatalf("TC_VOICE_INPUT=logitech chose monitor %q over the headset mic (note %q)", devs[i].id, note)
	}
}
