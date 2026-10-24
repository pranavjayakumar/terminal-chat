package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// KeepFor values for Fact.KeepFor.
const (
	KeepToday   = "today"
	KeepWeek    = "week"
	KeepMonth   = "month"
	KeepForever = "forever"
)

// Fact is one line for the daily log.
type Fact struct {
	Subject string // who or what it is about
	Text    string // the fact, one line
	KeepFor string // today, week, month, forever ("" = forever)
	Source  string // who said it: a handle, "web", "hook:stop"
	Trust   Trust
	Private bool // owner's confidence: only shown for the owner's asks
}

// Remember appends f to today's daily log. Text over FactMaxBytes is refused
// so the caller splits it; a full day's log fails with ErrMergeFirst.
func (s *Store) Remember(f Fact) error {
	line, err := s.dailyLine(f)
	if err != nil {
		return err
	}
	day := s.now().Format("2006-01-02")
	rel := "daily/" + day + ".md"
	return s.withLock(func() error {
		p := filepath.Join(s.dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return fmt.Errorf("creating daily dir: %w", err)
		}
		fi, err := os.Stat(p)
		header := ""
		switch {
		case err == nil && fi.Size()+int64(len(line)) > DailyMaxBytes:
			return fmt.Errorf("daily log %s is full: %w", day, ErrMergeFirst)
		case err != nil && errors.Is(err, os.ErrNotExist):
			header = "# " + day + "\n\n"
		case err != nil:
			return fmt.Errorf("checking daily log: %w", err)
		}
		fh, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("opening daily log: %w", err)
		}
		if _, err := fh.WriteString(header + line); err != nil {
			fh.Close()
			return fmt.Errorf("appending to daily log: %w", err)
		}
		return fh.Close()
	})
}

func (s *Store) dailyLine(f Fact) (string, error) {
	subject := oneLine(f.Subject, 1<<20)
	text := oneLine(f.Text, 1<<20)
	switch {
	case subject == "" || text == "":
		return "", errors.New("a fact needs a subject and text")
	case len(subject) > SubjectMaxBytes:
		return "", fmt.Errorf("subject is over %d bytes", SubjectMaxBytes)
	case len(text) > FactMaxBytes:
		return "", fmt.Errorf("fact is over %d bytes: split it into smaller facts", FactMaxBytes)
	case !f.Trust.Valid():
		return "", fmt.Errorf("bad trust %q", f.Trust)
	}
	keep := f.KeepFor
	if keep == "" {
		keep = KeepForever
	}
	switch keep {
	case KeepToday, KeepWeek, KeepMonth, KeepForever:
	default:
		return "", fmt.Errorf("keep_for %q: want today, week, month or forever", f.KeepFor)
	}
	tags := []string{string(f.Trust), "src=" + sourceTag(f.Source), "keep=" + keep}
	if f.Private {
		tags = append(tags, "private")
	}
	return fmt.Sprintf("- %s [%s] %s: %s\n", s.now().Format("15:04"), strings.Join(tags, " "), subject, text), nil
}

// sourceTag keeps the source to characters that cannot break the tag syntax.
func sourceTag(src string) string {
	var b strings.Builder
	for _, r := range src {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.:@#-", r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := cutBytes(b.String(), 64)
	if out == "" {
		return "unknown"
	}
	return out
}

var dailyLineRe = regexp.MustCompile(`^- \d\d:\d\d \[([^\]]*)\] `)

// linePrivate reports whether a daily log line carries the private tag.
func linePrivate(line string) bool {
	m := dailyLineRe.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	for _, t := range strings.Fields(m[1]) {
		if t == "private" {
			return true
		}
	}
	return false
}

// Daily returns the daily log for day, without private lines unless
// includePrivate is set.
func (s *Store) Daily(day time.Time, includePrivate bool) (string, error) {
	text, err := s.readRel("daily/" + day.Format("2006-01-02") + ".md")
	if err != nil || includePrivate {
		return text, err
	}
	return filterPrivate(text), nil
}

func filterPrivate(text string) string {
	if !strings.Contains(text, "private") {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	out := lines[:0]
	for _, l := range lines {
		if !linePrivate(l) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "")
}
