package client

import (
	"strings"
	"testing"
)

// Server-supplied BYE text is printed by the CLI; it must not carry escapes.
func TestByeErrorIsTerminalSafe(t *testing.T) {
	e := &ByeError{Code: "revoked\x1b]0;x\a", Msg: "\x1b]52;c;cHduZWQ=\a bye\nnow"}
	got := e.Error()
	if strings.ContainsAny(got, "\x1b\a\n") {
		t.Fatalf("unsafe error text: %q", got)
	}
}
