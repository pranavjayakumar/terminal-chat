package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

func adminModel(t *testing.T) (*Model, *fakeClient) {
	t.Helper()
	m, f := newTestModelFake(t, 100, 24)
	f.me.Admin = true
	f.users[0].Admin = true
	m.reloadAll()
	return m, f
}

func TestGroupCommandSetsAndDeletes(t *testing.T) {
	m, f := adminModel(t)
	submitCmd(m, "/group oncall @bob carol @bob")
	run(m, nil)
	if !f.called("group:oncall:[2 3]") {
		t.Fatalf("calls %v", f.calls)
	}
	if m.input.Value() != "" {
		t.Fatalf("input kept: %q", m.input.Value())
	}
	// The callThen result arrives as a message.
	runInput(t, m, "/group Backend @alice")
	if !f.called("group:backend:[1]") {
		t.Fatalf("name not normalized: %v", f.calls)
	}
	if !strings.Contains(m.notice, "@backend is now 1 person") {
		t.Fatalf("notice %q", m.notice)
	}

	// No users deletes an existing group.
	runInput(t, m, "/group oncall")
	if !f.called("group:oncall:[]") {
		t.Fatalf("delete not sent: %v", f.calls)
	}
	if !strings.Contains(m.notice, "deleted group @oncall") {
		t.Fatalf("notice %q", m.notice)
	}
	if got := f.Groups(); len(got) != 1 || got[0].Name != "backend" {
		t.Fatalf("groups %v", got)
	}
}

// "me" is reserved (#113): no new "me" group, and @me in a command is me,
// never a legacy account named "me". An old "me" group can be deleted.
func TestGroupCommandMe(t *testing.T) {
	m, f := adminModel(t)
	f.users = append(f.users, proto.User{ID: 77, Name: "me"})
	m.reloadUsers()
	submitCmd(m, "/group me @bob")
	if !m.noticeErr || !strings.Contains(m.notice, "reserved") || f.called("group:me:[2]") {
		t.Fatalf("notice %q calls %v", m.notice, f.calls)
	}
	runInput(t, m, "/group oncall @me @bob")
	if !f.called("group:oncall:[1 2]") || f.called("group:oncall:[2 77]") {
		t.Fatalf("@me went to someone else: %v", f.calls)
	}
	f.groups = append(f.groups, proto.Group{Name: "me", Members: []uint64{2}})
	runInput(t, m, "/group me")
	if !f.called("group:me:[]") {
		t.Fatalf("delete of an old me group not sent: %v", f.calls)
	}
}

// runInput types a command, presses Enter and plays the resulting commands.
func runInput(t *testing.T, m *Model, line string) {
	t.Helper()
	m.input.SetValue(line)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	run(m, cmd)
}

func TestGroupCommandErrorsKeepTheInput(t *testing.T) {
	m, f := adminModel(t)
	f.groups = []proto.Group{{Name: "oncall", Members: []uint64{2}}}
	for _, tc := range []struct{ line, want string }{
		{"/group", "usage"},
		{"/group Bad!Name @bob", "group names"},
		{"/group ci-bot @bob", "reserved for webhook bots"},
		{"/group " + strings.Repeat("a", 33) + " @bob", "group names"},
		{"/group bob @carol", "is a user's name"},
		{"/group @alice @carol", "is a user's name"},
		{"/group oncall @nobody", "unknown user"},
		{"/group nosuch", "no group @nosuch"},
	} {
		m.input.SetValue(tc.line)
		press(m, "enter")
		if !m.noticeErr || !strings.Contains(m.notice, tc.want) || m.input.Value() != tc.line {
			t.Errorf("%q: err=%v notice=%q input=%q, want %q", tc.line, m.noticeErr, m.notice, m.input.Value(), tc.want)
		}
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "group:") {
			t.Fatalf("a rejected command reached the backend: %v", f.calls)
		}
	}
	// 51 members is refused locally.
	f.users = nil
	m.reloadUsers()
	var sb strings.Builder
	sb.WriteString("/group big")
	for i := 0; i < proto.MaxGroupMembers+1; i++ {
		f.users = append(f.users, proto.User{ID: uint64(100 + i), Name: "u" + string(rune('a'+i%26)) + string(rune('a'+i/26))})
		sb.WriteString(" @" + f.users[i].Name)
	}
	m.reloadUsers()
	m.input.SetValue(sb.String())
	press(m, "enter")
	if !m.noticeErr || !strings.Contains(m.notice, "at most 50") {
		t.Fatalf("notice %q", m.notice)
	}
}

func TestGroupCommandAdminOnly(t *testing.T) {
	m, f := newTestModelFake(t, 100, 24) // alice isn't an admin
	submitCmd(m, "/group oncall @bob")
	if !m.noticeErr || !strings.Contains(m.notice, "only admins") || m.input.Value() == "" {
		t.Fatalf("non-admin: err=%v notice=%q", m.noticeErr, m.notice)
	}
	if f.called("group:oncall:[2]") {
		t.Fatal("sent anyway")
	}
	// Listing is for everyone.
	m.input.Reset()
	submitCmd(m, "/groups")
	if m.noticeErr || !strings.Contains(m.notice, "no groups") {
		t.Fatalf("/groups: %q", m.notice)
	}
}

