package tui

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
)

// fakeTrust is the fake's trust UX state.
type fakeTrust struct {
	unverified []uint64
	sn         map[uint64]client.SafetyNumber
	verified   []string // "user:digits"
	gh         client.GitHubCheck
	ghErr      error
	ghCalls    []string
	keyChg     map[uint64][]uint64
	accepted   []string
	pending    map[uint64][]string
	alarms     map[uint64]client.AlarmInfo
	cleared    []uint64
}

func (f *fakeClient) UnverifiedUsers() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tr.unverified)
}

func (f *fakeClient) SafetyNumber(user uint64) (client.SafetyNumber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sn, ok := f.tr.sn[user]
	if !ok {
		return sn, client.ErrNoKeys
	}
	return sn, nil
}

func (f *fakeClient) MarkVerified(user uint64, digits string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tr.sn[user].Digits != digits {
		return client.ErrSafetyChanged
	}
	f.tr.verified = append(f.tr.verified, strconv.FormatUint(user, 10)+":"+digits)
	f.tr.unverified = slices.DeleteFunc(f.tr.unverified, func(u uint64) bool { return u == user })
	return nil
}

func (f *fakeClient) VerifyGitHub(ctx context.Context, user uint64, login string) (client.GitHubCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tr.ghCalls = append(f.tr.ghCalls, login)
	res := f.tr.gh
	res.User, res.Login = user, login
	return res, f.tr.ghErr
}

func (f *fakeClient) ConfirmGitHub(res client.GitHubCheck) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !res.Match {
		return client.ErrNotMatched
	}
	f.tr.verified = append(f.tr.verified, strconv.FormatUint(res.User, 10)+":github:"+res.Login)
	f.tr.unverified = slices.DeleteFunc(f.tr.unverified, func(u uint64) bool { return u == res.User })
	return nil
}

func (f *fakeClient) KeyChangedMembers(ch uint64) []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tr.keyChg[ch])
}

func (f *fakeClient) PendingKeys(user uint64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tr.pending[user])
}

// AcceptKeyChanges mirrors the client: exactly the pending set, or with
// only a subset of it.
func (f *fakeClient) AcceptKeyChanges(user uint64, fps []string, only bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	pend := f.tr.pending[user]
	if len(pend) == 0 {
		return errors.New("no pending key change for that user")
	}
	for _, fp := range fps {
		if !slices.Contains(pend, fp) {
			return client.ErrPendingChanged
		}
	}
	if !only && len(fps) != len(pend) {
		return client.ErrPendingChanged
	}
	f.tr.accepted = append(f.tr.accepted, fps...)
	pend = slices.DeleteFunc(slices.Clone(pend), func(fp string) bool { return slices.Contains(fps, fp) })
	f.tr.pending[user] = pend
	if len(pend) == 0 {
		for ch, us := range f.tr.keyChg {
			f.tr.keyChg[ch] = slices.DeleteFunc(us, func(u uint64) bool { return u == user })
		}
	}
	return nil
}

// pastTypeAhead lets the open trust overlay take keys.
func pastTypeAhead(m *Model) { m.verify.opened = m.verify.opened.Add(-time.Second) }

func (f *fakeClient) Alarm(ch uint64) (client.AlarmInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.tr.alarms[ch]
	return a, ok
}

func (f *fakeClient) ClearAlarm(ch uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tr.alarms[ch]; !ok {
		return client.ErrNoAlarm
	}
	delete(f.tr.alarms, ch)
	f.e2eSt[ch] = client.E2EStatus{State: client.E2EOK, Epoch: 1}
	f.tr.cleared = append(f.tr.cleared, ch)
	return nil
}

const digits60 = "123450987654321098765432109876543210987654321098765432109876"

// trustFake: the DM with bob (20) is encrypted and bob is trusted only on
// first use; the safety number with him is known.
func trustFake(f *fakeClient) {
	e2eFake(f, client.E2EStatus{State: client.E2EOK, Epoch: 1})
	f.tr.unverified = []uint64{2}
	f.tr.sn = map[uint64]client.SafetyNumber{2: {
		Digits: digits60,
		Me:     client.SafetyParty{User: 1, Name: "alice", Fingerprints: []string{"SHA256:" + strings.Repeat("A", 43)}},
		Them:   client.SafetyParty{User: 2, Name: "bob", Fingerprints: []string{"SHA256:" + strings.Repeat("B", 43), "SHA256:" + strings.Repeat("C", 43)}},
	}}
}

