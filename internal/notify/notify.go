// Package notify shows desktop notifications and talks to herdr (the
// terminal multiplexer tc usually runs in) through its CLI.
//
// Everything runs external programs with exec.Command and an argv — never a
// shell — and every string that came from a remote user is passed through
// sanitize.Line and truncated first. Nothing here blocks the caller for
// longer than it takes to start a goroutine.
package notify

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skailama/tc/internal/sanitize"
)

// Backend names, as TC_NOTIFY spells them (plus the resolved OS variants).
const (
	Herdr     = "herdr"
	OSAScript = "osascript"   // macOS
	NotifySnd = "notify-send" // Linux
	Bell      = "bell"
	Off       = "off"
)

const (
	execTimeout = 2 * time.Second
	maxTitle    = 60
	maxBody     = 160
)

// Env is what backend selection looks at; tests fake it.
type Env struct {
	Getenv   func(string) string
	LookPath func(string) (string, error)
	GOOS     string
}

// OSEnv is the real environment.
func OSEnv() Env { return Env{Getenv: os.Getenv, LookPath: exec.LookPath, GOOS: runtime.GOOS} }

// pick resolves TC_NOTIFY (auto|herdr|os|bell|off) to a backend. An
// explicit choice that isn't available falls through like auto would, so a
// misconfigured TC_NOTIFY degrades to the bell instead of silence.
func pick(e Env) string {
	has := func(bin string) bool { _, err := e.LookPath(bin); return err == nil }
	osBackend := func() string {
		switch {
		case e.GOOS == "darwin" && has("osascript"):
			return OSAScript
		case e.GOOS != "darwin" && e.GOOS != "windows" && has("notify-send"):
			return NotifySnd
		}
		return ""
	}
	bell := func() string {
		if e.Getenv("TC_BELL") == "0" { // M1 switch: still turns the bell off
			return Off
		}
		return Bell
	}
	inHerdr := e.Getenv("HERDR_ENV") == "1" && has("herdr")
	switch strings.ToLower(strings.TrimSpace(e.Getenv("TC_NOTIFY"))) {
	case "off", "none", "0":
		return Off
	case "bell":
		return bell()
	case "herdr":
		if has("herdr") {
			return Herdr
		}
	case "os":
		if b := osBackend(); b != "" {
			return b
		}
		return bell()
	}
	if inHerdr {
		return Herdr
	}
	if b := osBackend(); b != "" {
		return b
	}
	return bell()
}

// Notifier shows notifications. At most one is in flight; while one is,
// further calls are dropped (the caller already rate-limits, so this only
// matters when a backend hangs).
type Notifier struct {
	backend string
	bell    io.Writer
	timeout time.Duration
	busy    atomic.Bool
	wg      sync.WaitGroup
}

// New picks a backend from e. bell receives "\a" for the bell backend; it
// should be the terminal.
func New(e Env, bell io.Writer) *Notifier {
	return &Notifier{backend: pick(e), bell: bell, timeout: execTimeout}
}

// Backend is the backend in use.
func (n *Notifier) Backend() string { return n.backend }

// Notify shows title/body asynchronously. It reports whether the
// notification was started (false: off, or another one is still running).
func (n *Notifier) Notify(title, body string) bool {
	if n == nil || n.backend == Off {
		return false
	}
	if !n.busy.CompareAndSwap(false, true) {
		return false
	}
	title, body = clean(title, maxTitle), clean(body, maxBody)
	if title == "" {
		title = "tc"
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer n.busy.Store(false)
		if n.backend == Bell {
			if n.bell != nil {
				_, _ = n.bell.Write([]byte("\a"))
			}
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), n.timeout)
		defer cancel()
		_ = run(ctx, n.argv(title, body))
	}()
	return true
}

// Wait blocks until the in-flight notification (if any) is done.
func (n *Notifier) Wait() {
	if n != nil {
		n.wg.Wait()
	}
}

// osaScript reads the text from argv so it is never parsed as AppleScript.
const osaScript = "on run argv\ndisplay notification (item 2 of argv) with title (item 1 of argv)\nend run"

func (n *Notifier) argv(title, body string) []string {
	switch n.backend {
	case Herdr:
		// herdr takes the positional first and options after it.
		a := []string{"herdr", "notification", "show", title}
		if body != "" {
			a = append(a, "--body", body)
		}
		return append(a, "--sound", "request")
	case OSAScript:
		// "--" so a title starting with "-" isn't an osascript option.
		return []string{"osascript", "-e", osaScript, "--", title, body}
	case NotifySnd:
		return []string{"notify-send", "--app-name=tc", "--", title, body}
	}
	return nil
}

// clean makes remote text safe as a single argv element: one line, no
// controls, at most n runes.
func clean(s string, n int) string {
	s = strings.Join(strings.Fields(sanitize.Line(s)), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n-1]) + "…"
	}
	return s
}

// run executes argv without a shell, discarding output, killing it when
// ctx ends.
func run(ctx context.Context, argv []string) error {
	_, err := output(ctx, argv, 0)
	return err
}

// output runs argv and returns at most limit bytes of stdout (0: discard).
func output(ctx context.Context, argv []string, limit int) ([]byte, error) {
	if len(argv) == 0 {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = nil
	cmd.Stderr = nil
	var buf *limitBuf
	if limit > 0 {
		buf = &limitBuf{max: limit}
		cmd.Stdout = buf
	}
	// Don't wait for grandchildren holding the pipe after the kill.
	cmd.WaitDelay = 500 * time.Millisecond
	err := cmd.Run()
	if buf == nil {
		return nil, err
	}
	return buf.b, err
}

type limitBuf struct {
	b   []byte
	max int
}

func (l *limitBuf) Write(p []byte) (int, error) {
	if room := l.max - len(l.b); room > 0 {
		l.b = append(l.b, p[:min(len(p), room)]...)
	}
	return len(p), nil
}
