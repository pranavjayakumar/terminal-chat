package tui

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

// TestStrWidth_BreakTwoEmDash: the same pattern as screen.layText's crash
// (internal/screen/grid_breaker_watch_test.go): strWidth measures a
// grapheme uniseg says is wider than 2 "one rune at a time" by calling
// itself on string(r); U+2E3A/U+2E3B are single runes of width 3/4, so it
// recurses until a fatal stack overflow. Message text is only sanitized,
// which keeps both dashes, so any member posting "⸻" would take down the
// TUI of everyone who renders it (canvas.put has the same per-rune
// fallback). Runs in a child process: a stack overflow is fatal.
func TestStrWidth_BreakTwoEmDash(t *testing.T) {
	if os.Getenv("TC_BREAK_CHILD") == "strwidth" {
		_ = strWidth("a \u2e3b b")
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStrWidth_BreakTwoEmDash$")
	cmd.Env = append(os.Environ(), "TC_BREAK_CHILD=strwidth")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if i := bytes.IndexByte(out, '\n'); i > 0 {
			out = out[:i]
		}
		t.Fatalf("strWidth(\"\u2e3b\") crashed the process: %v: %s", err, out)
	}
}
