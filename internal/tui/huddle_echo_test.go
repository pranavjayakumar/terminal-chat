package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Echo control (docs/VOICE.md "Echo"): a mic is gateable (open) unless a
// key-repeat proves it held; the headphones hint shows once per run, on
// screen, without system AEC.
func TestHuddleEchoControl(t *testing.T) {
	m, h := newHuddleModel(t, 80, 24)
	clk := newClock(m)
	h.aec = "gate"
	runInput(t, m, "/huddle")
	if !strings.Contains(plain(m), headphonesHint) {
		t.Fatalf("hint not on screen at 80 cols:\n%s", plain(m))
	}
	if m.noticeDim {
		t.Fatal("hint dims the talk help")
	}
	runInput(t, m, "/huddle")
	if m.noticeErr || !strings.Contains(plain(m), "echo: gate only, use headphones") {
		t.Fatalf("/huddle info %q", m.notice)
	}

	// A fresh press is gateable until a repeat shows it's held.
	clk.add(time.Second)
	ctrlT(m)
	if !h.OpenMic() || !h.Talking() {
		t.Fatal("fresh press: want talking, gateable")
	}
	m.Update(talkCheckMsg{m.hud.talkGen}) // no repeat: a tap
	if !h.OpenMic() || m.hud.talk != talkLatched {
		t.Fatal("tap didn't latch an open mic")
	}
	// Real timing: a later press unlatches, then a fresh press, the OS
	// repeat delay and repeats: held, never gated.
	clk.add(5 * time.Second)
	ctrlT(m)
	if h.Talking() || h.OpenMic() {
		t.Fatal("press on a latched mic didn't close it")
	}
	clk.add(5 * time.Second)
	ctrlT(m)
	clk.add(400 * time.Millisecond)
	ctrlT(m) // first repeat
	clk.add(30 * time.Millisecond)
	ctrlT(m)
	if h.OpenMic() || !h.Talking() {
		t.Fatalf("held key still gateable: open=%v talking=%v", h.OpenMic(), h.Talking())
	}
	m.Update(talkCheckMsg{m.hud.talkGen}) // released
	if h.OpenMic() || h.Talking() {
		t.Fatal("release left the mic on")
	}
	runInput(t, m, "/talk")
	if !h.OpenMic() {
		t.Fatal("/talk isn't open mic")
	}
	runInput(t, m, "/mute")
	if h.OpenMic() {
		t.Fatal("/mute left open mic set")
	}

	// Once per run: leaving and rejoining doesn't nag again.
	runInput(t, m, "/leave-huddle")
	runInput(t, m, "/huddle")
	if strings.Contains(m.notice, "headphones") {
		t.Fatalf("hint repeated: %q", m.notice)
	}

	// With system AEC: no hint at all.
	m2, _ := newHuddleModel(t, 100, 24)
	runInput(t, m2, "/huddle")
	if strings.Contains(m2.notice, "headphones") {
		t.Fatalf("hint despite system AEC: %q", m2.notice)
	}
	m3, f3 := newHuddleModel(t, 100, 24)
	f3.aec = "off"
	runInput(t, m3, "/huddle")
	runInput(t, m3, "/huddle")
	if !strings.Contains(m3.notice, "echo cancellation off") {
		t.Fatalf("off: %q", m3.notice)
	}
}

// A device note (monitor default skipped) shows on join and in /huddle.
func TestHuddleVoiceNote(t *testing.T) {
	// The realistic PulseAudio note (voice.chooseCapture), gate mode: the
	// chosen mic and the headphones hint must both be on screen at 80
	// columns with the sidebar, and /huddle answers about echo first.
	const note = "mic: Built-in Audio Analog Stereo · default input is a speaker monitor · TC_VOICE_INPUT picks another"
	for _, w := range []int{80, 120} {
		m, h := newHuddleModel(t, w, 24)
		h.note, h.aec = note, "gate"
		runInput(t, m, "/huddle")
		if v := plain(m); !strings.Contains(v, "mic: Built-in Audio Analog Stereo") {
			t.Fatalf("%d cols: note not on screen:\n%s", w, v)
		}
		if m.hud.aecHinted {
			t.Fatal("hint recorded as shown before it was")
		}
		// The note's notice expires: the headphones hint follows.
		m.Update(noticeExpireMsg{m.noticeGen})
		if v := plain(m); !strings.Contains(v, headphonesHint) || !m.hud.aecHinted {
			t.Fatalf("%d cols: hint didn't follow the note:\n%s", w, v)
		}
		runInput(t, m, "/huddle")
		want := "echo: gate only, use headphones"
		if w >= 120 {
			want += " · mic: Built-in"
		}
		if v := plain(m); !strings.Contains(v, want) {
			t.Fatalf("%d cols: /huddle:\n%s", w, v)
		}
	}
	m, h := newHuddleModel(t, 40, 12)
	h.note = "no microphone, listening only · default input is a speaker monitor"
	runInput(t, m, "/huddle")
	if v := ansi.Strip(checkView(t, m, "no mic")); !strings.Contains(v, "no microphone") {
		t.Fatalf("40 cols: listen-only not on screen:\n%s", v)
	}
}

// The hint must be on screen wherever it shows, sidebar or not, also when
// switching from another huddle.
func TestHuddleEchoHintFits(t *testing.T) {
	for _, w := range []int{40, 60, 80, 120} {
		m, h := newHuddleModel(t, w, 12)
		h.aec = "gate"
		h.in = 99 // switching from another huddle
		runInput(t, m, "/huddle")
		if v := ansi.Strip(checkView(t, m, "hint")); !strings.Contains(v, "headphones") {
			t.Errorf("%d cols: hint not on screen:\n%s", w, v)
		}
		runInput(t, m, "/huddle")
		checkView(t, m, "info")
	}
	m, h := newHuddleModel(t, 20, 12) // tiny: only has to fit
	h.aec = "gate"
	runInput(t, m, "/huddle")
	checkView(t, m, "hint")
}