var trustSizes = [][2]int{{40, 12}, {80, 24}, {200, 60}}

func plain(m *Model) string { return ansi.Strip(m.View()) }

// /verify shows the safety number and both users' keys; v marks bob
// verified (the "?" goes away), Esc doesn't.
func TestVerifyOverlay(t *testing.T) {
	for _, sz := range trustSizes {
		m, f := newTestModelFake(t, sz[0], sz[1])
		trustFake(f)
		run(m, m.applyUpdate(client.Update{Channels: true}))
		run(m, m.openChannel(20))
		if h := ansi.Strip(m.headerLine(m.w)); !strings.Contains(h, "@bob? 🔒") {
			t.Errorf("%v header %q", sz, h)
		}
		runInput(t, m, "/verify @bob")
		if m.mode != modeVerify {
			t.Fatalf("%v: mode %v, notice %q", sz, m.mode, m.notice)
		}
		v := ansi.Strip(checkView(t, m, "verify"))
		for _, want := range []string{"verify @bob", "12345 09876", "54321 09876"} {
			if !strings.Contains(v, want) {
				t.Errorf("%v: missing %q in\n%s", sz, want, v)
			}
		}
		if sz[1] >= 24 {
			for _, want := range []string{"Press v", "you (@alice)", "SHA256:BBBB", "SHA256:CCCC"} {
				if !strings.Contains(v, want) {
					t.Errorf("%v: missing %q in\n%s", sz, want, v)
				}
			}
		}
		pastTypeAhead(m)
		for range 20 { // scrolling stays in bounds
			press(m, "down")
			checkView(t, m, "verify scrolled")
		}
		press(m, "esc")
		if m.mode != modeNormal || len(f.tr.verified) != 0 {
			t.Fatalf("Esc verified: %v", f.tr.verified)
		}
		runInput(t, m, "/verify @bob")
		press(m, "v") // typed ahead, before the overlay could be read
		if m.mode != modeVerify || len(f.tr.verified) != 0 {
			t.Fatalf("type-ahead v verified: %v", f.tr.verified)
		}
		pastTypeAhead(m)
		press(m, "v")
		if m.mode != modeNormal || !slices.Equal(f.tr.verified, []string{"2:" + digits60}) {
			t.Fatalf("v: mode %v verified %v", m.mode, f.tr.verified)
		}
		if h := ansi.Strip(m.headerLine(m.w)); strings.Contains(h, "?") {
			t.Errorf("%v: still unverified: %q", sz, h)
		}
	}

	m, f := newTestModelFake(t, 80, 24)
	trustFake(f)
	run(m, m.applyUpdate(client.Update{Channels: true}))
	for line, want := range map[string]string{
		"/verify":                    "usage",
		"/verify @nobody":            "unknown user",
		"/verify @alice":             "that's you",
		"/verify @carol":             "no key of that user",
		"/verify @bob --github":      "usage",
		"/verify @bob @carol":        "usage",
		"/verify @bob --github a b":  "usage",
		"/accept-key @bob":           "no pending key change",
		"/alarm":                     "no alarm",
		"/alarm-clear #general":      "no alarm",
		"/verify @bob --github x -y": "usage",
	} {
		runInput(t, m, line)
		if !m.noticeErr || !strings.Contains(m.notice, want) || m.input.Value() != line || m.mode != modeNormal {
			t.Errorf("%q: notice %q (err %v) input %q mode %v", line, m.notice, m.noticeErr, m.input.Value(), m.mode)
		}
		m.clearInput()
	}
}

