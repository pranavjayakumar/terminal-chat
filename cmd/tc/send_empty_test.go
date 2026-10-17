package main

import (
	"os"
	"strings"
	"testing"
)

// Empty or whitespace-only input is refused before any connection, never
// wrapped into an empty code block.
func TestSendRefusesEmptyInput(t *testing.T) {
	for _, in := range []string{"", "\n\n", "  \n", "\x1b[31m\x1b[0m\n"} {
		f, err := os.CreateTemp(t.TempDir(), "stdin")
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(in)
		f.Seek(0, 0)
		old := os.Stdin
		os.Stdin = f
		err = cmdSend([]string{"#general"})
		os.Stdin = old
		f.Close()
		if err == nil || !strings.Contains(err.Error(), "nothing to send") {
			t.Errorf("stdin %q: err %v", in, err)
		}
	}
}
