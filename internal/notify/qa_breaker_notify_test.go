package notify

import (
	"strings"
	"testing"
)

// The freedesktop notification spec lets the server interpret the body as
// markup (<b>, <i>, <a href>, <img src>) when it has the body-markup
// capability (GNOME Shell, dunst, KDE, mako all do). Remote message text
// goes into the notify-send body unescaped, so a hostile message can
// render as bold/links/images, and an innocent "a < b && c" can make the
// server drop or mangle the whole body. The client is supposed to escape
// &, < and > (spec "Markup" section).
func TestNotifySend_BreakBodyMarkupNotEscaped(t *testing.T) {
	t.Skip("issue #98")
	n := &Notifier{backend: NotifySnd}
	body := clean(`<a href="https://evil.example/login">your session expired, click</a> <img src="/etc/passwd"> a < b && c`, maxBody)
	argv := n.argv(Note{Title: "#general · bob", Body: body, Cue: CueMention})
	got := argv[len(argv)-1]
	for _, bad := range []string{"<a ", "<img", "< b", "&&"} {
		if strings.Contains(got, bad) {
			t.Fatalf("notify-send body carries raw markup %q: %q", bad, got)
		}
	}
}

// herdr gets the title as a bare positional argument with no "--" before it
// (osascript and notify-send both get "--"). A title beginning with "-"
// (a DM title is "<name> (DM)", and names come from the server) is parsed
// by herdr's argument parser as an option instead of the title.
func TestHerdrArgv_BreakTitleReadAsOption(t *testing.T) {
	t.Skip("issue #99")
	n := &Notifier{backend: Herdr}
	for _, title := range []string{"-h (DM)", "--sound=none (DM)", "--help"} {
		argv := n.argv(Note{Title: clean(title, maxTitle), Body: "hi", Cue: CueMention})
		// Find the title element: everything after "show".
		i := 0
		for i < len(argv) && argv[i] != "show" {
			i++
		}
		rest := argv[i+1:]
		sawDashDash := false
		for _, a := range rest {
			if a == "--" {
				sawDashDash = true
				continue
			}
			if a == clean(title, maxTitle) && !sawDashDash && strings.HasPrefix(a, "-") {
				t.Fatalf("title %q reaches herdr as an option-looking argument: %q", title, argv)
			}
		}
	}
}