// /verify --github shows a match (now verified) or a clear mismatch.
func TestVerifyGitHubOverlay(t *testing.T) {
	for _, sz := range trustSizes {
		m, f := newTestModelFake(t, sz[0], sz[1])
		trustFake(f)
		f.tr.gh = client.GitHubCheck{Name: "bob", Pinned: []string{"SHA256:pinned1"}, GitHub: []string{"SHA256:other"}, Missing: []string{"SHA256:pinned1"}}
		run(m, m.applyUpdate(client.Update{Channels: true}))
		run(m, m.openChannel(20))
		runInput(t, m, "/verify @bob --github bob-gh")
		if m.mode != modeVerify || !slices.Equal(f.tr.ghCalls, []string{"bob-gh"}) {
			t.Fatalf("mode %v calls %v notice %q", m.mode, f.tr.ghCalls, m.notice)
		}
		v := ansi.Strip(checkView(t, m, "github mismatch"))
		if !strings.Contains(v, "MISMATCH") {
			t.Errorf("%v: no mismatch in\n%s", sz, v)
		}
		pastTypeAhead(m)
		press(m, "v") // nothing to confirm here
		if m.mode != modeVerify || len(f.tr.verified) != 0 {
			t.Fatal("v in a GitHub result did something")
		}
		press(m, "esc")
		f.tr.gh = client.GitHubCheck{Name: "bob", Pinned: []string{"SHA256:pinned1"}, GitHub: []string{"SHA256:pinned1"}, Match: true}
		runInput(t, m, "/verify @bob --github bob-gh")
		v = ansi.Strip(checkView(t, m, "github match"))
		if !strings.Contains(v, "✓ every key") || strings.Contains(v, "MISMATCH") {
			t.Errorf("%v: match view\n%s", sz, v)
		}
		if sz[0] >= 200 && !strings.Contains(v, "verify @bob because you confirmed github.com/bob-gh is theirs") {
			t.Errorf("no confirmation text:\n%s", v)
		}
		// A match alone verifies nothing; Esc leaves bob unverified.
		pastTypeAhead(m)
		press(m, "esc")
		if len(f.tr.verified) != 0 || !strings.Contains(ansi.Strip(m.headerLine(m.w)), "?") {
			t.Fatalf("a match verified without v: %v", f.tr.verified)
		}
		runInput(t, m, "/verify @bob --github bob-gh")
		press(m, "v") // type-ahead
		pastTypeAhead(m)
		if len(f.tr.verified) != 0 {
			t.Fatal("type-ahead v confirmed")
		}
		press(m, "v")
		if !slices.Equal(f.tr.verified, []string{"2:github:bob-gh"}) || m.mode != modeNormal {
			t.Fatalf("v: %v mode %v", f.tr.verified, m.mode)
		}
		if h := ansi.Strip(m.headerLine(m.w)); strings.Contains(h, "?") {
			t.Errorf("still unverified after confirming: %q", h)
		}
	}
	m, f := newTestModelFake(t, 80, 24)
	trustFake(f)
	f.tr.ghErr = errors.New("fetching GitHub keys: HTTP 404")
	runInput(t, m, "/verify @bob --github ghost")
	if m.mode != modeNormal || !m.noticeErr || !strings.Contains(m.notice, "404") {
		t.Fatalf("error: mode %v notice %q", m.mode, m.notice)
	}
}

// A key change: one red banner line in the shared channel, Enter doesn't
// send (input kept), /accept-key unblocks; frames fit with the banner and
// overlays on top.
func TestKeyChangeBanner(t *testing.T) {
	for _, sz := range trustSizes {
		m, f := newTestModelFake(t, sz[0], sz[1])
		trustFake(f)
		f.tr.keyChg = map[uint64][]uint64{20: {2}}
		f.tr.pending = map[uint64][]string{2: {"SHA256:new1"}}
		run(m, m.applyUpdate(client.Update{Channels: true}))
		run(m, m.openChannel(20))
		v := ansi.Strip(checkView(t, m, "banner"))
		banner := ""
		for _, l := range strings.Split(v, "\n") {
			if strings.Contains(l, "⚠") {
				banner = l
				break
			}
		}
		if !strings.Contains(banner, "/accept-key @bob") {
			t.Errorf("%v: the command didn't survive truncation:\n%s", sz, v)
		}
		if n := strings.Count(v, "⚠ /accept-key"); n != 1 {
			t.Errorf("%v: %d banner lines in\n%s", sz, n, v)
		}
		if sz[0] >= 200 && !strings.Contains(v, "⚠ /accept-key @bob, then /verify @bob — @bob's key changed") {
			t.Errorf("banner text:\n%s", v)
		}
		lines := strings.Split(v, "\n")
		if g := m.geo(); g.banner < 0 || !strings.Contains(lines[g.banner], "⚠ /accept-key") || g.banner > g.top+2 {
			t.Errorf("%v: banner not under the header:\n%s", sz, v)
		}
		typeVia(m, "psst")
		press(m, "enter")
		if len(f.sent) != 0 || m.input.Value() != "psst" || !strings.Contains(m.notice, "key changed") {
			t.Fatalf("sent %v input %q notice %q", f.sent, m.input.Value(), m.notice)
		}
		checkView(t, m, "blocked")
		for _, line := range []string{"/verify @bob", "/members", "/help", "/accept-key @bob"} {
			m.clearInput()
			runInput(t, m, line)
			checkView(t, m, "banner + "+line)
			press(m, "esc")
		}
		// Elsewhere there's no banner.
		run(m, m.openChannel(10))
		if v := plain(m); strings.Contains(v, "key changed") {
			t.Errorf("banner in #general:\n%s", v)
		}
		run(m, m.openChannel(20))
		m.clearInput()
		runInput(t, m, "/accept-key @bob")
		if m.mode != modeVerify || len(f.tr.accepted) != 0 {
			t.Fatalf("accept-key didn't ask: mode %v accepted %v", m.mode, f.tr.accepted)
		}
		if v := plain(m); !strings.Contains(v, "SHA256:new1") {
			t.Errorf("pending key not shown:\n%s", v)
		}
		press(m, "y") // type-ahead
		if len(f.tr.accepted) != 0 {
			t.Fatal("type-ahead y accepted")
		}
		pastTypeAhead(m)
		press(m, "y")
		if !slices.Equal(f.tr.accepted, []string{"SHA256:new1"}) || m.noticeErr || m.mode != modeNormal {
			t.Fatalf("accept: %v notice %q", f.tr.accepted, m.notice)
		}
		if v := plain(m); strings.Contains(v, "⚠ /accept-key") {
			t.Errorf("banner after accept:\n%s", v)
		}
		typeVia(m, "psst")
		press(m, "enter")
		if len(f.sent) != 1 {
			t.Fatalf("not sent after accept: %v (%q)", f.sent, m.notice)
		}
	}
	m, f := newTestModelFake(t, 80, 24)
	trustFake(f)
	f.tr.keyChg = map[uint64][]uint64{20: {2, 3}}
	run(m, m.applyUpdate(client.Update{Channels: true}))
	run(m, m.openChannel(20))
	if v := plain(m); !strings.Contains(v, "keys changed: @bob, @carol") {
		t.Errorf("multi banner:\n%s", v)
	}
}

