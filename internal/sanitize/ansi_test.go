package sanitize

import "testing"

func TestStripANSI(t *testing.T) {
	in := "\x1b[31mFAIL\x1b[0m: test\r\n\x1b]0;title\x07ok\tdone\x1b]8;;http://x\x1b\\link\x1b[1;"
	got := StripANSI(in)
	if got != "FAIL: test\nok    donelink" {
		t.Fatalf("got %q", got)
	}
	if !Valid(got) {
		t.Fatal("output not valid message text")
	}
	if got := StripANSI("bad\xffutf8\x07"); !Valid(got) {
		t.Fatalf("not valid: %q", got)
	}
}
