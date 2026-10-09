package tui

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

func submitCmd(m *Model, line string) {
	m.input.SetValue(line)
	press(m, "enter")
}

func TestHighlightCommands(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	submitCmd(m, "/highlight")
	if !strings.Contains(m.notice, "no highlight words") {
		t.Fatalf("empty list: %q", m.notice)
	}
	submitCmd(m, "/highlight Deploy oncall")
	if got := f.Highlights(); !slices.Equal(got, []string{"deploy", "oncall"}) {
		t.Fatalf("stored %v", got)
	}
	if m.noticeErr || !strings.Contains(m.notice, "deploy, oncall") || m.input.Value() != "" {
		t.Fatalf("add: notice=%q err=%v input=%q", m.notice, m.noticeErr, m.input.Value())
	}
	submitCmd(m, "/highlight deploy")
	if !strings.Contains(m.notice, "already") {
		t.Fatalf("duplicate: %q", m.notice)
	}
	submitCmd(m, "/highlight")
	if !strings.Contains(m.notice, "highlight words: deploy, oncall") {
		t.Fatalf("list: %q", m.notice)
	}

	// Errors keep the input and change nothing.
	submitCmd(m, "/highlight good ---")
	if !m.noticeErr || m.input.Value() != "/highlight good ---" || slices.Contains(f.Highlights(), "good") {
		t.Fatalf("invalid add: err=%v input=%q words=%v", m.noticeErr, m.input.Value(), f.Highlights())
	}
	m.input.Reset()
	submitCmd(m, "/unhighlight nope")
	if !m.noticeErr || !strings.Contains(m.notice, "isn't a highlight word") {
		t.Fatalf("unknown word: %q", m.notice)
	}
	m.input.Reset()
	submitCmd(m, "/unhighlight")
	if !m.noticeErr || !strings.Contains(m.notice, "usage") {
		t.Fatalf("no args: %q", m.notice)
	}
	m.input.Reset()
	submitCmd(m, "/unhighlight oncall nope")
	if !slices.Equal(f.Highlights(), []string{"deploy", "oncall"}) {
		t.Fatalf("partial removal: %v", f.Highlights())
	}
	m.input.Reset()
	submitCmd(m, "/unhighlight ONCALL")
	if got := f.Highlights(); !slices.Equal(got, []string{"deploy"}) || m.noticeErr {
		t.Fatalf("remove: %v err=%q", got, m.notice)
	}
}

func TestHighlightWordsAreSanitizedInNotices(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	f.words = []string{"x\x1b]0;pwned\a"} // as a hostile cache could hold
	m.highlights = f.Highlights()
	submitCmd(m, "/highlight")
	if strings.ContainsAny(m.notice, "\x1b\a") {
		t.Fatalf("notice has control characters: %q", m.notice)
	}
	checkView(t, m, "list")
}

// highlightModel is a model whose #general ends with messages around the
// keyword "deploy".
func highlightModel(t *testing.T, w, h int) (*Model, *fakeClient) {
	t.Helper()
	f := newFake()
	f.users = append(f.users, proto.User{ID: 5, Name: "ci-bot", Bot: true})
	last := f.msgs[10][len(f.msgs[10])-3].Time
	add := func(id, author uint64, body string) {
		x := client.Message{Message: proto.Message{ID: id, Chan: 10, Author: author, Body: body, Time: last + int64(id)}}
		f.msgs[10] = append(f.msgs[10][:len(f.msgs[10])-2], append([]client.Message{x}, f.msgs[10][len(f.msgs[10])-2:]...)...)
	}
	add(150, 2, "the Deploy is broken")
	add(151, 1, "deploy note from me")
	add(152, 5, "deploy from the bot")
	add(153, 2, "redeployment is fine")
	add(154, 2, "nothing to see")
	f.words = []string{"deploy"}
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m, f
}