// /accept-key acknowledges exactly what it showed: if the pending set
// changed meanwhile it refuses and shows the new set; a named fingerprint
// is accepted alone.
func TestAcceptKeyExactlyShown(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	trustFake(f)
	f.tr.keyChg = map[uint64][]uint64{20: {2}}
	f.tr.pending = map[uint64][]string{2: {"SHA256:k1"}}
	run(m, m.applyUpdate(client.Update{Channels: true}))
	run(m, m.openChannel(20))
	runInput(t, m, "/accept-key @bob")
	f.tr.pending[2] = []string{"SHA256:k1", "SHA256:k2"} // arrives while the overlay is up
	pastTypeAhead(m)
	press(m, "y")
	if len(f.tr.accepted) != 0 || m.mode != modeVerify || !m.noticeErr {
		t.Fatalf("accepted a changed set: %v mode %v notice %q", f.tr.accepted, m.mode, m.notice)
	}
	if v := plain(m); !strings.Contains(v, "SHA256:k2") {
		t.Fatalf("new set not shown:\n%s", v)
	}
	press(m, "esc")
	for line, want := range map[string]string{
		"/accept-key @bob SHA256:nope": "no pending key",
		"/accept-key @bob a b":         "usage",
		"/accept-key":                  "usage",
		"/accept-key @carol":           "no pending key change",
	} {
		m.clearInput()
		runInput(t, m, line)
		if !m.noticeErr || !strings.Contains(m.notice, want) || m.mode != modeNormal {
			t.Errorf("%q: notice %q mode %v", line, m.notice, m.mode)
		}
	}
	m.clearInput()
	runInput(t, m, "/accept-key @bob SHA256:k2")
	if v := plain(m); strings.Contains(v, "SHA256:k1") || !strings.Contains(v, "SHA256:k2") {
		t.Fatalf("single-key overlay:\n%s", v)
	}
	pastTypeAhead(m)
	press(m, "y")
	if !slices.Equal(f.tr.accepted, []string{"SHA256:k2"}) || !slices.Equal(f.tr.pending[2], []string{"SHA256:k1"}) {
		t.Fatalf("accepted %v pending %v", f.tr.accepted, f.tr.pending[2])
	}
	if v := plain(m); !strings.Contains(v, "key changed") {
		t.Error("banner gone while a key is still pending")
	}
}

