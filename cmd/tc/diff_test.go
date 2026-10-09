package main

import (
	"strings"
	"testing"
)

func TestPipedBodyDiff(t *testing.T) {
	d := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n"
	if got := pipedBody(d, false); got != "```diff\n"+strings.TrimRight(d, "\n")+"\n```" {
		t.Fatalf("diff: %q", got)
	}
	colored := "\x1b[1mdiff --git a/x b/x\x1b[m\r\n--- a/x\r\n+++ b/x\r\n\x1b[36m@@ -1 +1 @@\x1b[m\r\n\x1b[31m-a\x1b[m\r\n\x1b[32m+b\x1b[m\r\n"
	if got := pipedBody(colored, false); !strings.HasPrefix(got, "```diff\ndiff --git") || strings.ContainsAny(got, "\x1b\r") {
		t.Fatalf("colored diff: %q", got)
	}
	if got := pipedBody(d, true); strings.Contains(got, "```") {
		t.Fatalf("raw must stay raw: %q", got)
	}
	if got := pipedBody("plain\n--- not a diff\n", false); got != "```\nplain\n--- not a diff\n```" {
		t.Fatalf("non-diff: %q", got)
	}
}