// markedLines returns which of the pane's message texts carry the mention
// marker in their gutter.
func markedLines(m *Model) []string {
	lines, owner := m.paneLines(&m.main, m.dims().msgW)
	var out []string
	for i, l := range lines {
		if owner[i] < 0 {
			continue
		}
		p := ansi.Strip(l)
		if strings.HasPrefix(p, "▎") {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return out
}

func TestHighlightMarkerInLog(t *testing.T) {
	m, _ := highlightModel(t, 100, 50)
	marked := strings.Join(markedLines(m), "\n")
	if !strings.Contains(marked, "Deploy is broken") {
		t.Fatalf("keyword message not marked:\n%s", marked)
	}
	for _, not := range []string{"deploy note from me", "deploy from the bot", "redeployment", "nothing to see"} {
		if strings.Contains(marked, not) {
			t.Errorf("%q must not be marked:\n%s", not, marked)
		}
	}
	// The existing @alice mention is still marked.
	if !strings.Contains(marked, "can you look?") {
		t.Errorf("@mention lost its marker:\n%s", marked)
	}
}

func TestHighlightCommandsUpdateMarkerLive(t *testing.T) {
	m, _ := highlightModel(t, 100, 50)
	submitCmd(m, "/unhighlight deploy")
	if got := strings.Join(markedLines(m), "\n"); strings.Contains(got, "Deploy is broken") {
		t.Fatalf("marker stayed after /unhighlight:\n%s", got)
	}
	submitCmd(m, "/highlight broken")
	if got := strings.Join(markedLines(m), "\n"); !strings.Contains(got, "Deploy is broken") {
		t.Fatalf("marker missing after /highlight:\n%s", got)
	}
}

func TestHighlightNotifications(t *testing.T) {
	m, _ := highlightModel(t, 80, 24)
	nt := m.opt.Notifier.(*fakeNotifier)
	msg := func(id, author uint64, ch uint64, body string) proto.Message {
		return proto.Message{ID: id, Chan: ch, Author: author, Body: body}
	}
	// Another channel than the active #general (10): #backend (11).
	run(m, m.applyUpdate(client.Update{New: []proto.Message{msg(500, 2, 11, "Deploy finished? ")}}))
	got := nt.got()
	if len(got) != 1 || got[0][0] != "#backend · bob" || got[0][1] != "Deploy finished?" {
		t.Fatalf("keyword from a person: %v", got)
	}
	for name, x := range map[string]proto.Message{
		"no keyword":     msg(501, 2, 11, "lunch?"),
		"partial word":   msg(502, 2, 11, "redeployment tonight"),
		"my own":         msg(503, 1, 11, "deploy done"),
		"a bot":          msg(504, 5, 11, "deploy done"),
		"deleted":        {ID: 505, Chan: 11, Author: 2, Body: "deploy", Deleted: true},
		"active+focused": msg(506, 2, 10, "deploy"),
	} {
		if m.shouldNotify(x) {
			t.Errorf("%s: would notify", name)
		}
	}
	for name, x := range map[string]proto.Message{
		"keyword":          msg(507, 2, 11, "any deploy-prod news"),
		"keyword, blurred": msg(508, 2, 10, "deploy"),
	} {
		if name == "keyword, blurred" {
			m.Update(tea.BlurMsg{})
		}
		if !m.shouldNotify(x) {
			t.Errorf("%s: would not notify", name)
		}
	}
	// Removing the keyword turns it off.
	m.Update(tea.FocusMsg{})
	submitCmd(m, "/unhighlight deploy")
	if m.shouldNotify(msg(509, 2, 11, "deploy")) {
		t.Error("notifies after /unhighlight")
	}
}

func TestHighlightViewsFitAllSizes(t *testing.T) {
	for _, sz := range [][2]int{{40, 12}, {80, 24}, {200, 60}} {
		m, _ := highlightModel(t, sz[0], sz[1])
		checkView(t, m, "start")
		submitCmd(m, "/highlight")
		checkView(t, m, "list")
	}
}
