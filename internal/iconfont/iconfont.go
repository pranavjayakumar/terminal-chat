// Package iconfont carries tc Symbols, tc's small icon font, and installs
// it where the terminal's font fallback finds it. The font is generated
// from assets/iconfont/icons.txt by scripts/iconfont.py and embedded here;
// tc needs no font tooling at run time.
package iconfont

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// TTF is the font file.
//
//go:embed tc-symbols.ttf
var TTF []byte

// File is the installed font's file name.
const File = "tc-symbols.ttf"

// First and Last bound the font's code points: Private Use, in the gap
// Nerd Fonts leave between its Weather (to U+E3E3) and Seti-UI (from
// U+E5FA) sets, so a Nerd Font user still gets these icons.
const (
	First rune = 0xE400
	Last  rune = 0xE419
)

// State is whether the font is installed and current.
type State int

const (
	Missing State = iota
	Stale         // installed, but not this build's
	Current
)

func (s State) String() string {
	return [...]string{"not installed", "installed, out of date", "installed"}[s]
}

// ErrUnsupported: this OS has no per-user font folder tc knows.
var ErrUnsupported = errors.New("tc can install its icon font on macOS and Linux only")

// Path is where the font goes for this user: ~/Library/Fonts on macOS,
// $XDG_DATA_HOME/fonts (~/.local/share/fonts) on Linux.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Fonts", File), nil
	case "linux", "freebsd", "openbsd", "netbsd":
		data := os.Getenv("XDG_DATA_HOME")
		if !filepath.IsAbs(data) {
			data = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(data, "fonts", File), nil
	}
	return "", ErrUnsupported
}

// Status reports the installed font's state and where it is (or goes).
func Status() (State, string, error) {
	p, err := Path()
	if err != nil {
		return Missing, "", err
	}
	b, err := os.ReadFile(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Missing, p, nil
	case err != nil:
		return Missing, p, err
	case bytes.Equal(b, TTF):
		return Current, p, nil
	}
	return Stale, p, nil
}

// Installed reports whether some build of the font is on this machine.
func Installed() bool {
	p, err := Path()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Install writes the font atomically (a temp file renamed into place) and,
// on Linux, refreshes the font cache when fc-cache exists. It returns the
// font's path.
func Install() (string, error) {
	p, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", fmt.Errorf("creating the font folder: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tc-symbols-*.ttf")
	if err != nil {
		return "", fmt.Errorf("writing the font: %w", err)
	}
	_, werr := tmp.Write(TTF)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o644)
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), p)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("writing the font: %w", werr)
	}
	refreshCache(filepath.Dir(p))
	return p, nil
}

// Remove deletes the installed font; existed is false when there was none.
func Remove() (path string, existed bool, err error) {
	p, err := Path()
	if err != nil {
		return "", false, err
	}
	switch err := os.Remove(p); {
	case errors.Is(err, os.ErrNotExist):
		return p, false, nil
	case err != nil:
		return p, true, fmt.Errorf("removing the font: %w", err)
	}
	refreshCache(filepath.Dir(p))
	return p, true, nil
}

// Refresh replaces an installed font that isn't this build's; it reports
// whether it did. A missing font stays missing: installing is the user's
// choice.
func Refresh() (bool, error) {
	s, _, err := Status()
	if err != nil || s != Stale {
		return false, err
	}
	if _, err := Install(); err != nil {
		return false, err
	}
	return true, nil
}

// refreshCache runs fc-cache on the font folder where fontconfig is the
// fallback (Linux); best effort, as the font works after a restart anyway.
func refreshCache(dir string) {
	if runtime.GOOS == "darwin" {
		return
	}
	if fc, err := exec.LookPath("fc-cache"); err == nil {
		exec.Command(fc, "-f", dir).Run()
	}
}
