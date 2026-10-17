package tui

import (
	"strings"
	"testing"
)

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in   string
		name string
		args []string
		rest string
		err  bool
	}{
		{"/join #general", "join", []string{"#general"}, "#general", false},
		{"  /TOPIC  hello   world ", "topic", []string{"hello", "world"}, "hello   world", false},
		{"/create #x --private", "create", []string{"#x", "--private"}, "#x --private", false},
		{"/leave", "leave", nil, "", false},
		{"/dm @a @b", "dm", []string{"@a", "@b"}, "@a @b", false},
		{"/away\tat lunch", "away", []string{"at", "lunch"}, "at lunch", false},
		{"/", "", nil, "", true},
		{"/ join", "", nil, "", true},
		{"hello", "", nil, "", true},
	}
	for _, c := range cases {
		got, err := parseCommand(c.in)
		if (err != nil) != c.err {
			t.Errorf("%q: err = %v", c.in, err)
			continue
		}
		if c.err {
			continue
		}
		if got.name != c.name || strings.Join(got.args, ",") != strings.Join(c.args, ",") || got.rest != c.rest {
			t.Errorf("%q: got %+v", c.in, got)
		}
	}
}

func TestRunCommandErrorsKeepInput(t *testing.T) {
	m := newTestModel(t, 80, 24)
	for _, in := range []string{"/bogus", "/join", "/join #nope", "/create", "/create #Bad!", "/dm @nobody", "/search", "/add @ghost"} {
		m.input.SetValue(in)
		m.submit()
		if m.input.Value() != in {
			t.Errorf("%q: input not kept (now %q)", in, m.input.Value())
		}
		if !m.noticeErr || m.notice == "" {
			t.Errorf("%q: no error notice", in)
		}
	}
}

func TestRunCommands(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	do := func(in string) {
		m.input.SetValue(in)
		run(m, m.submit())
		if m.input.Value() != "" {
			t.Fatalf("%q: input not cleared (notice %q)", in, m.notice)
		}
	}
	do("/topic new topic")
	do("/create #ops --private")
	do("/away lunch")
	if !f.called("topic new topic") || !f.called("create ops") || !f.called("away") {
		t.Fatalf("calls: %v", f.calls)
	}
	do("/join #backend")
	if m.active != 11 {
		t.Fatalf("join didn't switch: %d", m.active)
	}
	do("/search deploy")
	if m.mode != modeSearch || len(m.search.res) != 2 {
		t.Fatalf("search mode %v res %d", m.mode, len(m.search.res))
	}
	m.searchKey(keyMsg("down"))
	run(m, m.searchKey(keyMsg("enter")))
	if !m.thread.open() || m.thread.root != 103 {
		t.Fatalf("jump to reply didn't open thread: %+v", m.thread.root)
	}
	// "//" escapes a leading slash.
	m.thread = pane{sel: -1}
	m.input.SetValue("//shrug")
	m.submit()
	if n := len(f.sent); n == 0 || f.sent[n-1].Body != "/shrug" {
		t.Fatalf("sent %+v", f.sent)
	}
}

// B7: /create --private makes an end-to-end encrypted channel; only an
// explicit --plaintext makes an unencrypted private one.
func TestCreatePrivateIsEncrypted(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	for in, want := range map[string]string{
		"/create #ops --private":               "create e2e ops",
		"/create --private #ops2":              "create e2e ops2",
		"/create #pub":                         "create public pub",
		"/create #hooks --private --plaintext": "create private hooks",
	} {
		m.input.SetValue(in)
		run(m, m.submit())
		if m.input.Value() != "" || !f.called(want) {
			t.Errorf("%q: calls %v, notice %q", in, f.calls, m.notice)
		}
	}
	if f.called("create private ops") || f.called("create private ops2") {
		t.Fatalf("--private went the plaintext way: %v", f.calls)
	}
	m.input.SetValue("/create #x --plaintext")
	m.submit()
	if !m.noticeErr || m.input.Value() == "" {
		t.Fatalf("--plaintext without --private: notice %q", m.notice)
	}
	checkView(t, m, "create errors")
}

func TestResolveEmoji(t *testing.T) {
	ok := map[string]string{"+1": "👍", ":tada:": "🎉", "tada": "🎉", "🚀": "🚀", " 👀 ": "👀"}
	for in, want := range ok {
		if got, good := resolveEmoji(in); !good || got != want {
			t.Errorf("%q -> %q %v", in, got, good)
		}
	}
	for _, in := range []string{"", "hello", ":nope:", "a b", "\x1b[1m", "👍 👍"} {
		if got, good := resolveEmoji(in); good {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}
