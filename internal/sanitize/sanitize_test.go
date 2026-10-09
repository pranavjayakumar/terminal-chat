package sanitize

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestText(t *testing.T) {
	cases := map[string]string{
		"hello":                  "hello",
		"line1\nline2":           "line1\nline2",
		"a\tb":                   "a    b",
		"\x1b[31mred\x1b[0m":     "[31mred[0m",
		"\x1b]0;pwned\x07title":  "]0;pwnedtitle",
		"\x1b]52;c;aGk=\x07clip": "]52;c;aGk=clip",
		"bell\x07":               "bell",
		"cr\roverwrite":          "croverwrite",
		"\u009b31mcsi":           "31mcsi",
		"evil‮gnp.exe":           "evilgnp.exe",
		"iso⁦late⁩":              "isolate",
		"bad\xffutf8":            "bad�utf8",
		"emoji 👍🏽 and 👨‍👩‍👧 ok": "emoji 👍🏽 and 👨‍👩‍👧 ok",
		"日本語": "日本語",
	}
	for in, want := range cases {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLine(t *testing.T) {
	if got := Line("a\nb\x1b"); got != "a b" {
		t.Fatalf("got %q", got)
	}
}

func TestValid(t *testing.T) {
	for _, s := range []string{"hi", "a\nb\tc", "日本 👍"} {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false", s)
		}
	}
	for _, s := range []string{"\x1b[0m", "a\rb", "\xff", "\u0085", "\x7f"} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true", s)
		}
	}
}

func FuzzSanitize(f *testing.F) {
	for _, s := range []string{"hi", "\x1b[2J", "‮", "\xff\xfe", "a\tb\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := Text(s)
		if !utf8.ValidString(out) {
			t.Fatalf("invalid utf8 output for %q", s)
		}
		if strings.ContainsAny(out, "\x1b\r\x07\x00\t\u009b‮") {
			t.Fatalf("unsafe rune survived: %q -> %q", s, out)
		}
		for _, r := range out {
			if r != '\n' && unsafe(r) {
				t.Fatalf("unsafe rune %U survived in %q", r, out)
			}
		}
		if !Valid(Line(s)) {
			t.Fatalf("Line output not Valid: %q", Line(s))
		}
	})
}
