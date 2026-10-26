package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"unicode/utf8"
)

// Size caps for the owner's files. Both go into every session-start snapshot,
// which Claude Code caps at 10,000 characters.
const (
	SoulMaxBytes = 2048
	MeMaxBytes   = 2048
)

// ErrTooBig is returned when soul.md or me.md would go over its cap.
var ErrTooBig = errors.New("too long: keep it under 2 KB")

// ReadSoul returns soul.md, or "" when it is missing.
func ReadSoul(dir string) (string, error) { return readOptional(PathsFor(dir).Soul) }

// ReadMe returns me.md, or "" when it is missing.
func ReadMe(dir string) (string, error) { return readOptional(PathsFor(dir).Me) }

// WriteSoul replaces soul.md. Callers check that the ask came from the owner.
func WriteSoul(dir, text string) error { return writeOwnerFile(PathsFor(dir).Soul, text, SoulMaxBytes) }

// WriteMe replaces me.md. Callers check that the ask came from the owner.
func WriteMe(dir, text string) error { return writeOwnerFile(PathsFor(dir).Me, text, MeMaxBytes) }

func writeOwnerFile(path, text string, max int) error {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if !utf8.ValidString(text) {
		return errors.New("text is not valid UTF-8")
	}
	if text == "" {
		return errors.New("text is empty")
	}
	if len(text) > max {
		return ErrTooBig
	}
	return WriteFileAtomic(path, []byte(text+"\n"), 0o600)
}

func readOptional(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return string(b), nil
}

// IsDefaultSoul reports whether soul.md is still the one tc shipped, which
// means setup has not run yet.
func IsDefaultSoul(dir string) (bool, error) {
	cur, err := ReadSoul(dir)
	if err != nil {
		return false, err
	}
	def, err := defaultSoul()
	if err != nil {
		return false, err
	}
	return cur == "" || cur == def, nil
}