// /alarm explains; /alarm-clear needs the channel's name typed and clears
// only then.
func TestAlarmClearConfirm(t *testing.T) {
	for _, sz := range trustSizes {
		m, f := newTestModelFake(t, sz[0], sz[1])
		trustFake(f)
		f.e2eSt[20] = client.E2EStatus{State: client.E2EAlarm, Detail: "fork: a second bundle for epoch 2"}
		f.tr.alarms = map[uint64]client.AlarmInfo{20: {Detail: "fork: a second bundle for epoch 2", Since: time.Now(),
			Why: "The server showed two different key bundles for the same epoch."}}
		run(m, m.applyUpdate(client.Update{Channels: true}))
		run(m, m.openChannel(20))
		if h := ansi.Strip(m.headerLine(200)); !strings.Contains(h, "⚠ alarm: fork") {
			t.Errorf("header %q", h)
		}
		runInput(t, m, "/alarm")
		if m.mode != modeList {
			t.Fatalf("mode %v notice %q", m.mode, m.notice)
		}
		v := ansi.Strip(checkView(t, m, "alarm"))
		if !strings.Contains(v, "alarm in @bob") || !strings.Contains(v, "fork") {
			t.Errorf("%v alarm view\n%s", sz, v)
		}
		if sz[1] >= 24 && !strings.Contains(v, "two different key bundles") {
			t.Errorf("%v no explanation\n%s", sz, v)
		}
		press(m, "esc")
		for _, line := range []string{"/alarm-clear", "/alarm-clear #general", "/alarm-clear bobby"} {
			runInput(t, m, line)
			if len(f.tr.cleared) != 0 || !m.noticeErr || !strings.Contains(m.notice, "/alarm-clear @bob") {
				t.Fatalf("%q: cleared %v notice %q", line, f.tr.cleared, m.notice)
			}
			checkView(t, m, line)
			m.clearInput()
		}
		runInput(t, m, "/alarm-clear @bob")
		if !slices.Equal(f.tr.cleared, []uint64{20}) || m.noticeErr {
			t.Fatalf("cleared %v notice %q", f.tr.cleared, m.notice)
		}
		if h := ansi.Strip(m.headerLine(200)); strings.Contains(h, "alarm") {
			t.Errorf("header after clear %q", h)
		}
	}
	m, f := newTestModelFake(t, 80, 24)
	trustFake(f)
	f.e2eSt[20] = client.E2EStatus{State: client.E2EAlarm, Detail: "membership not confirmed by a member: @carol"}
	f.tr.alarms = map[uint64]client.AlarmInfo{20: {Detail: "membership not confirmed by a member: @carol", Computed: true, Why: "only the server says"}}
	run(m, m.applyUpdate(client.Update{Channels: true}))
	run(m, m.openChannel(20))
	runInput(t, m, "/alarm-clear @bob")
	if len(f.tr.cleared) != 0 || !strings.Contains(m.notice, "can't be cleared") {
		t.Fatalf("computed alarm cleared: %v %q", f.tr.cleared, m.notice)
	}
	if !confirmMatches("bob", "@bob") || !confirmMatches("#sec", "#sec") || confirmMatches("", "#") || confirmMatches("Bob", "@bob") {
		t.Fatal("confirmMatches")
	}

	// The server-wide alarm: /alarm, the prompt and the result all say it
	// clears for every encrypted channel.
	m, f = newTestModelFake(t, 200, 60)
	trustFake(f)
	f.e2eSt[20] = client.E2EStatus{State: client.E2EAlarm, Detail: "the server stopped offering end-to-end encryption"}
	f.tr.alarms = map[uint64]client.AlarmInfo{20: {Detail: "the server stopped offering end-to-end encryption", Server: true, Why: "downgrade"}}
	run(m, m.applyUpdate(client.Update{Channels: true}))
	run(m, m.openChannel(20))
	runInput(t, m, "/alarm")
	if v := plain(m); !strings.Contains(v, "server-wide — clears the alarm for all encrypted channels") {
		t.Errorf("/alarm doesn't say server-wide:\n%s", v)
	}
	press(m, "esc")
	runInput(t, m, "/alarm-clear")
	if !strings.Contains(m.notice, "server-wide — clears the alarm for all encrypted channels") || len(f.tr.cleared) != 0 {
		t.Fatalf("prompt %q cleared %v", m.notice, f.tr.cleared)
	}
	m.clearInput()
	runInput(t, m, "/alarm-clear @bob")
	if !slices.Equal(f.tr.cleared, []uint64{20}) || !strings.Contains(m.notice, "for all encrypted channels") {
		t.Fatalf("cleared %v notice %q", f.tr.cleared, m.notice)
	}
}

