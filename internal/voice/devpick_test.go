package voice

import (
	"strings"
	"testing"
)

func TestChooseCapture(t *testing.T) {
	pulse := []capDev{
		{id: "alsa_output.pci.analog-stereo.monitor", desc: "Monitor of Built-in Audio Analog Stereo", def: true},
		{id: "alsa_input.pci.analog-stereo", desc: "Built-in Audio Analog Stereo"},
		{id: "alsa_input.usb-Blue_Yeti.analog-stereo", desc: "Yeti Stereo Microphone"},
	}
	cases := []struct {
		name string
		devs []capDev
		want string
		idx  int
		note string // substring; "" = no note
	}{
		{"monitor default skipped", pulse, "", 1, "records the speakers"},
		{"env picks by description", pulse, "yeti", 2, ""},
		{"env picks by id", pulse, "alsa_input.pci", 1, ""},
		{"env matches nothing, monitor default", pulse, "nope", 1, "matches no microphone"},
		{"localized monitor", []capDev{{id: "x", desc: "Monitor von Lautsprecher", def: true}, {id: "mic", desc: "Mikrofon"}}, "", 1, "records the speakers"},
		{"only a monitor", []capDev{{id: "spk.monitor", def: true}}, "", -1, "no other microphone"},
		{"normal default", []capDev{{id: "mic", desc: "Mic", def: true}, {id: "spk.monitor"}}, "", -1, ""},
		{"env matches nothing, fine default", []capDev{{id: "mic", def: true}}, "yeti", -1, "matches no microphone"},
		{"nothing listed", nil, "", -1, ""},
	}
	for _, c := range cases {
		idx, note := chooseCapture(c.devs, c.want)
		if idx != c.idx {
			t.Errorf("%s: idx %d want %d", c.name, idx, c.idx)
		}
		if (c.note == "") != (note == "") || !strings.Contains(note, c.note) {
			t.Errorf("%s: note %q want ~%q", c.name, note, c.note)
		}
	}
}

func TestSessionNote(t *testing.T) {
	s, err := Start(Config{Src: 1, Send: (&wire{}).send}, Platform{Codec: fakeCodec{},
		Open: func(func(out, in []int16)) (Duplex, error) { return notedCloser{"hi"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if s.Note() != "hi" {
		t.Fatalf("note %q", s.Note())
	}
}

type notedCloser struct{ note string }

func (notedCloser) Close() error   { return nil }
func (c notedCloser) Note() string { return c.note }
