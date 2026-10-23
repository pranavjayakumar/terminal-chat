package tui

import "strings"

// keyNames picks how key hints are spelled. Mac keyboards print ⌥ ⌃ ⇧ ⏎ on
// the keys; elsewhere nobody reads those, so labels become Alt+K, Ctrl+P,
// Enter. Labels are written once, in the Mac form, and pass through of.
type keyNames struct{ pc bool }

var pcKeyWords = strings.NewReplacer("⌥", "Alt+", "⌃", "Ctrl+", "⇧", "Shift+", "⌘", "Cmd+", "⏎", "Enter", "⇥", "Tab")

// keyNamesFor chooses from TC_KEYS (mac|pc, e.g. ssh from a Mac into
// Linux) and then the OS.
func keyNamesFor(env, goos string) keyNames {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "mac":
		return keyNames{}
	case "pc":
		return keyNames{pc: true}
	}
	return keyNames{pc: goos != "darwin"}
}

// of spells the Mac-form label s for this keyboard. In the word form, a pair
// like "Alt+↑ Alt+↓" shrinks to "Alt+↑/↓"; a repeated key ("Ctrl+C Ctrl+C",
// press twice) stays as it is.
func (k keyNames) of(s string) string {
	if !k.pc {
		return s
	}
	s = pcKeyWords.Replace(s)
	f := strings.Split(s, " ")
	out := f[:0]
	for _, t := range f {
		if n := len(out); n > 0 {
			prev := out[n-1]
			pi, ti := strings.LastIndexByte(prev, '+'), strings.LastIndexByte(t, '+')
			if pi > 0 && pi == ti && prev[:pi] == t[:ti] && prev[pi+1:] != t[ti+1:] && !strings.Contains(prev, "/") {
				out[n-1] = prev + "/" + t[ti+1:]
				continue
			}
		}
		out = append(out, t)
	}
	return strings.Join(out, " ")
}
