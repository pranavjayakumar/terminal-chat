package screen

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"os"
	"os/exec"
	"testing"
)

// The crash below is a fatal stack overflow (not a recoverable panic), so
// each case runs in a child process of the test binary.
func inChild(t *testing.T, name string, f func()) {
	t.Helper()
	switch os.Getenv("TC_BREAK_CHILD") {
	case "":
	case name:
		f()
		os.Exit(0)
	default:
		return // a child running another case
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), "TC_BREAK_CHILD="+name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		first := out
		if i := bytes.IndexByte(out, '\n'); i > 0 {
			first = out[:i]
		}
		t.Fatalf("%s crashed the process: %v: %s", name, err, first)
	}
}

// TestParse_BreakTwoEmDash: U+2E3A TWO-EM DASH and U+2E3B THREE-EM DASH
// are single runes uniseg measures 3 and 4 cells wide. layText's "w > 2"
// case lays such a grapheme out "one rune at a time" by calling itself on
// string(r), which is the same one-rune grapheme of width > 2 again:
// unbounded recursion, a fatal stack overflow (grid.go layText). The
// streamer runs Parse on every capture, so a shared pane that merely
// shows "⸻" (man pages, typography, a cat'ed markdown file) kills the
// streamer's tc. Found by FuzzRoundTrip
// (testdata/fuzz/FuzzRoundTrip/ed82ed693dd269bd).
func TestParse_BreakTwoEmDash(t *testing.T) {
	for _, s := range []string{"a \u2e3a b", "\u2e3b"} {
		inChild(t, "parse "+s, func() { Parse(s, 80, 2) })
	}
}

// TestDecode_BreakTwoEmDash: the same recursion on the viewer side. Every
// cell's text goes through decSpan.apply → layText, so a single 1-chunk
// update whose run text is "⸻" (from a hostile streamer, or the server in
// a plaintext huddle, or an honest streamer if Parse didn't crash first)
// kills every viewer's tc.
func TestDecode_BreakTwoEmDash(t *testing.T) {
	var b []byte
	b = append(b, version, typeKey)
	b = binary.BigEndian.AppendUint32(b, 1)
	b = binary.BigEndian.AppendUint32(b, 0)
	b = binary.BigEndian.AppendUint16(b, 10)
	b = binary.BigEndian.AppendUint16(b, 1)
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, 1) // one span
	b = binary.BigEndian.AppendUint16(b, 0) // row
	b = binary.BigEndian.AppendUint16(b, 0) // x
	b = binary.BigEndian.AppendUint16(b, 10)
	b = binary.BigEndian.AppendUint16(b, 1) // one run
	txt := "\u2e3b"
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, uint16(len(txt)))
	b = append(b, txt...)
	var z bytes.Buffer
	w, _ := flate.NewWriter(&z, flate.BestSpeed)
	_, _ = w.Write(b)
	_ = w.Close()
	upd := z.Bytes()
	inChild(t, "decode", func() {
		var d Decoder
		if _, err := d.Apply(upd); err != nil {
			panic(err)
		}
	})
}
