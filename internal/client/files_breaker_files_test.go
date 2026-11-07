package client

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/skailama/tc/internal/media"
)

// defaultIgnorable reports Unicode Default_Ignorable_Code_Point characters
// that render as nothing: invisible in a file name.
func defaultIgnorable(r rune) bool {
	return r == 0x034F || r == 0x17B4 || r == 0x17B5 || r >= 0x180B && r <= 0x180F ||
		r >= 0xFE00 && r <= 0xFE0F || r >= 0xE0100 && r <= 0xE01EF || r == 0x2800 ||
		unicode.Is(unicode.Cf, r) || r == 0x115F || r == 0x1160 || r == 0x3164 || r == 0xFFA0
}

func checkSafeName(t *testing.T, in, got string) {
	t.Helper()
	visible := false
	for _, r := range got {
		if !defaultIgnorable(r) && !unicode.IsSpace(r) && !unicode.Is(unicode.Mn, r) {
			visible = true
		}
	}
	switch {
	case got == "" || !visible:
		t.Fatalf("SafeName(%q) = %q: no visible character", in, got)
	case !utf8.ValidString(got):
		t.Fatalf("SafeName(%q) = %q: invalid UTF-8", in, got)
	case strings.ContainsAny(got, `/\`) || got == "." || got == "..":
		t.Fatalf("SafeName(%q) = %q: a path", in, got)
	case strings.HasPrefix(got, ".") || strings.HasPrefix(got, "-"):
		t.Fatalf("SafeName(%q) = %q: leading dot or dash", in, got)
	case len(got)+len(" (999)") > 255:
		t.Fatalf("SafeName(%q) = %q: too long for a file name with a counter", in, got)
	}
	for _, r := range got {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || defaultIgnorable(r) {
			t.Fatalf("SafeName(%q) = %q: keeps invisible %U", in, got, r)
		}
	}
}

// §16.5: names are made safe — "control, bidi and invisible characters ...
// removed; never empty". SafeName drops Cc/Cf/Co and four Hangul fillers,
// but keeps the other default-ignorable code points (combining grapheme
// joiner U+034F, variation selectors U+FE00–FE0F / U+E0100–E01EF, Mongolian
// FVS U+180B–180D, Khmer U+17B4/17B5) and U+2800 BRAILLE PATTERN BLANK, all
// of which render as nothing. A hostile sender's "͏" becomes an
// invisible, non-empty name; "͏.bashrc" shows as ".bashrc", dodging
// the leading-dot rule as far as the reader can tell.
func TestSafeNameInvisible_BreakFiles(t *testing.T) {
	for _, in := range []string{"͏", "⠀⠀", "️", "͏.bashrc", "\U000E0100-rf"} {
		checkSafeName(t, in, media.SafeName(in))
	}
}

// A name with a code point the filesystem doesn't know (unassigned, or
// newer than the OS's Unicode tables, e.g. a brand-new emoji) can't be
// created on APFS: open/link fail with EILSEQ "illegal byte sequence".
// SafeName keeps such runes (it only drops controls, format, private use
// and fillers) and placeFile returns the error, so on macOS that file can
// never be downloaded (found by FuzzSafeNamePlace; passes on Linux, whose
// filesystems take any bytes).
func TestPlaceFileUnassignedRune_BreakFiles(t *testing.T) {
	dir := t.TempDir()
	tmp, err := os.CreateTemp(dir, ".tc-download-*")
	if err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	name := media.SafeName("report͸.pdf")
	if _, err := placeFile(tmp.Name(), dir, name); err != nil {
		t.Fatalf("placeFile(%q): %v", name, err)
	}
}

// FuzzSafeNamePlace: whatever name a hostile message carries, the saved
// file lands directly in dir, under a safe name, never over another file.
func FuzzSafeNamePlace(f *testing.F) {
	for _, s := range []string{"cat.png", "../../etc/passwd", ".bashrc", "-rf", "a‮gnp.exe", "CON.txt",
		strings.Repeat("é", 200) + ".png", "a/b\\c", "\x00", "\xff\xfe", "..", " . ", "x." + strings.Repeat("y", 300)} {
		f.Add(s)
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, name string) {
		safe := media.SafeName(name)
		if !utf8.ValidString(safe) || safe == "" || strings.ContainsAny(safe, "/\\\x00") ||
			strings.HasPrefix(safe, ".") || strings.HasPrefix(safe, "-") || len(safe) > 240 {
			t.Fatalf("SafeName(%q) = %q", name, safe)
		}
		for _, r := range safe {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				t.Fatalf("SafeName(%q) = %q keeps %U", name, safe, r)
			}
		}
		sub, err := os.MkdirTemp(dir, "d")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(sub)
		for i := 0; i < 2; i++ {
			tmp, err := os.CreateTemp(sub, ".tc-download-*")
			if err != nil {
				t.Fatal(err)
			}
			tmp.Close()
			p, err := placeFile(tmp.Name(), sub, safe)
			os.Remove(tmp.Name())
			if errors.Is(err, syscall.EILSEQ) {
				return // TestPlaceFileUnassignedRune_BreakFiles
			}
			if err != nil {
				t.Fatalf("placeFile(%q): %v", safe, err)
			}
			if filepath.Dir(p) != sub {
				t.Fatalf("placeFile(%q) = %q, outside %q", safe, p, sub)
			}
		}
		if ents, _ := os.ReadDir(sub); len(ents) != 2 {
			t.Fatalf("placeFile(%q) twice left %d files", safe, len(ents))
		}
	})
}
