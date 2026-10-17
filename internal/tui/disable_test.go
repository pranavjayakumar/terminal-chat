package tui

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

func (f *fakeClient) DisableUser(_ context.Context, user uint64) error {
	f.record("disable " + strconv.FormatUint(user, 10))
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disableErr
}

func pastDisableTypeAhead(m *Model) { m.dis.opened = m.dis.opened.Add(-time.Second) }

func disableCalls(f *fakeClient) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "disable ") {
			n++
		}
	}
	return n
}

func adminModelSize(t *testing.T, w, h int) (*Model, *fakeClient) {
	t.Helper()
	m, f := newTestModelFake(t, w, h)
	f.me.Admin = true
	f.users[0].Admin = true
	m.reloadAll()
	return m, f
}

// B8: /disable @user asks first (a full-body question no notice can
// replace), ignores typed-ahead keys, and only y disables.
func TestDisableConfirm(t *testing.T) {
	for _, sz := range [][2]int{{20, 6}, {40, 12}, {59, 16}, {80, 24}, {200, 60}} {
		m, f := adminModelSize(t, sz[0], sz[1])
		runInput(t, m, "/disable @averyveryverylongusername")
		if m.mode != modeDisable || m.input.Value() != "" {
			t.Fatalf("%v: mode %v input %q notice %q", sz, m.mode, m.input.Value(), m.notice)
		}
		v := checkView(t, m, "disable question")
		if !strings.Contains(v, "disable @") {
			t.Errorf("%v: question not shown:\n%s", sz, v)
		}
		if sz[1] >= 16 && !strings.Contains(v, "can't be undone") {
			t.Errorf("%v: consequences not shown:\n%s", sz, v)
		}
		press(m, "y") // typed ahead: ignored
		if m.mode != modeDisable || disableCalls(f) != 0 {
			t.Fatalf("%v: type-ahead y acted", sz)
		}
		m.setErr("x", errors.New("some background error")) // doesn't hide the question
		if v := checkView(t, m, "notice over question"); !strings.Contains(v, "disable @") {
			t.Errorf("%v: question gone after a notice:\n%s", sz, v)
		}
		pastDisableTypeAhead(m)
		press(m, "y")
		if m.mode != modeNormal || !f.called("disable 4") {
			t.Fatalf("%v: y didn't disable: mode %v calls %v", sz, m.mode, f.calls)
		}
		if !strings.Contains(m.notice, "✓ disabled @averyveryverylongusername") || m.noticeErr {
			t.Errorf("%v: notice %q", sz, m.notice)
		}
		checkView(t, m, "disabled")
	}

	// Any other key cancels; Esc cancels even during the type-ahead window.
	for _, k := range []string{"n", "enter", "d", "esc"} {
		m, f := adminModelSize(t, 80, 24)
		runInput(t, m, "/disable @bob")
		if k != "esc" {
			pastDisableTypeAhead(m)
		}
		press(m, k)
		if m.mode != modeNormal || disableCalls(f) != 0 || m.notice != "not disabled" {
			t.Errorf("%q: mode %v calls %v notice %q", k, m.mode, f.calls, m.notice)
		}
	}
}

func TestDisableErrors(t *testing.T) {
	m, f := adminModelSize(t, 80, 24)
	f.users[2].Disabled = true
	m.reloadAll()
	for _, tc := range []struct{ line, want string }{
		{"/disable", "usage"},
		{"/disable @bob @carol", "usage"},
		{"/disable @nobody\x1b[31m", "unknown user"},
		{"/disable @alice", "yourself"},
		{"/disable @carol", "already disabled"},
	} {
		runInput(t, m, tc.line)
		if m.mode != modeNormal || !m.noticeErr || !strings.Contains(m.notice, tc.want) || m.input.Value() == "" {
			t.Errorf("%q: mode %v notice %q (err %v) input %q", tc.line, m.mode, m.notice, m.noticeErr, m.input.Value())
		}
		if strings.Contains(m.notice, "\x1b") {
			t.Errorf("%q: unsanitized notice %q", tc.line, m.notice)
		}
	}

	// Members can't.
	m2, f2 := newTestModelFake(t, 80, 24)
	runInput(t, m2, "/disable @bob")
	if m2.mode != modeNormal || !strings.Contains(m2.notice, "only admins") {
		t.Errorf("member: mode %v notice %q", m2.mode, m2.notice)
	}

	// A server refusal surfaces as an error notice.
	f2.disableErr = proto.Errf(proto.ErrForbidden, "admins only")
	f2.me.Admin = true
	f2.users[0].Admin = true
	m2.reloadAll()
	runInput(t, m2, "/disable @bob")
	pastDisableTypeAhead(m2)
	press(m2, "y")
	if !m2.noticeErr || !strings.Contains(m2.notice, "forbidden") {
		t.Errorf("refused: notice %q", m2.notice)
	}
}
