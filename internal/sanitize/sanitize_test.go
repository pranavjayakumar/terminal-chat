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
		"evil\u202egnp.exe":      "evilgnp.exe",
		"iso⁦late⁩":              "isolate",
		"bad\xffutf8":            "bad�utf8",
		"emoji 👍\U0001F3FD and 👨\u200d👩\u200d👧 ok": "emoji 👍\U0001F3FD and 👨\u200d👩\u200d👧 ok",
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
	for _, s := range []string{"hi", "\x1b[2J", "\u202e", "\xff\xfe", "a\tb\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := Text(s)
		if !utf8.ValidString(out) {
			t.Fatalf("invalid utf8 output for %q", s)
		}
		if strings.ContainsAny(out, "\x1b\r\x07\x00\t\u009b\u202e") {
			t.Fatalf("unsafe rune survived: %q -> %q", s, out)
		}
		for _, r := range out {
			if r != '\n' && r != zwj && r != zwnj && !(r >= tagFirst && r <= tagCancel) && unsafe(r) {
				t.Fatalf("unsafe rune %U survived in %q", r, out)
			}
		}
		if !Valid(Line(s)) {
			t.Fatalf("Line output not Valid: %q", Line(s))
		}
		if again := Text(out); again != out {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, out, again)
		}
	})
}

// Invisible and stacking characters can make one name look like another
// ("ad\u200bmin" vs "admin") or smear text over its neighbours.
func TestSpoofing(t *testing.T) {
	zalgo := "a" + strings.Repeat("\u0301", 40) + "b"
	scotland := "🏴\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F"
	for _, tc := range []struct{ in, want string }{
		{"ad\u200bmin", "admin"},                     // zero-width space
		{"ad\u2060min\ufeff", "admin"},               // word joiner, BOM
		{"ad\u00admin", "admin"},                     // soft hyphen
		{"ad\u200dmin", "admin"},                     // ZWJ between letters
		{"ad\u200cmin", "admin"},                     // ZWNJ in Latin
		{"x\U000E0041\U000E0042y", "xy"},             // tag characters on their own
		{"\u3164", ""},                               // Hangul filler: a blank "letter"
		{zalgo, "a\u0301\u0301\u0301b"},              // marks capped
		{"e\ufe0f\ufe0f\ufe0f", "e\ufe0f"},           // one variation selector
		{"👩\u200d💻", "👩\u200d💻"},                     // emoji ZWJ sequence
		{"👨\U0001F3FD\u200d🚀", "👨\U0001F3FD\u200d🚀"}, // with a skin tone
		{"❤\ufe0f\u200d🔥", "❤\ufe0f\u200d🔥"},         // after VS16
		{"1\ufe0f\u20e3", "1\ufe0f\u20e3"},           // keycap
		{scotland, scotland},                         // flag tag sequence
		{"می\u200cخواهم", "می\u200cخواهم"},           // Persian ZWNJ
		{"שָׁלוֹם", "שָׁלוֹם"},                       // Hebrew points
		{"Tiếng Việt", "Tiếng Việt"},
	} {
		if got := Text(tc.in); got != tc.want {
			t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, s := range []string{"", "Alice", "Zoë Smith", "王小明", "👩\u200d💻 dev", "می\u200cخواهم"} {
		if !ValidName(s) {
			t.Errorf("ValidName(%q) = false", s)
		}
	}
	for _, s := range []string{"ad\u200bmin", " alice", "alice ", "   ", "\u0301\u0301", "a\nb", "a\tb", "x\ufe0f\ufe0f",
		"a" + strings.Repeat("\u0301", 4), "\u202eevil", "\u3164", "\xff"} {
		if ValidName(s) {
			t.Errorf("ValidName(%q) = true", s)
		}
	}
}