func TestGroupsOverlay(t *testing.T) {
	m, f := newTestModelFake(t, 100, 24)
	f.users[1].Name = "bob\x1b]0;pwned\a"
	m.reloadUsers()
	f.groups = []proto.Group{
		{Name: "backend", Members: []uint64{3, 2}},
		{Name: "oncall", Members: []uint64{1, 3}},
	}
	submitCmd(m, "/groups")
	if m.mode != modeList {
		t.Fatalf("mode %v, notice %q", m.mode, m.notice)
	}
	v := checkView(t, m, "groups")
	for _, want := range []string{"2 groups", "@backend: bob", "carol", "@oncall: alice, carol (you're in it)"} {
		if !strings.Contains(v, want) {
			t.Errorf("overlay lacks %q:\n%s", want, v)
		}
	}
	for _, l := range strings.Split(m.View(), "\n") {
		if strings.ContainsAny(l, "\a") || strings.Contains(l, "\x1b]") {
			t.Errorf("terminal escape reached the screen: %q", l)
		}
	}
	press(m, "down")
	if m.list.sel != 1 {
		t.Fatalf("sel %d", m.list.sel)
	}
	press(m, "esc")
	if m.mode != modeNormal {
		t.Fatalf("mode %v after Esc", m.mode)
	}
	for _, size := range [][2]int{{20, 6}, {40, 8}, {120, 40}, {10, 4}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		submitCmd(m, "/groups")
		checkView(t, m, "groups small")
		press(m, "esc")
	}
}

func TestGroupMentionsNotifyMembersOnly(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	clk := newClock(m)
	nt := notes(m)
	f.groups = []proto.Group{{Name: "oncall", Members: []uint64{1, 3}}, {Name: "backend", Members: []uint64{2}}}
	run(m, m.applyUpdate(client.Update{Channels: true})) // a group.update arrived

	deliver(m, newMsg(301, 11, 3, "paging @oncall please look"))
	want := [][2]string{{"#backend · carol", "paging @oncall please look"}}
	if got := nt.got(); !slices.Equal(got, want) {
		t.Fatalf("member: %q", got)
	}

	clk.add(10 * time.Second)
	deliver(m,
		newMsg(302, 11, 3, "@backend you're not in that one"), // alice isn't in @backend
		newMsg(303, 11, 3, "@oncalls or x@oncall or @oncall-2"),
		newMsg(304, 11, 1, "@oncall my own message"),
		newMsg(305, 10, 2, "@oncall in the open, focused channel"),
	)
	if got := nt.got(); !slices.Equal(got, want) {
		t.Fatalf("ignored cases notified: %q", got)
	}

	// A bot never triggers one.
	f.users = append(f.users, proto.User{ID: 9, Name: "ci-bot", Bot: true})
	m.reloadUsers()
	clk.add(10 * time.Second)
	deliver(m, newMsg(306, 11, 9, "build failed @oncall"))
	if got := nt.got(); !slices.Equal(got, want) {
		t.Fatalf("bot notified: %q", got)
	}

	// Leaving the group (an update) stops them; joining another starts them.
	f.groups = []proto.Group{{Name: "oncall", Members: []uint64{3}}, {Name: "backend", Members: []uint64{1, 2}}}
	run(m, m.applyUpdate(client.Update{Channels: true}))
	clk.add(10 * time.Second)
	deliver(m, newMsg(307, 11, 3, "@oncall again"))
	if got := nt.got(); !slices.Equal(got, want) {
		t.Fatalf("after leaving the group: %q", got)
	}
	clk.add(10 * time.Second)
	deliver(m, newMsg(308, 13, 3, "@backend now it is for me"))
	want = append(want, [2]string{"#secret · carol", "@backend now it is for me"})
	if got := nt.got(); !slices.Equal(got, want) {
		t.Fatalf("after joining another group: %q", got)
	}
}

func TestGroupMentionMarksTheLog(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	if m.concernMark("hi @oncall") != " " {
		t.Fatal("marked before the group exists")
	}
	f.groups = []proto.Group{{Name: "oncall", Members: []uint64{1}}}
	run(m, m.applyUpdate(client.Update{Channels: true}))
	if ansi.Strip(m.concernMark("hi @oncall")) != "▎" || !m.concernsMe("hi @ONCALL") {
		t.Fatal("group mention not marked")
	}
	if m.concernMark("hi @oncalls") != " " || m.concernsMe("x@oncall") {
		t.Fatal("near-miss marked")
	}
	// The cached rendering is refreshed when the groups change.
	v := ansi.Strip(m.View())
	f.groups = nil
	run(m, m.applyUpdate(client.Update{Channels: true}))
	if m.concernsMe("hi @oncall") {
		t.Fatal("still marked after the group was deleted")
	}
	_ = v
}
