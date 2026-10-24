package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RecallMaxHits caps how many lines Recall returns.
const RecallMaxHits = 40

// Hit is one memory line that matched a Recall query.
type Hit struct {
	Path string // memory file, e.g. people/ana.md
	Line int    // 1-based
	Text string
}

// Recall greps memory for lines holding every word of query, ignoring case.
// Without includePrivate it skips private notes, private daily lines and
// handoffs (a handoff may come from the owner's DM). Index and notes come
// first, then daily logs newest first.
func (s *Store) Recall(query string, includePrivate bool) ([]Hit, error) {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil, errors.New("empty query")
	}
	files, err := s.List()
	if err != nil {
		return nil, err
	}
	var hits []Hit
	for _, f := range files {
		if !includePrivate && (f.Private || f.Kind == KindHandoff) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, filepath.FromSlash(f.Path)))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("recalling: %w", err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !matchAll(strings.ToLower(line), words) {
				continue
			}
			if !includePrivate && f.Kind == KindDaily && linePrivate(line) {
				continue
			}
			hits = append(hits, Hit{Path: f.Path, Line: i + 1, Text: oneLine(line, 300)})
			if len(hits) >= RecallMaxHits {
				return hits, nil
			}
		}
	}
	return hits, nil
}

func matchAll(line string, words []string) bool {
	for _, w := range words {
		if !strings.Contains(line, w) {
			return false
		}
	}
	return true
}
