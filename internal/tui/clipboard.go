package tui

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"strings"
	"time"
)

// osc52 is the clipboard escape for text. tmux swallows OSC 52 unless
// set-clipboard is on, so inside tmux it is also sent wrapped in a DCS
// passthrough (needs allow-passthrough, which tmux 3.3+ has off by default;
// ExtCopy covers that case).
func osc52(text string, tmux bool) string {
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
	if tmux {
		seq += "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	return seq
}

// clip sends text to the clipboard by every available route and says how:
// "" means nothing was attempted, so callers must not claim "copied".
// "system" means a clipboard tool accepted it; "terminal" only that the
// escape was written (the terminal may ignore it). Over SSH the tools would
// reach the remote machine's clipboard, so only the terminal route is used.
// It may block (up to extCopyTimeout): call it from a tea.Cmd.
func clip(ctx context.Context, o Options, text string) (how string, err error) {
	if ext := o.ExtCopy; ext != nil && !o.SSH && ext(ctx, text) == nil {
		how = "system"
	}
	if out := o.Out; out != nil {
		if _, werr := out.Write([]byte(osc52(text, o.Tmux))); werr != nil {
			err = werr
		} else if how == "" {
			how = "terminal"
		}
	}
	if how != "" {
		err = nil
	}
	return how, err
}

const extCopyTimeout = 2 * time.Second

// defaultExtCopy pipes text to the first clipboard tool on PATH. xclip,
// xsel and wl-copy fork a daemon that keeps inheriting our pipes, so stdout
// and stderr stay nil (os/exec then uses /dev/null, nothing to wait for) and
// WaitDelay bounds the rest.
func defaultExtCopy(ctx context.Context, text string) error {
	ctx, cancel := context.WithTimeout(ctx, extCopyTimeout)
	defer cancel()
	for _, t := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "-ib"}} {
		path, err := exec.LookPath(t[0])
		if err != nil {
			continue
		}
		cmd := exec.CommandContext(ctx, path, t[1:]...)
		cmd.Stdin = strings.NewReader(text)
		cmd.WaitDelay = 500 * time.Millisecond
		if cmd.Run() == nil {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return os.ErrNotExist
}
