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
	"strconv"
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
	Terminal  = "terminal" // OSC 9/777/99 to the terminal (osc.go)
	Off       = "off"
)

// Cue is a notification's sound class (docs/research/NOTIFICATIONS-2026-10.md
// §2.3). The sound itself comes from the backend that shows the popup —
// herdr, Notification Center, the freedesktop server, the terminal — from
// fixed tables, never from user text: one event, one sound.
type Cue uint8

const (
	CueNone    Cue = iota // a silent popup
	CueMessage            // soft: any message in a level-all channel
	CueMention            // DMs, mentions, highlights, replies, reminders
	CueRing               // someone started a huddle in a DM
	CueHuddle             // a channel huddle started (level all)
)

// Note is one notification. Tag groups a channel's notifications where
// the backend can replace the previous one (notify-send); it must be
// short ASCII the caller made (e.g. "tc-12"), never remote text.
type Note struct {
	Title, Body string
	Cue         Cue
	Tag         string
}

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
	overSSH := e.Getenv("SSH_CONNECTION") != "" || e.Getenv("SSH_TTY") != ""
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
	case "terminal":
		if oscDialect(e) != 0 {
			return Terminal
		}
		return bell()
	}
	if inHerdr {
		return Herdr // herdr owns delivery: never OSC inside it
	}
	// Over SSH the OS notifier would fire on the remote machine; the
	// terminal's own notifications show up where the user is.
	if overSSH && oscDialect(e) != 0 {
		return Terminal
	}
	if b := osBackend(); b != "" {
		return b
	}
	return bell()
}

// Notifier shows notifications. At most one is in flight; while one is,
// further calls are dropped (the caller already rate-limits, so this only
// matters when a backend hangs). A ring has its own slot, so a hung
// message popup can't swallow it.
type Notifier struct {
	backend  string
	dialect  int  // OSC dialect for Terminal
	tmux     bool // wrap OSC for tmux passthrough
	bell     io.Writer
	bellMu   sync.Mutex // one terminal write at a time
	timeout  time.Duration
	busy     atomic.Bool
	busyRing atomic.Bool
	seq      atomic.Uint32 // OSC 99 notification ids
	wg       sync.WaitGroup
}

// New picks a backend from e. bell receives "\a" for the bell backend and
// the OSC sequences for the terminal backend; it should be the terminal.
func New(e Env, bell io.Writer) *Notifier {
	return &Notifier{backend: pick(e), dialect: oscDialect(e), tmux: e.Getenv("TMUX") != "",
		bell: bell, timeout: execTimeout}
}

// Backend is the backend in use.
func (n *Notifier) Backend() string { return n.backend }

// Dialect names the OSC dialect the terminal backend uses ("" if none).
func (n *Notifier) Dialect() string {
	if n == nil || n.dialect == 0 {
		return ""
	}
	return "OSC " + strconv.Itoa(n.dialect)
}

// Notify shows title/body with the mention sound.
func (n *Notifier) Notify(title, body string) bool {
	return n.Send(Note{Title: title, Body: body, Cue: CueMention})
}

// Send shows nt asynchronously. It reports whether the notification was
// started (false: off, or another one is still running).
func (n *Notifier) Send(nt Note) bool {
	if n == nil || n.backend == Off {
		return false
	}
	slot := &n.busy
	if nt.Cue == CueRing {
		slot = &n.busyRing
	}
	if !slot.CompareAndSwap(false, true) {
		return false
	}
	nt.Title, nt.Body = clean(nt.Title, maxTitle), clean(nt.Body, maxBody)
	if nt.Title == "" {
		nt.Title = "tc"
	}
	if !validTag(nt.Tag) {
		nt.Tag = ""
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer slot.Store(false)
		switch n.backend {
		case Bell:
			if nt.Cue != CueNone { // sounds off: a bell is nothing but a sound
				n.write([]byte("\a"))
			}
			return
		case Terminal:
			n.write(oscSeq(n.dialect, nt.Title, nt.Body, nt.Cue, n.tmux, n.seq.Add(1)))
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), n.timeout)
		defer cancel()
		_ = run(ctx, n.argv(nt))
	}()
	return true
}

func (n *Notifier) write(b []byte) {
	if n.bell == nil || len(b) == 0 {
		return
	}
	n.bellMu.Lock()
	defer n.bellMu.Unlock()
	_, _ = n.bell.Write(b) // one Write per sequence (between frames)
}

// validTag accepts what notify-send hints may carry: short [a-z0-9-].
func validTag(s string) bool {
	if len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// Wait blocks until the in-flight notification (if any) is done.
func (n *Notifier) Wait() {
	if n != nil {
		n.wg.Wait()
	}
}

// osaScript reads the text from argv so it is never parsed as AppleScript;
// a third item, from macSound, names the sound.
const osaScript = "on run argv\nif (count of argv) > 2 then\ndisplay notification (item 2 of argv) with title (item 1 of argv) sound name (item 3 of argv)\nelse\ndisplay notification (item 2 of argv) with title (item 1 of argv)\nend if\nend run"

// Sound tables (§2.3). Only these constants ever reach a sound argument.
var (
	herdrSound = map[Cue]string{CueNone: "none", CueMessage: "none", CueMention: "request", CueRing: "request", CueHuddle: "done"}
	// /System/Library/Sounds
	macSound = map[Cue]string{CueMessage: "Tink", CueMention: "Glass", CueRing: "Submarine", CueHuddle: "Pop"}
	// freedesktop sound naming spec
	xdgSound = map[Cue]string{CueMessage: "message-new-instant", CueMention: "message-new-instant", CueRing: "phone-incoming-call", CueHuddle: "service-login"}
)

func (n *Notifier) argv(nt Note) []string {
	title, body := nt.Title, nt.Body
	switch n.backend {
	case Herdr:
		// herdr takes the positional first and options after it.
		a := []string{"herdr", "notification", "show", title}
		if body != "" {
			a = append(a, "--body="+body) // one element: a body like "-1" can't read as a flag
		}
		return append(a, "--sound", herdrSound[nt.Cue])
	case OSAScript:
		// "--" so a title starting with "-" isn't an osascript option.
		a := []string{"osascript", "-e", osaScript, "--", title, body}
		if s := macSound[nt.Cue]; s != "" {
			a = append(a, s)
		}
		return a
	case NotifySnd:
		a := []string{"notify-send", "--app-name=tc"}
		switch nt.Cue {
		case CueRing:
			a = append(a, "--category=call.incoming", "--urgency=critical")
		case CueHuddle:
			a = append(a, "--category=call")
		default:
			a = append(a, "--category=im.received")
		}
		if s := xdgSound[nt.Cue]; s != "" {
			a = append(a, "--hint=string:sound-name:"+s)
		} else {
			a = append(a, "--hint=boolean:suppress-sound:true")
		}
		if nt.Tag != "" { // a channel's newer popup replaces its older one
			a = append(a, "--hint=string:x-dunst-stack-tag:"+nt.Tag, "--hint=string:x-canonical-private-synchronous:"+nt.Tag)
		}
		return append(a, "--", title, body)
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
