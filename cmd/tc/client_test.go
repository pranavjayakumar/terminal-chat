package main

import (
	"strings"
	"testing"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

func TestStripANSI(t *testing.T) {
	in := "\x1b[31mFAIL\x1b[0m: test\r\n\x1b]0;title\x07ok\tdone\x1b]8;;http://x\x1b\\link"
	got := stripANSI(in)
	if got != "FAIL: test\nok    donelink" {
		t.Fatalf("got %q", got)
	}
	if !sanitize.Valid(got) {
		t.Fatal("output not valid message text")
	}
}

func TestPipedBody(t *testing.T) {
	if got := pipedBody("hello\n", false); got != "```\nhello\n```" {
		t.Fatalf("got %q", got)
	}
	if got := pipedBody("a```b", true); got != "a```b" {
		t.Fatalf("raw: %q", got)
	}
	big := pipedBody(strings.Repeat("日本語ログ\n", 20000), false)
	if len(big) > proto.MaxBody || !sanitize.Valid(big) || !strings.Contains(big, "truncated") {
		t.Fatalf("truncation: len=%d valid=%v", len(big), sanitize.Valid(big))
	}
}
