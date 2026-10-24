package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Kind is what a memory file is, from where it lives.
type Kind string

const (
	KindIndex   Kind = "index"
	KindPerson  Kind = "person"
	KindChannel Kind = "channel"
	KindFact    Kind = "fact"
	KindDaily   Kind = "daily"
	KindHandoff Kind = "handoff"
)

var kindDirs = map[string]Kind{
	"people":   KindPerson,
	"channels": KindChannel,
	"facts":    KindFact,
	"daily":    KindDaily,
	"handoff":  KindHandoff,
}

// Note is one memory file. Frontmatter fields are set for person, channel and
// fact notes; for the index, daily logs and handoffs only Path, Kind,
// Modified and Body are.
type Note struct {
	Path     string    // slash path inside memory/, e.g. people/ana.md
	Kind     Kind      // from the folder
	Source   string    // who or what it came from
	Trust    Trust     // owner, other, web
	Private  bool      // only for the owner's asks
	Modified time.Time // set on every Write
	Body     string
}

// NoteInfo is a row in the panel's memory list.
type NoteInfo struct {
	Path     string
	Kind     Kind
	Trust    Trust
	Private  bool
	Modified time.Time
	Size     int
	Summary  string // first non-empty body line
}

// kindOf validates a memory path and returns its kind.
func kindOf(rel string) (Kind, error) {
	if rel == "" || strings.Contains(rel, `\`) || path.IsAbs(rel) || path.Clean(rel) != rel {
		return "", fmt.Errorf("bad memory path %q", rel)
	}
	if rel == indexFile {
		return KindIndex, nil
	}
	dir, name := path.Split(rel)
	k, ok := kindDirs[strings.TrimSuffix(dir, "/")]
	if !ok || !validName(name) {
		return "", fmt.Errorf("bad memory path %q", rel)
	}
	return k, nil
}

func validName(name string) bool {
	if !strings.HasSuffix(name, ".md") || len(name) < 4 || len(name) > 104 || name[0] == '.' {
		return false
	}
	for _, r := range strings.TrimSuffix(name, ".md") {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func isNoteKind(k Kind) bool { return k == KindPerson || k == KindChannel || k == KindFact }

// Slug turns a name into a note file name stem: lowercase letters, digits,
// dashes.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-.")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// PersonPath is the note path for a person, or "" when the name has no
// usable characters.
func PersonPath(name string) string {
	if s := Slug(name); s != "" {
		return "people/" + s + ".md"
	}
	return ""
}

func parseNote(rel string, k Kind, content string) Note {
	n := Note{Path: rel, Kind: k, Body: content}
	if !isNoteKind(k) || !strings.HasPrefix(content, "---\n") {
		return n
	}
	end := strings.Index(content[4:], "\n---\n")
	if end < 0 {
		return n
	}
	front := content[4 : 4+end]
	n.Body = strings.TrimPrefix(content[4+end+5:], "\n")
	for _, line := range strings.Split(front, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "source":
			n.Source = v
		case "trust":
			n.Trust = Trust(v)
		case "private":
			n.Private = v == "true"
		case "modified":
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				n.Modified = t
			} else if t, err := time.Parse("2006-01-02", v); err == nil {
				n.Modified = t
			}
		}
	}
	return n
}

func renderNote(n Note) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "type: %s\n", n.Kind)
	fmt.Fprintf(&b, "source: %s\n", oneLine(n.Source, 120))
	fmt.Fprintf(&b, "trust: %s\n", n.Trust)
	if n.Private {
		b.WriteString("private: true\n")
	}
	fmt.Fprintf(&b, "modified: %s\n", n.Modified.Format(time.RFC3339))
	b.WriteString("---\n")
	body := strings.TrimSpace(strings.ReplaceAll(n.Body, "\r\n", "\n"))
	b.WriteString(body)
	b.WriteString("\n")
	return b.String()
}

// Read returns one memory file.
func (s *Store) Read(rel string) (Note, error) {
	k, err := kindOf(rel)
	if err != nil {
		return Note{}, err
	}
	p := filepath.Join(s.dir, filepath.FromSlash(rel))
	b, err := os.ReadFile(p)
	if err != nil {
		return Note{}, fmt.Errorf("reading memory %s: %w", rel, err)
	}
	n := parseNote(rel, k, string(b))
	if n.Modified.IsZero() {
		if fi, err := os.Stat(p); err == nil {
			n.Modified = fi.ModTime()
		}
	}
	return n, nil
}

// Write creates or replaces a person, channel or fact note, or the index
// (path MEMORY.md, Body only). Over a cap it fails with ErrMergeFirst.
func (s *Store) Write(n Note) error {
	k, err := kindOf(n.Path)
	if err != nil {
		return err
	}
	if k == KindIndex {
		return s.WriteIndex(n.Body)
	}
	if !isNoteKind(k) {
		return fmt.Errorf("%s is written by tc, not edited", n.Path)
	}
	if !utf8.ValidString(n.Body) {
		return errors.New("note is not valid UTF-8")
	}
	if n.Trust == "" {
		n.Trust = TrustOwner
	}
	if !n.Trust.Valid() {
		return fmt.Errorf("bad trust %q", n.Trust)
	}
	n.Kind = k
	n.Modified = s.now()
	out := renderNote(n)
	if len(out) > NoteMaxBytes {
		return fmt.Errorf("%s is %d bytes, cap %d: %w", n.Path, len(out), NoteMaxBytes, ErrMergeFirst)
	}
	return s.withLock(func() error { return s.writeRel(n.Path, out) })
}

// Index returns MEMORY.md.
func (s *Store) Index() (string, error) { return s.readRel(indexFile) }

// WriteIndex replaces MEMORY.md. Over IndexMaxLines it fails with ErrMergeFirst.
func (s *Store) WriteIndex(text string) error {
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n") + "\n"
	if n := strings.Count(text, "\n"); n > IndexMaxLines {
		return fmt.Errorf("index is %d lines, cap %d: %w", n, IndexMaxLines, ErrMergeFirst)
	}
	if !utf8.ValidString(text) {
		return errors.New("index is not valid UTF-8")
	}
	return s.withLock(func() error { return s.writeRel(indexFile, text) })
}

// Forget deletes a memory file and drops index lines that point at it.
// The index itself cannot be forgotten.
func (s *Store) Forget(rel string) error {
	k, err := kindOf(rel)
	if err != nil {
		return err
	}
	if k == KindIndex {
		return errors.New("the index cannot be forgotten; edit it instead")
	}
	return s.withLock(func() error {
		if err := os.Remove(filepath.Join(s.dir, filepath.FromSlash(rel))); err != nil {
			return fmt.Errorf("forgetting %s: %w", rel, err)
		}
		idx, err := s.readRel(indexFile)
		if err != nil {
			return err
		}
		var keep []string
		for _, line := range strings.Split(idx, "\n") {
			if !strings.Contains(line, "("+rel+")") {
				keep = append(keep, line)
			}
		}
		if out := strings.Join(keep, "\n"); out != idx {
			return s.writeRel(indexFile, out)
		}
		return nil
	})
}

// List returns every memory file, index first, then notes by path, then
// daily logs newest first, then handoffs.
func (s *Store) List() ([]NoteInfo, error) {
	var out []NoteInfo
	err := filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, s.dir+string(filepath.Separator)))
		if d.IsDir() {
			if p != s.dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		k, err := kindOf(rel)
		if err != nil {
			return nil
		}
		n, err := s.Read(rel)
		if err != nil {
			return err
		}
		fi, _ := d.Info()
		size := 0
		if fi != nil {
			size = int(fi.Size())
		}
		out = append(out, NoteInfo{Path: rel, Kind: k, Trust: n.Trust, Private: n.Private,
			Modified: n.Modified, Size: size, Summary: summary(n.Body)})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing memory: %w", err)
	}
	order := map[Kind]int{KindIndex: 0, KindPerson: 1, KindChannel: 1, KindFact: 1, KindDaily: 2, KindHandoff: 3}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if order[a.Kind] != order[b.Kind] {
			return order[a.Kind] < order[b.Kind]
		}
		if a.Kind == KindDaily {
			return a.Path > b.Path
		}
		return a.Path < b.Path
	})
	return out, nil
}

func summary(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "#-* "))
		if line != "" {
			return oneLine(line, 100)
		}
	}
	return ""
}

// oneLine flattens s to a single line without control characters and cuts
// it to at most max bytes on a rune boundary.
func oneLine(s string, max int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == utf8.RuneError {
			continue
		}
		b.WriteRune(r)
		space = false
	}
	return cutBytes(strings.TrimSpace(b.String()), max)
}

func cutBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
