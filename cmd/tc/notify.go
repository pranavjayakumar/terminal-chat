package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/skailama/tc/internal/sanitize"
)

// cmdNotify runs a command (output passes through untouched), then posts its
// result: `tc notify [--to '#chan'|@user] [--tail N] [--on-fail] -- make build`.
// It exits with the command's exit code.
func cmdNotify(args []string) error {
	fs := flag.NewFlagSet("notify", flag.ExitOnError)
	to := fs.String("to", "@me", "where to post: #channel, @user, or @me (your notes)")
	tailN := fs.Int("tail", 20, "lines of output to include when the command fails")
	onFail := fs.Bool("on-fail", false, "only post when the command fails")
	fs.Parse(args)
	argv := fs.Args()
	if len(argv) == 0 {
		return errUsage
	}

	tail := newTailBuffer(*tailN)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(os.Stdout, tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)
	start := time.Now()
	runErr := cmd.Run()
	took := time.Since(start)

	code := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(runErr, &exitErr):
		code = exitErr.ExitCode()
	case runErr != nil:
		code = 127
		fmt.Fprintln(os.Stderr, "tc notify:", runErr)
	}
	if code == 0 && *onFail {
		return nil
	}

	body := notifyBody(argv, code, took, tail.String())
	ctx, cancel := signalContext()
	defer cancel()
	if err := postTo(ctx, *to, body); err != nil {
		fmt.Fprintln(os.Stderr, "tc notify: couldn't post result:", err)
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

func notifyBody(argv []string, code int, took time.Duration, tail string) string {
	cmdline := sanitize.Line(sanitize.StripANSI(strings.Join(argv, " ")))
	if len(cmdline) > 200 {
		cut := 200
		for cut > 0 && cmdline[cut]&0xC0 == 0x80 { // invalid UTF-8 would be rejected
			cut--
		}
		cmdline = cmdline[:cut] + "…"
	}
	cmdline = strings.ReplaceAll(cmdline, "`", "'")
	took = took.Round(time.Second)
	if took == 0 {
		took = time.Second
	}
	if code == 0 {
		return fmt.Sprintf("✓ `%s` · %s", cmdline, took)
	}
	head := fmt.Sprintf("✗ `%s` · exit %d · %s", cmdline, code, took)
	if t := strings.TrimSpace(tail); t != "" {
		return head + "\n" + pipedBody(t, false)
	}
	return head
}

// postTo sends body to "#chan", "@user" or "@me" and waits until it's out.
func postTo(ctx context.Context, target, body string) error {
	c, err := connected(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	ch, err := resolve(ctx, c, target)
	if err != nil {
		return err
	}
	return sendWait(ctx, c, ch, body)
}

// sendWait queues body and waits for that message only: a row the server
// rejected in an earlier run (still shown as failed in the TUI) must not
// fail this one.
func sendWait(ctx context.Context, c interface {
	SendLocal(ch uint64, body string, thread uint64) (int64, error)
	FlushLocal(ctx context.Context, ids ...int64) error
}, ch uint64, body string) error {
	id, err := c.SendLocal(ch, body, 0)
	if err != nil {
		return err
	}
	fctx, fcancel := context.WithTimeout(ctx, 20*time.Second)
	defer fcancel()
	return c.FlushLocal(fctx, id)
}

// tailBuffer keeps the last n lines written to it (bounded in bytes too).
type tailBuffer struct {
	mu    sync.Mutex
	n     int
	lines [][]byte
	cur   []byte
}

const maxTailLine = 1 << 10

func newTailBuffer(n int) *tailBuffer { return &tailBuffer{n: max(n, 0)} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.n == 0 {
		return n, nil
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			t.cur = appendCapped(t.cur, p)
			break
		}
		t.cur = appendCapped(t.cur, p[:i])
		t.lines = append(t.lines, t.cur)
		if len(t.lines) > t.n {
			t.lines = t.lines[1:]
		}
		t.cur = nil
		p = p[i+1:]
	}
	return n, nil
}

func appendCapped(dst, src []byte) []byte {
	if room := maxTailLine - len(dst); room < len(src) {
		src = src[:max(room, 0)]
	}
	return append(dst, src...)
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := t.lines
	if len(t.cur) > 0 {
		lines = append(append([][]byte{}, lines...), t.cur)
		if len(lines) > t.n {
			lines = lines[1:]
		}
	}
	return string(bytes.Join(lines, []byte("\n")))
}
