package main

import (
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

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

func TestTailBuffer(t *testing.T) {
	tb := newTailBuffer(3)
	for _, chunk := range []string{"one\ntw", "o\nthree\nfour", "\nfive"} {
		if n, _ := tb.Write([]byte(chunk)); n != len(chunk) {
			t.Fatalf("Write returned %d for %d bytes", n, len(chunk))
		}
	}
	if got := tb.String(); got != "three\nfour\nfive" {
		t.Fatalf("got %q", got)
	}
	big := newTailBuffer(2)
	big.Write([]byte(strings.Repeat("x", 10000) + "\nend\n"))
	if got := big.String(); len(got) > maxTailLine+10 || !strings.HasSuffix(got, "\nend") {
		t.Fatalf("long line not capped: %d bytes", len(got))
	}
	if newTailBuffer(0).String() != "" {
		t.Fatal("zero tail")
	}
}

func TestNotifyBody(t *testing.T) {
	ok := notifyBody([]string{"make", "build"}, 0, 83*time.Second, "noise")
	if ok != "✓ `make build` · 1m23s" {
		t.Fatalf("success: %q", ok)
	}
	fail := notifyBody([]string{"go", "test", "./`x`"}, 2, 400*time.Millisecond, "\x1b[31mFAIL\x1b[0m pkg\n")
	if !strings.HasPrefix(fail, "✗ `go test ./'x'` · exit 2 · 1s\n```\nFAIL pkg\n```") {
		t.Fatalf("failure: %q", fail)
	}
	if !sanitize.Valid(fail) {
		t.Fatal("failure body not valid message text")
	}
}
