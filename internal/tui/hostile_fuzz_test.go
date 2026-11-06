package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// FuzzHostileRender draws a message body, a sender's name and display
// name, and a channel topic chosen by someone else at a few terminal sizes:
// every frame must fit and no control sequence may reach the terminal.
func FuzzHostileRender(f *testing.F) {
	seeds := []string{
		"\x1b]8;;http://evil\x1b\\click\x1b]8;;\x1b\\",
		"\x1b[2J\x1b[H\x9b31m\u009d0;title\a",
		"a\u202eevil\u2066x\u2069\u200f",
		"e" + strings.Repeat("\u0301", 200),
		strings.Repeat("\u4f60\u597d", 300),
		strings.Repeat("\U0001f469\u200d\U0001f469\u200d\U0001f467\u200d\U0001f466", 50),
		"\u200b\u200c\u200d\u2060\ufeff\u00ad",
		strings.Repeat("x", 5000),
		"\xff\xfe\xc0\x80 bad utf8",
		"\u0e01\u0e34\u0e34\u0e34\u0e34 thai stack",
		"\U000E0041\U000E0042 tags \U0001f3f4\U000E0067\U000E0062\U000E007F",
		"\u1100\u1161\u11a8 jamo \u115f\u1160",
		"\u2028line\u2029para",
		"```\n\x1b[31mcode\n```",
		"> quote\n> \x1b]52;c;Zm9v\a",
	}
	for _, s := range seeds {
		f.Add(s, "bob", "Bob", "topic")
		f.Add("hi", s, s, s)
	}
	f.Fuzz(func(t *testing.T, body, name, display, topic string) {
		for _, sz := range [][2]int{{40, 12}, {80, 24}, {33, 9}} {
			m, fc := newTestModelFake(t, sz[0], sz[1])
			for i := range fc.users {
				if fc.users[i].ID == 2 {
					fc.users[i].Name, fc.users[i].Display = name, display
				}
			}
			for i := range fc.chans {
				if fc.chans[i].ID == 10 {
					fc.chans[i].Topic = topic
				}
			}
			ms := fc.msgs[10]
			ms = append(ms, client.Message{Message: proto.Message{ID: 150, Chan: 10, Author: 2, Body: body, Time: ms[len(ms)-3].Time + 1}})
			fc.msgs[10] = ms
			fc.threads[103] = append(fc.threads[103], client.Message{Message: proto.Message{ID: 202, Chan: 10, Thread: 103, Author: 2, Body: body}})
			m.Update(updateMsg(client.Update{Reset: true, Channels: true, Users: true}))
			run(m, m.openChannel(10))
			hostileCheck(t, checkView(t, m, "open"))
			m.enterSelect()
			for i, x := range m.main.msgs {
				if x.ID == 103 {
					m.main.sel = i
				}
			}
			run(m, m.handleKey(keyMsg("r")))
			hostileCheck(t, checkView(t, m, "thread"))
			run(m, m.handleKey(keyMsg("esc")))
			m.enterSelect()
			for i, x := range m.main.msgs {
				if x.ID == 150 {
					m.main.sel = i
				}
			}
			run(m, m.handleKey(keyMsg("v")))
			hostileCheck(t, checkView(t, m, "viewer"))
		}
	})
}

// hostileCheck fails on anything a terminal could take as a control in a
// frame with its styling stripped: C0 (but newline), DEL, C1, bidi
// controls, invalid UTF-8.
func hostileCheck(t *testing.T, plain string) {
	t.Helper()
	if !utf8.ValidString(plain) {
		t.Fatalf("invalid UTF-8 reached the terminal")
	}
	for _, r := range plain {
		switch {
		case r < 0x20 && r != '\n', r == 0x7f, r >= 0x80 && r <= 0x9f,
			r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			t.Fatalf("control %U reached the terminal", r)
		}
	}
}
