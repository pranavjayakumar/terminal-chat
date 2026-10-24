package memory

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// handoffRel maps a session key (a thread, a DM topic) to its handoff file.
func handoffRel(key, suffix string) (string, error) {
	var b strings.Builder
	for _, r := range key {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "_")
	if name == "" {
		return "", fmt.Errorf("bad session key %q", key)
	}
	if len(name) > 90 {
		name = name[:90]
	}
	return "handoff/" + name + suffix + ".md", nil
}

// WriteHandoff replaces the handoff note for a session key: what the next
// session on that thread needs to pick up where this one stopped.
func (s *Store) WriteHandoff(key, text string) error {
	return s.writeHandoff(key, "", text)
}

// ReadHandoff returns the handoff note for a session key, or "".
func (s *Store) ReadHandoff(key string) (string, error) {
	rel, err := handoffRel(key, "")
	if err != nil {
		return "", err
	}
	return s.readRel(rel)
}

// WriteCheckpoint saves the context Claude Code is about to compact away for
// a session key. It sits beside the handoff and is shown when no handoff is.
func (s *Store) WriteCheckpoint(key, text string) error {
	return s.writeHandoff(key, ".checkpoint", text)
}

// ReadCheckpoint returns the checkpoint for a session key, or "".
func (s *Store) ReadCheckpoint(key string) (string, error) {
	rel, err := handoffRel(key, ".checkpoint")
	if err != nil {
		return "", err
	}
	return s.readRel(rel)
}

func (s *Store) writeHandoff(key, suffix, text string) error {
	rel, err := handoffRel(key, suffix)
	if err != nil {
		return err
	}
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if !utf8.ValidString(text) {
		return errors.New("handoff is not valid UTF-8")
	}
	if len(text)+1 > HandoffMaxBytes {
		return fmt.Errorf("handoff is %d bytes, cap %d: %w", len(text), HandoffMaxBytes, ErrMergeFirst)
	}
	return s.withLock(func() error { return s.writeRel(rel, text+"\n") })
}
