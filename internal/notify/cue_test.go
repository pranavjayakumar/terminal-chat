package notify

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every backend × cue: the sound comes from a fixed table, hints go before
// "--", and a silent popup asks for no sound.
func TestCueArgv(t *testing.T) {
	cases := []struct {
		backend string
		nt      Note
		want    []string
	}{
		{Herdr, Note{Title: "t", Body: "b", Cue: CueMention}, []string{"herdr", "notification", "show", "t", "--body", "b", "--sound", "request"}},
		{Herdr, Note{Title: "t", Cue: CueRing}, []string{"herdr", "notification", "show", "t", "--sound", "request"}},
		{Herdr, Note{Title: "t", Cue: CueHuddle}, []string{"herdr", "notification", "show", "t", "--sound", "done"}},
		{Herdr, Note{Title: "t", Cue: CueMessage}, []string{"herdr", "notification", "show", "t", "--sound", "none"}},
		{Herdr, Note{Title: "t", Cue: CueNone}, []string{"herdr", "notification", "show", "t", "--sound", "none"}},
		{OSAScript, Note{Title: "t", Body: "b", Cue: CueRing}, []string{"osascript", "-e", osaScript, "--", "t", "b", "Submarine"}},
		{OSAScript, Note{Title: "t", Body: "b", Cue: CueMessage}, []string{"osascript", "-e", osaScript, "--", "t", "b", "Tink"}},
		{OSAScript, Note{Title: "t", Body: "b", Cue: CueHuddle}, []string{"osascript", "-e", osaScript, "--", "t", "b", "Pop"}},
		{OSAScript, Note{Title: "t", Body: "b", Cue: CueNone}, []string{"osascript", "-e", osaScript, "--", "t", "b"}},
		{NotifySnd, Note{Title: "t", Body: "b", Cue: CueRing, Tag: "tc-20"}, []string{"notify-send", "--app-name=tc", "--category=call.incoming", "--urgency=critical",
			"--hint=string:sound-name:phone-incoming-call", "--hint=string:x-dunst-stack-tag:tc-20", "--hint=string:x-canonical-private-synchronous:tc-20", "--", "t", "b"}},
		{NotifySnd, Note{Title: "t", Body: "b", Cue: CueNone}, []string{"notify-send", "--app-name=tc", "--category=im.received", "--hint=boolean:suppress-sound:true", "--", "t", "b"}},
		{NotifySnd, Note{Title: "t", Body: "b", Cue: CueHuddle}, []string{"notify-send", "--app-name=tc", "--category=call", "--hint=string:sound-name:service-login", "--", "t", "b"}},
	}
	for _, c := range cases {
		n := &Notifier{backend: c.backend}
		if got := n.argv(c.nt); !slices.Equal(got, c.want) {
			t.Errorf("%s %+v\n got %q\nwant %q", c.backend, c.nt, got, c.want)
		}
	}
}

// A tag that isn't short [a-z0-9-] never reaches a hint.
func TestSendDropsBadTag(t *testing.T) {
	log := fakeBin(t, "notify-send", "")
	e := OSEnv()
	e.Getenv = func(string) string { return "" }
	e.GOOS = "linux"
	n := New(e, nil)
	n.timeout = slowExec
	n.Send(Note{Title: "t", Body: "b", Cue: CueMention, Tag: "x --hint=evil"})
	n.Wait()
	for _, a := range readArgv(t, log) {
		if strings.Contains(a, "evil") || strings.Contains(a, "stack-tag") {
			t.Fatalf("bad tag reached argv: %q", a)
		}
	}
}

// A hung message popup can't block a ring: rings have their own slot.
func TestRingHasOwnSlot(t *testing.T) {
	fakeBin(t, "herdr", "sleep 5\n")
	n := newWith(t, map[string]string{"HERDR_ENV": "1"})
	n.timeout = 3 * time.Second
	if !n.Send(Note{Title: "a", Cue: CueMention}) {
		t.Fatal("first dropped")
	}
	if n.Send(Note{Title: "b", Cue: CueMessage}) {
		t.Fatal("second message should be dropped while one is in flight")
	}
	if !n.Send(Note{Title: "ring", Cue: CueRing}) {
		t.Fatal("ring dropped behind a hung message popup")
	}
	n.Wait()
}

func TestBellSilentCue(t *testing.T) {
	var buf syncBuf
	n := &Notifier{backend: Bell, bell: &buf, timeout: time.Second}
	n.Send(Note{Title: "t", Cue: CueNone})
	n.Wait()
	n.Send(Note{Title: "t", Cue: CueMention})
	n.Wait()
	if buf.String() != "\a" {
		t.Fatalf("bell wrote %q", buf.String())
	}
}

