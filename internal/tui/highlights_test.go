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

// markedLines returns which of the pane's message texts carry a mention or
// keyword marker in their gutter.
func markedLines(m *Model) []string { return gutterLines(m, "▎", "*") }

// gutterLines returns the text of rows whose bar marks them: "▎" is a
// mention (accent bar), "*" a highlight word (warn bar).
func gutterLines(m *Model, marks ...string) []string {
	lines, owner := m.paneLines(&m.main, m.dims().msgW)
	var out []string
	for i, l := range lines {
		if owner[i] < 0 || l.b == nil {
			continue
		}
		bar := l.b.at(0, l.y)
		for _, mark := range marks {
			want := m.st.th.accent
			if mark == "*" {
				want = m.st.th.warn
			}
			if bar.s == "│" && bar.fg == want {
				out = append(out, strings.TrimSpace(strings.TrimPrefix(l.text(), "│")))
			}
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
	run(m, m.applyUpdate(client.Update{New: news(msg(500, 2, 11, "Deploy finished? "))}))
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

// Another tc on the same cache changes the keywords: the log's markers,
// notifications and /unhighlight follow without a restart.
func TestHighlightsFollowAnotherPane(t *testing.T) {
	m, f := highlightModel(t, 100, 50)
	if !strings.Contains(strings.Join(markedLines(m), "\n"), "Deploy is broken") {
		t.Fatal("setup: keyword not marked")
	}
	f.words = []string{"nothing"} // /unhighlight deploy + /highlight nothing, elsewhere
	m.Update(updateMsg(client.Update{Channels: true}))
	if !slices.Equal(m.highlights, []string{"nothing"}) {
		t.Fatalf("highlights = %v", m.highlights)
	}
	marked := strings.Join(markedLines(m), "\n")
	if strings.Contains(marked, "Deploy is broken") || !strings.Contains(marked, "nothing to see") {
		t.Fatalf("markers didn't follow:\n%s", marked)
	}
	if !m.shouldNotify(proto.Message{ID: 600, Chan: 11, Author: 2, Body: "nothing here"}) {
		t.Error("new keyword doesn't notify")
	}
	if m.shouldNotify(proto.Message{ID: 601, Chan: 11, Author: 2, Body: "deploy"}) {
		t.Error("removed keyword still notifies")
	}
}

func TestUnhighlightChecksTheFreshList(t *testing.T) {
	m, f := highlightModel(t, 100, 50)
	f.words = []string{"deploy", "extra"} // added by another pane
	submitCmd(m, "/unhighlight extra")
	if m.noticeErr || slices.Contains(f.words, "extra") {
		t.Fatalf("a word from another pane can't be removed: err=%v %q words=%v", m.noticeErr, m.notice, f.words)
	}
	f.words = nil // removed by another pane
	submitCmd(m, "/unhighlight deploy")
	if !m.noticeErr || !strings.Contains(m.notice, "isn't a highlight word") {
		t.Fatalf("removing a word that is gone: err=%v %q", m.noticeErr, m.notice)
	}
}

// An @mention keeps the bar; a keyword shows "*" (both still count as
// mentions for the sidebar and notifications).
func TestHighlightGutterIsAsteriskNotMentionBar(t *testing.T) {
	m, _ := highlightModel(t, 100, 50)
	bars := strings.Join(gutterLines(m, "▎"), "\n")
	stars := strings.Join(gutterLines(m, "*"), "\n")
	if !strings.Contains(stars, "Deploy is broken") || strings.Contains(bars, "Deploy is broken") {
		t.Fatalf("keyword message: bars\n%s\nstars\n%s", bars, stars)
	}
	if !strings.Contains(bars, "can you look?") || strings.Contains(stars, "can you look?") {
		t.Fatalf("@mention: bars\n%s\nstars\n%s", bars, stars)
	}
	// Mention and keyword in one message: the stronger signal wins.
	x := client.Message{Message: proto.Message{ID: 160, Chan: 10, Author: 2, Body: "@alice the deploy"}}
	if got := ansi.Strip(m.concernMark(x.Body)); got != "▎" {
		t.Fatalf("mention + keyword = %q", got)
	}
}

func TestHighlightIgnoresFencedCode(t *testing.T) {
	m, _ := highlightModel(t, 100, 50)
	fenced := "log:\n```\ndeploy step failed\n```"
	if got := ansi.Strip(m.concernMark(fenced)); got != " " {
		t.Fatalf("gutter for a keyword in a code block = %q", got)
	}
	if m.shouldNotify(proto.Message{ID: 700, Chan: 11, Author: 2, Body: fenced}) {
		t.Error("a keyword in a code block notifies")
	}
	if !m.shouldNotify(proto.Message{ID: 701, Chan: 11, Author: 2, Body: "deploy?\n" + fenced}) {
		t.Error("a keyword in prose next to a code block doesn't notify")
	}
}
