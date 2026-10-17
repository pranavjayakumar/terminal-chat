package notify

import (
	"encoding/base64"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Terminal notifications (docs/research/NOTIFICATIONS-2026-10.md §2.4): the
// local terminal shows them, so they work over SSH, where osascript or
// notify-send would fire on the remote machine.

// oscDialect picks OSC 99 (kitty), 777 (Ghostty, WezTerm, foot, VTE) or 9
// (iTerm2) from the environment alone — Bubble Tea owns stdin, so no
// queries. 0: none known (the caller falls back to os/bell).
func oscDialect(e Env) int {
	switch strings.TrimSpace(e.Getenv("TC_NOTIFY_OSC")) {
	case "9":
		return 9
	case "777":
		return 777
	case "99":
		return 99
	}
	term, prog := e.Getenv("TERM"), e.Getenv("TERM_PROGRAM")
	switch {
	case term == "xterm-kitty" || e.Getenv("KITTY_WINDOW_ID") != "":
		return 99
	case term == "xterm-ghostty" || strings.HasPrefix(term, "foot") || prog == "WezTerm" || prog == "ghostty" ||
		e.Getenv("GHOSTTY_RESOURCES_DIR") != "" || e.Getenv("WEZTERM_PANE") != "":
		return 777
	case e.Getenv("LC_TERMINAL") == "iTerm2" || prog == "iTerm.app":
		return 9
	}
	return 0
}

// maxOSCText caps the text in one OSC 9/777 sequence.
const maxOSCText = 256

// oscText keeps s printable: no C0, DEL or C1 (any of which could end or
// start a sequence), at most max bytes, cut on a rune boundary.
func oscText(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == utf8.RuneError {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// oscSeq encodes one notification for dialect, wrapped for tmux
// passthrough when tmux is set (plus a BEL there, since tmux without
// allow-passthrough swallows the sequence silently). Text was clean()ed
// already; oscText filters again so no byte of it can end the sequence.
func oscSeq(dialect int, title, body string, cue Cue, tmux bool, id uint32) []byte {
	var seq string
	switch dialect {
	case 99:
		// kitty's protocol: base64 payloads, so nothing needs escaping.
		// o=always: tc applied its own focus rules already.
		b64 := base64.StdEncoding.EncodeToString
		meta := "i=tc" + strconv.FormatUint(uint64(id), 10)
		first := meta + ":d=0:p=title:e=1:o=always"
		switch cue {
		case CueRing:
			first += ":u=2"
		case CueNone:
			first += ":s=" + b64([]byte("silent"))
		}
		seq = "\x1b]99;" + first + ";" + b64([]byte(oscText(title, maxOSCText))) + "\x1b\\" +
			"\x1b]99;" + meta + ":p=body:e=1;" + b64([]byte(oscText(body, maxOSCText))) + "\x1b\\"
	case 777:
		t := strings.ReplaceAll(oscText(title, maxOSCText/2), ";", ",")
		seq = "\x1b]777;notify;" + t + ";" + oscText(body, maxOSCText/2) + "\x1b\\"
	case 9:
		text := oscText(title, maxOSCText/2)
		if b := oscText(body, maxOSCText/2); b != "" {
			text += ": " + b
		}
		// ConEmu and Windows Terminal read "9;<digits>;" as commands
		// (9;4 = progress).
		if text != "" && text[0] >= '0' && text[0] <= '9' {
			text = "tc · " + text
		}
		seq = "\x1b]9;" + text + "\x1b\\"
	default:
		return nil
	}
	if !tmux {
		return []byte(seq)
	}
	out := "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	if cue != CueNone {
		out += "\a"
	}
	return []byte(out)
}