func TestPickTerminal(t *testing.T) {
	ssh := map[string]string{"SSH_CONNECTION": "1 2 3 4"}
	with := func(base map[string]string, kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cases := []struct {
		name string
		vars map[string]string
		bins []string
		want string
	}{
		{"ssh kitty", with(ssh, "TERM", "xterm-kitty"), []string{"notify-send"}, Terminal},
		{"ssh ghostty", with(ssh, "TERM", "xterm-ghostty"), []string{"osascript"}, Terminal},
		{"ssh iterm", with(ssh, "LC_TERMINAL", "iTerm2"), []string{"osascript"}, Terminal},
		{"ssh unknown terminal", with(ssh, "TERM", "xterm-256color"), []string{"osascript"}, OSAScript},
		{"local kitty keeps os", map[string]string{"TERM": "xterm-kitty"}, []string{"osascript"}, OSAScript},
		{"herdr over ssh", with(ssh, "TERM", "xterm-kitty", "HERDR_ENV", "1"), []string{"herdr", "osascript"}, Herdr},
		{"forced terminal", map[string]string{"TC_NOTIFY": "terminal", "TERM_PROGRAM": "WezTerm"}, []string{"osascript"}, Terminal},
		{"forced terminal unknown", map[string]string{"TC_NOTIFY": "terminal"}, []string{"osascript"}, Bell},
		{"forced dialect", map[string]string{"TC_NOTIFY": "terminal", "TC_NOTIFY_OSC": "99"}, nil, Terminal},
	}
	for _, c := range cases {
		if got := pick(fakeEnv(c.vars, c.bins...)); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestOSCSeqGolden(t *testing.T) {
	cases := []struct {
		dialect int
		title   string
		body    string
		cue     Cue
		tmux    bool
		want    string
	}{
		{9, "#dev · bo", "hi", CueMention, false, "\x1b]9;#dev · bo: hi\x1b\\"},
		{9, "4;3 lol", "x", CueMention, false, "\x1b]9;tc · 4;3 lol: x\x1b\\"},
		{777, "a;b", "c;d", CueMention, false, "\x1b]777;notify;a,b;c;d\x1b\\"},
		{99, "t", "b", CueRing, false, "\x1b]99;i=tc7:d=0:p=title:e=1:o=always:u=2;dA==\x1b\\\x1b]99;i=tc7:p=body:e=1;Yg==\x1b\\"},
		{99, "t", "b", CueNone, false, "\x1b]99;i=tc7:d=0:p=title:e=1:o=always:s=c2lsZW50;dA==\x1b\\\x1b]99;i=tc7:p=body:e=1;Yg==\x1b\\"},
		{9, "t", "b", CueMention, true, "\x1bPtmux;\x1b\x1b]9;t: b\x1b\x1b\\\x1b\\\a"},
		{9, "t", "b", CueNone, true, "\x1bPtmux;\x1b\x1b]9;t: b\x1b\x1b\\\x1b\\"},
	}
	for _, c := range cases {
		if got := string(oscSeq(c.dialect, c.title, c.body, c.cue, c.tmux, 7)); got != c.want {
			t.Errorf("%d %q\n got %q\nwant %q", c.dialect, c.title, got, c.want)
		}
	}
	if oscSeq(0, "t", "b", CueMention, false, 1) != nil {
		t.Error("dialect 0 wrote something")
	}
}

func TestTerminalBackendWrites(t *testing.T) {
	var buf syncBuf
	n := &Notifier{backend: Terminal, dialect: 777, bell: &buf, timeout: time.Second}
	n.Send(Note{Title: hostile, Body: hostile, Cue: CueMention})
	n.Wait()
	out := buf.String()
	if !strings.HasPrefix(out, "\x1b]777;notify;") || strings.Count(out, "\x1b") != 2 || strings.Contains(out, "\a") {
		t.Fatalf("terminal wrote %q", out)
	}
}

// The invariant: exactly one introducer and terminator per sequence and no
// other control bytes, whatever the text.
func FuzzOSCSeq(f *testing.F) {
	f.Add("#dev · bo", "hi", uint8(2))
	f.Add("\x1b]0;x\x07", "\x9d\x9c\u009b", uint8(0))
	f.Add("9;4;1", "", uint8(3))
	f.Fuzz(func(t *testing.T, title, body string, cue uint8) {
		title, body = clean(title, maxTitle), clean(body, maxBody)
		for _, d := range []int{9, 777, 99} {
			for _, tm := range []bool{false, true} {
				b := oscSeq(d, title, body, Cue(cue%5), tm, 1)
				s := string(b)
				if tm {
					if !strings.HasPrefix(s, "\x1bPtmux;") {
						t.Fatalf("tmux prefix: %q", s)
					}
					s = strings.TrimSuffix(strings.TrimSuffix(s, "\a"), "\x1b\\")
					s = strings.ReplaceAll(strings.TrimPrefix(s, "\x1bPtmux;"), "\x1b\x1b", "\x1b")
				}
				seqs := 1
				if d == 99 {
					seqs = 2
				}
				if strings.Count(s, "\x1b]") != seqs || strings.Count(s, "\x1b\\") != seqs || strings.Count(s, "\x1b") != 2*seqs {
					t.Fatalf("dialect %d: framing %q", d, s)
				}
				if bytes.ContainsAny([]byte(s), "\a\x00\x18\x1a\x7f") {
					t.Fatalf("dialect %d: control byte in %q", d, s)
				}
				for _, r := range s {
					if r >= 0x80 && r <= 0x9f {
						t.Fatalf("dialect %d: C1 in %q", d, s)
					}
				}
			}
		}
	})
}
