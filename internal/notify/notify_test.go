package notify

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeEnv(vars map[string]string, bins ...string) Env {
	return Env{
		Getenv: func(k string) string { return vars[k] },
		LookPath: func(b string) (string, error) {
			if slices.Contains(bins, b) {
				return "/fake/" + b, nil
			}
			return "", exec.ErrNotFound
		},
		GOOS: "darwin",
	}
}

func TestPick(t *testing.T) {
	herdr := map[string]string{"HERDR_ENV": "1"}
	cases := []struct {
		name string
		vars map[string]string
		goos string
		bins []string
		want string
	}{
		{"auto herdr", herdr, "darwin", []string{"herdr", "osascript"}, Herdr},
		{"auto herdr env but no binary", herdr, "darwin", []string{"osascript"}, OSAScript},
		{"auto no herdr env", nil, "darwin", []string{"herdr", "osascript"}, OSAScript},
		{"auto linux", nil, "linux", []string{"notify-send"}, NotifySnd},
		{"auto linux no notify-send", nil, "linux", nil, Bell},
		{"auto mac without osascript", nil, "darwin", nil, Bell},
		{"auto TC_BELL=0", map[string]string{"TC_BELL": "0"}, "linux", nil, Off},
		{"TC_BELL=0 keeps os", map[string]string{"TC_BELL": "0"}, "darwin", []string{"osascript"}, OSAScript},
		{"off", map[string]string{"TC_NOTIFY": "off", "HERDR_ENV": "1"}, "darwin", []string{"herdr"}, Off},
		{"bell", map[string]string{"TC_NOTIFY": "bell", "HERDR_ENV": "1"}, "darwin", []string{"herdr"}, Bell},
		{"bell with TC_BELL=0", map[string]string{"TC_NOTIFY": "bell", "TC_BELL": "0"}, "darwin", nil, Off},
		{"os inside herdr", map[string]string{"TC_NOTIFY": "os", "HERDR_ENV": "1"}, "darwin", []string{"herdr", "osascript"}, OSAScript},
		{"os unavailable", map[string]string{"TC_NOTIFY": "os"}, "linux", nil, Bell},
		{"explicit herdr outside herdr", map[string]string{"TC_NOTIFY": "HERDR"}, "darwin", []string{"herdr"}, Herdr},
		{"explicit herdr missing", map[string]string{"TC_NOTIFY": "herdr"}, "darwin", []string{"osascript"}, OSAScript},
	}
	for _, c := range cases {
		e := fakeEnv(c.vars, c.bins...)
		e.GOOS = c.goos
		if got := pick(e); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// fakeBin writes an executable that records its argv (NUL-separated) to
// a log file and puts its directory first in PATH.
func fakeBin(t *testing.T, name, extra string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, name+".log")
	script := "#!/bin/sh\nprintf '%s\\0' \"$0\" \"$@\" > '" + log + ".tmp' && mv '" + log + ".tmp' '" + log + "'\n" + extra
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func readArgv(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("fake binary did not run: %v", err)
	}
	parts := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	return parts[1:] // drop $0
}

const hostile = "evil\"; do shell script \"rm -rf ~\"\n\x1b]0;pwn\x07line2 $(touch /tmp/x) `id`"

func newWith(t *testing.T, vars map[string]string) *Notifier {
	t.Helper()
	e := OSEnv()
	e.Getenv = func(k string) string { return vars[k] }
	n := New(e, nil)
	n.timeout = slowExec
	return n
}

// slowExec is the timeout for fake binaries that should finish: a fresh
// executable can take seconds to start on a loaded macOS machine.
const slowExec = 30 * time.Second

func TestHerdrArgv(t *testing.T) {
	log := fakeBin(t, "herdr", "")
	n := newWith(t, map[string]string{"HERDR_ENV": "1"})
	if n.Backend() != Herdr {
		t.Fatalf("backend %s", n.Backend())
	}
	if !n.Notify("#general · bob", hostile) {
		t.Fatal("dropped")
	}
	n.Wait()
	got := readArgv(t, log)
	body := clean(hostile, maxBody)
	want := []string{"notification", "show", "#general · bob", "--body=" + body, "--sound", "request"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
	if strings.ContainsAny(body, "\n\x1b\x07") || !strings.Contains(body, `do shell script "rm -rf ~"`) {
		t.Fatalf("body not sanitized as expected: %q", body)
	}
}

func TestOSAScriptArgv(t *testing.T) {
	log := fakeBin(t, "osascript", "")
	n := newWith(t, map[string]string{})
	if n.Backend() != OSAScript {
		t.Skip("not macOS")
	}
	n.Notify(hostile, "body\nmore")
	n.Wait()
	got := readArgv(t, log)
	want := []string{"-e", osaScript, "--", clean(hostile, maxTitle), "body more", "Glass"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
	if strings.Contains(osaScript, "rm") {
		t.Fatal("text leaked into the script")
	}
	if len([]rune(got[3])) > maxTitle {
		t.Fatalf("title not truncated: %d runes", len([]rune(got[3])))
	}
}

func TestNotifySendArgv(t *testing.T) {
	log := fakeBin(t, "notify-send", "")
	e := OSEnv()
	e.Getenv = func(string) string { return "" }
	e.GOOS = "linux"
	n := New(e, nil)
	n.timeout = slowExec
	if n.Backend() != NotifySnd {
		t.Fatalf("backend %s", n.Backend())
	}
	n.Notify("-x title", hostile)
	n.Wait()
	got := readArgv(t, log)
	want := []string{"--app-name=tc", "--category=im.received", "--hint=string:sound-name:message-new-instant", "--", "-x title", clean(hostile, maxBody)}
	if !slices.Equal(got, want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
}

func TestBellBackend(t *testing.T) {
	var buf syncBuf
	e := fakeEnv(map[string]string{"TC_NOTIFY": "bell"})
	n := New(e, &buf)
	n.Notify("t", "b")
	n.Wait()
	if buf.String() != "\a" {
		t.Fatalf("bell wrote %q", buf.String())
	}
	off := New(fakeEnv(map[string]string{"TC_NOTIFY": "off"}), &buf)
	if off.Notify("t", "b") {
		t.Fatal("off backend notified")
	}
}

func TestTimeoutAndBusyDrop(t *testing.T) {
	log := fakeBin(t, "herdr", "[ \"$3\" = one ] && exec sleep 10\nexit 0\n")
	n := newWith(t, map[string]string{"HERDR_ENV": "1"})
	n.timeout = 300 * time.Millisecond
	start := time.Now()
	if !n.Notify("one", "") {
		t.Fatal("first dropped")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("Notify blocked")
	}
	if n.Notify("two", "") {
		t.Fatal("second should be dropped while the first runs")
	}
	n.Wait()
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout not enforced: %v", d)
	}
	n.timeout = slowExec
	if !n.Notify("three", "") {
		t.Fatal("not accepted after the first finished")
	}
	n.Wait()
	if got := readArgv(t, log); got[2] != "three" {
		t.Fatalf("argv %q", got)
	}
}

func TestHerdrCLI(t *testing.T) {
	log := fakeBin(t, "herdr", `printf '{"id":"x","result":{"panes":[{"pane_id":"w1:p1","tab_id":"w1:t1","workspace_id":"w1","focused":true,"agent":"claude"},{"pane_id":"w1:p2","tab_id":"w1:t2","label":"server","terminal_title_stripped":"make dev"}]}}'`+"\n")
	h := DetectHerdr(Env{
		Getenv: func(k string) string {
			return map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "w1:p1", "HERDR_TAB_ID": "w1:t1", "HERDR_WORKSPACE_ID": "w1"}[k]
		},
		LookPath: exec.LookPath,
	})
	if h == nil {
		t.Fatal("not detected")
	}
	h.timeout = slowExec
	ps, err := h.Panes(context.Background())
	if err != nil || len(ps) != 2 {
		t.Fatalf("panes %v %v", ps, err)
	}
	if ps[0].Name() != "claude" || ps[1].Name() != "server" || !ps[0].Focused || ps[1].Tab != "w1:t2" {
		t.Fatalf("parsed %+v", ps)
	}
	if got := readArgv(t, log); !slices.Equal(got, []string{"pane", "list", "--workspace", "w1"}) {
		t.Fatalf("argv %q", got)
	}
	if err := h.SetTitle(context.Background(), "tc · 3 unread\x1b[2J"); err != nil {
		t.Fatal(err)
	}
	if got := readArgv(t, log); !slices.Equal(got, []string{"pane", "report-metadata", "w1:p1", "--source", "tc", "--title", "tc · 3 unread[2J"}) {
		t.Fatalf("argv %q", got)
	}
	_ = h.SetTitle(context.Background(), "")
	if got := readArgv(t, log); !slices.Equal(got, []string{"pane", "report-metadata", "w1:p1", "--source", "tc", "--clear-title"}) {
		t.Fatalf("argv %q", got)
	}
	_, _ = h.ReadPane(context.Background(), "w1:p2", 60)
	if got := readArgv(t, log); !slices.Equal(got, []string{"pane", "read", "w1:p2", "--source", "recent", "--lines", "60", "--format", "text"}) {
		t.Fatalf("argv %q", got)
	}
	if DetectHerdr(fakeEnv(map[string]string{"HERDR_ENV": "1"}, "herdr")) != nil {
		t.Fatal("detected without a pane id")
	}
}

func TestParseRead(t *testing.T) {
	if got := ParseRead([]byte(`{"result":{"text":"a\nb"}}`)); got != "a\nb" {
		t.Fatalf("json: %q", got)
	}
	if got := ParseRead([]byte("plain\n")); got != "plain\n" {
		t.Fatalf("plain: %q", got)
	}
	if got := ParseRead([]byte("{not json")); got != "{not json" {
		t.Fatalf("broken json: %q", got)
	}
}

func TestHerdrError(t *testing.T) {
	fakeBin(t, "herdr", "exit 3\n")
	h := &HerdrCLI{Pane: "p", bin: "herdr", timeout: slowExec}
	var ee *exec.ExitError
	if _, err := h.Panes(context.Background()); !errors.As(err, &ee) {
		t.Fatalf("want exit error, got %v", err)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
