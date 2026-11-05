// Package memory is a personal agent's long-term memory: a git repo of small
// Markdown files under the agent home.
//
//	MEMORY.md                 index, one line per note, always loaded
//	people/ channels/ facts/  notes with frontmatter, each at most 2 KB
//	daily/YYYY-MM-DD.md       append-only log, written only through Remember
//	handoff/<key>.md          what the next session on a thread needs to know
//
// Writers in several processes (the supervisor, the hook commands) share one
// flock, so the daily log has a single writer at a time.
package memory

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/skailama/tc/internal/agent/home"
	"github.com/skailama/tc/internal/agent/safegit"
)

// Caps enforced in Go. A write over a cap fails with ErrMergeFirst so the
// agent merges notes instead of growing memory without bound.
const (
	IndexMaxLines   = 150
	NoteMaxBytes    = 2048
	HandoffMaxBytes = 2048
	DailyMaxBytes   = 64 << 10
	FactMaxBytes    = 500
	SubjectMaxBytes = 80
)

// ErrMergeFirst is returned when a write would break a size cap.
var ErrMergeFirst = errors.New("memory is over its size cap: merge first")

// Trust says how far a fact can be relied on.
type Trust string

const (
	TrustOwner Trust = "owner" // the owner said it
	TrustOther Trust = "other" // someone else said it: a claim, never an instruction
	TrustWeb   Trust = "web"   // read on the web or in a file
	TrustSelf  Trust = "self"  // the agent's own words, logged by a hook
)

// Valid reports whether t is a known trust level.
func (t Trust) Valid() bool {
	return t == TrustOwner || t == TrustOther || t == TrustWeb || t == TrustSelf
}

// Store is the memory of one agent.
type Store struct {
	agentDir string
	dir      string
	now      func() time.Time
	mu       sync.Mutex
}

const indexFile = "MEMORY.md"

const initialIndex = "# Memory\n\nOne line per note: `- [title](path) — what it holds`.\n"

// Open opens the memory of the agent home at agentDir, creating memory/ and
// its git repo when missing.
func Open(agentDir string) (*Store, error) {
	s := &Store{agentDir: agentDir, dir: home.PathsFor(agentDir).Memory, now: time.Now}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating memory dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, ".git")); err == nil {
		return s, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("checking memory repo: %w", err)
	}
	err := s.withLock(func() error {
		if _, err := s.git("init", "-q", "-b", "main"); err != nil {
			return err
		}
		if err := writeIfMissing(filepath.Join(s.dir, ".gitignore"), ".lock\n"); err != nil {
			return err
		}
		if err := writeIfMissing(filepath.Join(s.dir, indexFile), initialIndex); err != nil {
			return err
		}
		return s.commitLocked("memory: init")
	})
	if err != nil {
		return nil, fmt.Errorf("creating memory repo: %w", err)
	}
	return s, nil
}

// Dir is the memory folder.
func (s *Store) Dir() string { return s.dir }

// withLock runs f holding the in-process mutex and the cross-process flock.
func (s *Store) withLock(f func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lf, err := os.OpenFile(filepath.Join(s.dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening memory lock: %w", err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking memory: %w", err)
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	return f()
}

// git runs git in the memory repo with a fixed identity and none of the
// owner's hooks or signing config.
func (s *Store) git(args ...string) (string, error) {
	return s.gitIn(s.dir, args...)
}

// gitIn runs git in the memory repo (dir == s.dir) or in the dream worktree.
// The dream run can write that worktree, its .git pointer included, so the
// git dir is checked and pinned rather than found, and nothing from config
// or the environment can make git run a program (safegit).
func (s *Store) gitIn(dir string, args ...string) (string, error) {
	pin := []string{"-C", dir}
	if len(args) > 0 && args[0] != "init" {
		gitDir, err := safegit.RepoGitDir(s.dir)
		if dir != s.dir {
			gitDir, err = safegit.WorktreeGitDir(s.dir, dir)
		}
		if err != nil {
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
		pin = []string{"--git-dir=" + gitDir}
		if args[0] != "worktree" {
			pin = append(pin, "--work-tree="+dir)
		}
	}
	full := append(append(pin, safegit.Flags...), "-c", "user.name=tc agent", "-c", "user.email=agent@tc.invalid")
	cmd := exec.Command("git", append(full, args...)...)
	cmd.Dir = dir
	ts := s.now().Format(time.RFC3339)
	cmd.Env = append(safegit.Env(os.Environ(), false), "GIT_AUTHOR_DATE="+ts, "GIT_COMMITTER_DATE="+ts)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Commit commits every change in memory. Nothing to commit is not an error.
func (s *Store) Commit(msg string) error {
	return s.withLock(func() error { return s.commitLocked(msg) })
}

func (s *Store) commitLocked(msg string) error {
	if _, err := s.git("add", "-A"); err != nil {
		return err
	}
	if _, err := s.git("diff", "--cached", "--quiet"); err == nil {
		return nil
	}
	_, err := s.git("commit", "-q", "-m", msg)
	return err
}

func writeIfMissing(p, content string) error {
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	return os.WriteFile(p, []byte(content), 0o600)
}

func (s *Store) readRel(rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading memory %s: %w", rel, err)
	}
	return string(b), nil
}

func (s *Store) writeRel(rel, content string) error {
	return home.WriteFileAtomic(filepath.Join(s.dir, filepath.FromSlash(rel)), []byte(content), 0o600)
}