// "?" marks members trusted only on first use (header, /members) in e2e
// channels, with a one-time dim hint; plaintext channels show none.
func TestUnverifiedMarks(t *testing.T) {
	for _, sz := range trustSizes {
		m, f := newTestModelFake(t, sz[0], sz[1])
		trustFake(f)
		f.chans[3].E2E = true // #secret with carol
		f.e2eSt[13] = client.E2EStatus{State: client.E2EOK, Epoch: 1}
		f.tr.unverified = []uint64{2, 3}
		run(m, m.applyUpdate(client.Update{Channels: true}))
		m.clearNotice()
		run(m, m.openChannel(13))
		if h := ansi.Strip(m.headerLine(m.w)); sz[0] >= 80 && !strings.Contains(h, "🔒 ?1 unverified") {
			t.Errorf("%v header %q", sz, h)
		}
		if !m.noticeDim || !strings.Contains(m.notice, "/verify @carol") {
			t.Errorf("%v hint %q dim=%v", sz, m.notice, m.noticeDim)
		}
		checkView(t, m, "hint")
		run(m, m.openChannel(10)) // the hint is about #secret: it goes with it
		if strings.Contains(m.notice, "/verify") {
			t.Errorf("%v hint followed to #general: %q", sz, m.notice)
		}
		m.clearNotice()
		run(m, m.openChannel(10))
		run(m, m.openChannel(13))
		if m.notice != "" {
			t.Errorf("hint shown twice: %q", m.notice)
		}
		runInput(t, m, "/members")
		v := ansi.Strip(checkView(t, m, "members"))
		if !strings.Contains(v, "? @carol") || !strings.Contains(v, "@alice (you)") {
			t.Errorf("%v members\n%s", sz, v)
		}
		press(m, "esc")
		run(m, m.openChannel(10)) // plaintext: no marks
		if h := ansi.Strip(m.headerLine(m.w)); strings.Contains(h, "?") {
			t.Errorf("plaintext header %q", h)
		}
		runInput(t, m, "/members")
		if v := plain(m); strings.Contains(v, "? @bob") {
			t.Errorf("plaintext members\n%s", v)
		}
		press(m, "esc")
	}
}

// Names, fingerprints, logins and alarm text from the server are
// sanitized in every new string: banner, header, overlays, lists, notices.
func TestTrustStringsSanitized(t *testing.T) {
	const evil = "\x1b]0;pwned\a\x1b[2J‮"
	bad := func(s string) bool {
		return strings.Contains(s, "\x1b]") || strings.Contains(s, "\a") || strings.Contains(s, "‮") || strings.Contains(s, "\x1b[2J")
	}
	m, f := newTestModelFake(t, 120, 40)
	trustFake(f)
	f.users[1].Name = "bob" + evil
	sn := f.tr.sn[2]
	sn.Me.Fingerprints = []string{"SHA256:x" + evil}
	sn.Them.Fingerprints = []string{"SHA256:y" + evil}
	f.tr.sn[2] = sn
	f.tr.keyChg = map[uint64][]uint64{20: {2}}
	f.tr.gh = client.GitHubCheck{Pinned: []string{"SHA256:p" + evil}, GitHub: []string{"SHA256:g" + evil}, Missing: []string{"SHA256:p" + evil}}
	f.e2eSt[20] = client.E2EStatus{State: client.E2EAlarm, Detail: "fork" + evil}
	f.tr.alarms = map[uint64]client.AlarmInfo{20: {Detail: "fork" + evil, Why: "why" + evil}}
	run(m, m.applyUpdate(client.Update{Channels: true, Users: true}))
	run(m, m.openChannel(20))
	views := []string{m.View()}
	m.verify = verifyState{user: 2, sn: f.tr.sn[2]}
	m.mode = modeVerify
	views = append(views, m.View())
	m.mode = modeNormal
	res, _ := f.VerifyGitHub(context.Background(), 2, "gh"+evil)
	m.gotVerifyGH(verifyGHMsg{res: res})
	views = append(views, m.View())
	press(m, "esc")
	for _, line := range []string{"/alarm", "/members"} {
		runInput(t, m, line)
		views = append(views, m.View())
		press(m, "esc")
	}
	m.setNotice("accepted "+m.atName(2), false, false, 0)
	views = append(views, m.View(), m.bannerText())
	for i, v := range views {
		if bad(v) {
			t.Errorf("view %d not sanitized: %q", i, v)
		}
	}
}
