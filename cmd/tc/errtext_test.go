package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// Multi-line explanations (no key has an account, key-bound invite) keep
// their lines; control bytes are still stripped from each.
func TestErrorTextKeepsLines(t *testing.T) {
	got := errorText(errors.New("none of your keys\n  - a (fp)\x1b[31m\n  next step"))
	if got != "none of your keys\n  - a (fp)[31m\n  next step" && got != "none of your keys\n  - a (fp)\n  next step" {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "\x1b") {
		t.Fatal("escape kept")
	}
}

// tc login on a computer that never joined explains instead of dumping usage.
func TestShowLoginNotJoined(t *testing.T) {
	var out bytes.Buffer
	err := showLogin(&out, filepath.Join(t.TempDir(), "tc"))
	if err == nil || errors.Is(err, errUsage) || !strings.Contains(err.Error(), "tc join") || !strings.Contains(err.Error(), "isn't signed in") {
		t.Fatalf("err = %v", err)
	}
}
