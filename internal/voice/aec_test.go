package voice

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

// tone440 is n samples of a 440 Hz tone at amplitude amp (speech-like level
// for amp ≈ 3000, ≈ −20 dBFS RMS).
func tone440(n int, amp float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*440*float64(i)/SampleRate))
	}
	return out
}

func widen(pcm []int16) []int32 {
	out := make([]int32, len(pcm))
	for i, v := range pcm {
		out[i] = int32(v)
	}
	return out
}

func peak(pcm []int16) int {
	p := 0
	for _, v := range pcm {
		p = max(p, int(math.Abs(float64(v))))
	}
	return p
}

func TestAECPref(t *testing.T) {
	for in, want := range map[string]AECPref{"": AECAuto, "auto": AECAuto, " System ": AECPrefSystem, "gate": AECPrefGate, "OFF": AECPrefOff} {
		if got, err := ParseAECPref(in); err != nil || got != want {
			t.Errorf("ParseAECPref(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseAECPref("on"); err == nil {
		t.Error("bad value accepted")
	}
	cases := []struct {
		p    AECPref
		sys  bool
		want AEC
	}{
		{AECAuto, true, AECSystem}, {AECAuto, false, AECGate},
		{AECPrefSystem, true, AECSystem}, {AECPrefSystem, false, AECOff},
		{AECPrefGate, true, AECGate}, {AECPrefGate, false, AECGate},
		{AECPrefOff, true, AECOff}, {AECPrefOff, false, AECOff},
	}
	for _, c := range cases {
		if got := c.p.resolve(c.sys); got != c.want {
			t.Errorf("%d.resolve(%v) = %v want %v", c.p, c.sys, got, c.want)
		}
	}
	if AECSystem.String() != "system" || AECGate.String() != "gate" || AECOff.String() != "off" {
		t.Error("names")
	}
}

func TestGateSuppressesEcho(t *testing.T) {
	g := newGate()
	far := widen(tone440(FrameSamples, 3000))
	g.far(far)
	echo := tone440(FrameSamples, 1500)
	g.apply(echo)
	// Attack is 2.5 ms: after that, nothing gets through.
	if p := peak(echo[SampleRate/400:]); p != 0 {
		t.Fatalf("echo leaked at peak %d", p)
	}
	if p := peak(echo[:SampleRate/400]); p >= 1500 {
		t.Fatalf("no ramp at all: peak %d", p)
	}
}

func TestGateIgnoresQuietFarEnd(t *testing.T) {
	g := newGate()
	g.far(widen(tone440(FrameSamples, 200))) // ≈ −47 dBFS RMS: line noise
	near := tone440(FrameSamples, 3000)
	want := append([]int16(nil), near...)
	g.apply(near)
	if !equal(near, want) {
		t.Fatal("quiet far end gated the mic")
	}
	g.far(make([]int32, FrameSamples)) // silence
	if g.shut() {
		t.Fatal("silence shut the gate")
	}
}

// A noisy far end (open mic, fan) lets the gate open once its floor is
// learned, and their speech over that noise still shuts it.
func TestGateLearnsFarNoiseFloor(t *testing.T) {
	g := newGate()
	noise := widen(tone440(FrameSamples, 600)) // ≈ −38 dBFS RMS, steady
	for range 100 {                            // 2 s
		g.far(noise)
	}
	if g.shut() {
		t.Fatal("5 s of steady far-end noise still shuts the gate")
	}
	g.far(widen(tone440(FrameSamples, 4000)))
	if !g.shut() {
		t.Fatal("speech over the noise didn't shut the gate")
	}
	// Speech with pauses keeps the floor at the noise, not the voice.
	for range 50 {
		for range 10 {
			g.far(widen(tone440(FrameSamples, 4000)))
		}
		g.far(noise)
	}
	g.far(widen(tone440(FrameSamples, 4000)))
	if !g.shut() {
		t.Fatal("floor crept up to the far talker's voice")
	}
}

func TestGateHangover(t *testing.T) {
	g := newGate()
	g.far(widen(tone440(FrameSamples, 3000)))
	silence := make([]int32, FrameSamples)
	frames := gateHang / FrameSamples // 10 × 20 ms
	for i := range frames {
		if !g.shut() {
			t.Fatalf("gate opened after %d ms, want %d", i*20, gateHang*1000/SampleRate)
		}
		g.far(silence)
	}
	if g.shut() {
		t.Fatal("gate still shut after the hangover")
	}
	// Opening ramps back up over 20 ms rather than jumping.
	g.gain = 0
	near := tone440(FrameSamples, 3000)
	g.apply(near)
	if p := peak(near[:SampleRate/200]); p > 1000 {
		t.Fatalf("no release ramp: peak %d in the first 5 ms", p)
	}
	near = tone440(FrameSamples, 3000)
	want := append([]int16(nil), near...)
	g.apply(near)
	if !equal(near, want) {
		t.Fatal("gain not back to unity after the ramp")
	}
	// Far audio again re-arms the full hangover.
	g.far(widen(tone440(FrameSamples, 3000)))
	if g.hang.Load() != gateHang {
		t.Fatalf("hang %d", g.hang.Load())
	}
}

func TestGateNoAllocs(t *testing.T) {
	g := newGate()
	far := widen(tone440(FrameSamples, 3000))
	near := tone440(480, 3000)
	if n := testing.AllocsPerRun(100, func() {
		g.far(far)
		g.apply(near)
		g.far(make32)
		g.apply(near)
	}); n != 0 {
		t.Fatalf("%v allocs per run", n)
	}
}

var make32 = make([]int32, FrameSamples)

// sentLevel is the first sample of a captured frame (fakeEnc's payload).
func sentLevel(t *testing.T, b []byte) int16 {
	t.Helper()
	d, err := proto.ParseDatagram(b)
	if err != nil {
		t.Fatal(err)
	}
	return int16(binary.BigEndian.Uint16(d.Payload))
}

const echoDelay = SampleRate / 200

// huddleRig is a session on a fake device whose microphone hears the
// speaker (acoustic echo at half level, a period and 5 ms late) plus a
// near-end talker.
type huddleRig struct {
	t    *testing.T
	s    *Session
	far  *Engine // the remote speaker
	farW *wire
	cb   func(out, in []int16)
	w    *wire
	out  []int16
	hist []int16 // everything the speaker played
}

func newRig(t *testing.T, pref AECPref, sys bool) *huddleRig {
	t.Helper()
	r := &huddleRig{t: t, w: &wire{}, out: make([]int16, FrameSamples)}
	s, err := Start(Config{Src: 1, Send: r.w.send}, Platform{Codec: fakeCodec{}, AEC: pref,
		Open: func(f func(out, in []int16)) (Duplex, error) {
			r.cb = f
			return aecCloser{sys}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	r.s = s
	s.SetRoster(map[uint32]uint64{1: 10, 2: 20})
	fw := &wire{}
	if r.far, err = NewEngine(Config{Src: 2, Codec: fakeCodec{}, Send: fw.send}); err != nil {
		t.Fatal(err)
	}
	r.far.SetRoster(map[uint32]uint64{1: 10, 2: 20})
	r.far.SetTalking(true)
	r.farW = fw
	return r
}

type aecCloser struct{ sys bool }

func (aecCloser) Close() error      { return nil }
func (c aecCloser) SystemAEC() bool { return c.sys }

// period runs one 20 ms device period: the far end says farV (0 = quiet),
// the near talker says nearV; returns what we sent, if anything.
func (r *huddleRig) period(farV, nearV int16) (int16, bool) {
	r.t.Helper()
	if farV != 0 {
		n := len(r.farW.got)
		r.far.Capture(frameOf(farV))
		for _, b := range r.farW.got[n:] {
			r.s.Receive(b)
		}
	}
	in := make([]int16, FrameSamples)
	for i := range in {
		// What the speaker emitted one period ago, 5 ms (acoustic path,
		// converters) later, at half level.
		if j := len(r.hist) - FrameSamples - echoDelay + i; j >= 0 {
			in[i] = r.hist[j] / 2
		}
		in[i] += nearV
	}
	n := len(r.w.got)
	r.cb(r.out, in)
	r.hist = append(r.hist, r.out...)
	if len(r.w.got) == n {
		return 0, false
	}
	return sentLevel(r.t, r.w.got[len(r.w.got)-1]), true
}

// echoSent runs 20 periods of far-end talk into an open mic and returns
// the non-silent frames we sent.
func (r *huddleRig) echoSent() []int16 {
	r.s.SetTalking(true)
	r.s.SetOpenMic(true)
	var leaked []int16
	for range 20 {
		if v, _ := r.period(4000, 0); v != 0 {
			leaked = append(leaked, v)
		}
	}
	return leaked
}

func TestSessionGateOpenMic(t *testing.T) {
	// Control: without the gate the rig does echo.
	if off := newRig(t, AECPrefOff, false); len(off.echoSent()) < 10 {
		t.Fatalf("rig doesn't echo: %v", off.echoSent())
	}
	r := newRig(t, AECAuto, false)
	if r.s.AEC() != AECGate {
		t.Fatalf("mode %v", r.s.AEC())
	}
	// The far end talks: our speaker plays it, our mic hears it, and none
	// of it is sent.
	if leaked := r.echoSent(); len(leaked) > 0 {
		t.Fatalf("echo sent: %v", leaked)
	}
	// Far end stops; we talk after the hangover and are heard.
	got := int16(0)
	for range 30 {
		got, _ = r.period(0, 3000)
	}
	if got != 3000 {
		t.Fatalf("near talker after hangover sent %d", got)
	}
}

func TestSessionGatePushToTalkPasses(t *testing.T) {
	r := newRig(t, AECAuto, false)
	r.s.SetTalking(true) // held key: not an open mic
	var sent []int16
	for range 20 {
		if v, ok := r.period(4000, 3000); ok {
			sent = append(sent, v)
		}
	}
	// Double talk: the held key always gets through (echo included: that is
	// what headphones are for).
	for _, v := range sent[len(sent)-5:] {
		if v < 3000 {
			t.Fatalf("held talk key gated: %v", sent)
		}
	}
	// Latching open mid-talk starts gating; muting clears open mic.
	r.s.SetOpenMic(true)
	for range 3 {
		r.period(4000, 3000)
	}
	if v, _ := r.period(4000, 3000); v != 0 {
		t.Fatalf("open mic during far talk sent %d", v)
	}
	r.s.SetTalking(false)
	r.s.SetTalking(true)
	if v, _ := r.period(4000, 3000); v < 3000 {
		t.Fatalf("talking again after mute is gated (%d): open mic must reset", v)
	}
}

func TestSessionSystemAECNoGate(t *testing.T) {
	for _, c := range []struct {
		pref AECPref
		sys  bool
		want AEC
	}{{AECAuto, true, AECSystem}, {AECPrefOff, false, AECOff}, {AECPrefSystem, false, AECOff}} {
		r := newRig(t, c.pref, c.sys)
		if r.s.AEC() != c.want {
			t.Fatalf("%+v: mode %v", c, r.s.AEC())
		}
		r.s.SetTalking(true)
		r.s.SetOpenMic(true)
		var v int16
		for range 20 {
			v, _ = r.period(4000, 3000)
		}
		if v < 3000 {
			t.Fatalf("%+v: gated (%d) without the gate", c, v)
		}
	}
}

func TestEchoCancelPair(t *testing.T) {
	cases := []struct {
		caps, plays []string
		ci, pi      int
	}{
		// PipeWire libpipewire-module-echo-cancel defaults.
		{[]string{"alsa_input.pci-0000_00_1f.3.analog-stereo", "echo-cancel-sink.monitor", "echo-cancel-source"},
			[]string{"alsa_output.pci-0000_00_1f.3.analog-stereo", "echo-cancel-sink"}, 2, 1},
		// PulseAudio module-echo-cancel defaults.
		{[]string{"alsa_input.usb.analog-mono", "alsa_input.usb.analog-mono.echo-cancel"},
			[]string{"alsa_output.usb.analog-stereo.echo-cancel"}, 1, 0},
		// Descriptions rather than names.
		{[]string{"Built-in Audio (echo cancelled with Built-in Audio Analog Stereo)"},
			[]string{"Echo-Cancel Sink"}, 0, 0},
		// Only a source: no far-end reference, so no pair.
		{[]string{"echo-cancel-source"}, []string{"alsa_output.analog-stereo"}, -1, -1},
		// Only the sink's monitor: not a microphone.
		{[]string{"echo-cancel-sink.monitor", "Monitor of Echo-Cancel Sink"}, []string{"echo-cancel-sink"}, -1, -1},
		// A canceller whose master source is itself a monitor.
		{[]string{"micsrc.monitor", "micsrc.monitor.echo-cancel", "spk.echo-cancel.monitor"},
			[]string{"spk", "spk.echo-cancel"}, 1, 1},
		// Two PipeWire cancellers: only same-stem pairs.
		{[]string{"echo-cancel-source", "ec2-echo-cancel-source"},
			[]string{"ec2-echo-cancel-sink", "echo-cancel-sink"}, -1, -1},
		{nil, nil, -1, -1},
	}
	for _, c := range cases {
		ci, pi := echoCancelPair(c.caps, c.plays)
		if ci != c.ci || pi != c.pi {
			t.Errorf("%v / %v: got %d,%d want %d,%d", c.caps, c.plays, ci, pi, c.ci, c.pi)
		}
	}
}
