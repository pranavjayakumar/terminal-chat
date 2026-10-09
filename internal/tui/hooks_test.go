package tui

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

type hookFake struct {
	*fakeClient
	created []string
	deleted []uint64
}

const (
	fakeURL  = "https://h:7443/hooks/abc"
	fakeCurl = "# export TC_HOOK_URL='" + fakeURL + "'\ncurl -fsS -k --pinnedpubkey 'sha256//x=' -d '{}' \"$TC_HOOK_URL\"\n"
)

func (f *hookFake) CreateHook(_ context.Context, ch uint64, name string) (client.NewHook, error) {
	f.created = append(f.created, name)
	return client.NewHook{ID: 1, Name: name + "-bot", URL: fakeURL, Pin: "sha256//x=", Curl: fakeCurl}, nil
}

func (f *hookFake) ListHooks(context.Context) ([]proto.Hook, error) {
	return []proto.Hook{{ID: 5, Chan: 10, Name: "ci"}}, nil
}

func (f *hookFake) DeleteHook(_ context.Context, id uint64) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func newHookModel(t *testing.T, w, h int) (*Model, *hookFake) {
	t.Helper()
	f := &hookFake{fakeClient: newFake()}
	f.users = append(f.users, proto.User{ID: 9, Name: "ci", Bot: true})
	f.msgs[10] = append(f.msgs[10], client.Message{Message: proto.Message{ID: 900, Chan: 10, Author: 9, Body: "build passed", Time: f.msgs[10][len(f.msgs[10])-3].Time + 1}})
	m := New(context.Background(), f, Options{Out: &syncBuf{}, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m, f
}

func TestWebhookCommands(t *testing.T) {
	m, f := newHookModel(t, 100, 30)
	m.openChannel(10)
	do := func(in string) {
		t.Helper()
		m.input.SetValue(in)
		run(m, m.submit())
		if m.input.Value() != "" {
			t.Fatalf("%q: input kept, notice %q", in, m.notice)
		}
		checkView(t, m, in)
	}
	do("/webhook ci")
	if len(f.created) != 1 || f.created[0] != "ci" {
		t.Fatalf("created %v", f.created)
	}
	// The notice never shows the secret URL (L3).
	if m.notice != "webhook ci-bot created · curl command copied" || strings.Contains(m.View(), "/hooks/") {
		t.Fatalf("notice %q", m.notice)
	}
	if !strings.Contains(m.opt.Out.(*syncBuf).String(), base64.StdEncoding.EncodeToString([]byte(fakeCurl))) {
		t.Fatal("curl not copied via OSC 52")
	}
	do("/webhooks")
	if !strings.Contains(m.notice, "5 ci") || !strings.Contains(m.notice, "general") {
		t.Fatalf("list notice %q", m.notice)
	}
	do("/webhook-delete 5")
	if len(f.deleted) != 1 || f.deleted[0] != 5 {
		t.Fatalf("deleted %v", f.deleted)
	}

	for _, in := range []string{"/webhook", "/webhook Bad Name", "/webhook-delete x", "/webhook " + strings.Repeat("a", 29)} {
		m.input.SetValue(in)
		m.submit()
		if m.input.Value() != in || !m.noticeErr {
			t.Errorf("%q: want kept input and error", in)
		}
	}
}

func TestWebhookUnsupportedBackend(t *testing.T) {
	m := newTestModel(t, 80, 24)
	m.input.SetValue("/webhooks")
	m.submit()
	if !m.noticeErr || !strings.Contains(m.notice, "supported") {
		t.Fatalf("notice %q", m.notice)
	}
}

func TestBotTag(t *testing.T) {
	for _, w := range []int{30, 60, 100} {
		m, _ := newHookModel(t, w, 40)
		m.openChannel(10)
		v := ansi.Strip(checkView(t, m, "bot tag"))
		if !strings.Contains(v, "ci bot") {
			t.Fatalf("width %d: no bot tag in\n%s", w, v)
		}
		if strings.Contains(v, "bob bot") {
			t.Fatalf("width %d: human tagged", w)
		}
	}
}
