package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

// With an account named "me" from before the name was reserved (#113),
// @me in a command is still you: /remove @me doesn't leave or remove them,
// /add @me doesn't claim success, and completion never offers them.
func TestAtMeNeverALegacyAccount(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	f.users = append(f.users, proto.User{ID: 77, Name: "me"})
	m.reloadUsers()
	run(m, m.openChannel(13)) // #secret, private
	for _, tc := range []struct{ in, want string }{
		{"/remove @me", "/leave to go yourself"},
		{"/add @me", "already here"},
	} {
		submitLine(m, tc.in)
		if !m.noticeErr || !strings.Contains(m.notice, tc.want) || m.input.Value() != tc.in {
			t.Errorf("%q: notice %q input %q", tc.in, m.notice, m.input.Value())
		}
		m.input.SetValue("")
	}
	for _, c := range f.calls {
		if strings.Contains(c, "77") || strings.HasPrefix(c, "remove") || strings.HasPrefix(c, "add") || strings.HasPrefix(c, "leave") {
			t.Fatalf("backend called: %v", f.calls)
		}
	}
	for _, in := range []string{"/add @m", "/verify @m", "@m"} {
		m.input.SetValue("")
		typeKeys(m, in)
		for _, it := range m.comp.items {
			if strings.Contains(it.desc, "77") || it.text == "@me" && !strings.HasPrefix(in, "/dm") {
				t.Fatalf("%q offers %+v", in, m.comp.items)
			}
		}
		if slices.ContainsFunc(m.comp.items, func(it compItem) bool { return it.text == "@me" }) {
			t.Fatalf("%q offers the legacy @me account: %+v", in, m.comp.items)
		}
	}
}
