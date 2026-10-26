package tools

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/skailama/tc/internal/agent/home"
)

// JobsFile is the scheduler's file in the agent's state folder.
const JobsFile = "schedules.json"

// Job kinds.
const (
	KindSchedule = "schedule" // the owner's: post text in a channel
	KindRemind   = "remind"   // a reminder for one person
)

// Caps: the scheduler is a convenience, not a bulk poster.
const (
	MaxJobs        = 50
	MaxPerAsker    = 3 // pending reminders someone other than the owner may have
	MaxJobText     = 2000
	LateLimitHours = 6 // a job missed by more than this (machine asleep) is skipped
)

// Job is one scheduled post.
type Job struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Chan      uint64 `json:"chan"`
	Thread    uint64 `json:"thread,omitempty"`
	Where     string `json:"where"` // the channel's label when it was made
	Text      string `json:"text"`
	At        int64  `json:"at"` // next time it fires, unix ms
	Repeat    Repeat `json:"repeat"`
	Asker     uint64 `json:"asker"`
	AskerName string `json:"asker_name"`
	Person    string `json:"person,omitempty"` // remind: who it's for (a user name)
	Created   int64  `json:"created"`
}

// NewID makes a short id people can type ("s4kq7").
func NewID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return "s" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

func jobsPath(stateDir string) string { return filepath.Join(stateDir, JobsFile) }

// LoadJobs reads the jobs, soonest first.
func LoadJobs(stateDir string) ([]Job, error) {
	var jobs []Job
	b, err := os.ReadFile(jobsPath(stateDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading schedules: %w", err)
	}
	if err := json.Unmarshal(b, &jobs); err != nil {
		return nil, fmt.Errorf("parsing schedules: %w", err)
	}
	slices.SortStableFunc(jobs, func(a, b Job) int { return int(a.At - b.At) })
	return jobs, nil
}

// UpdateJobs changes the jobs under a file lock, so the supervisor firing a
// job and the panel cancelling one never undo each other.
func UpdateJobs(stateDir string, f func([]Job) ([]Job, error)) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(jobsPath(stateDir)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("locking schedules: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking schedules: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	jobs, err := LoadJobs(stateDir)
	if err != nil {
		return err
	}
	jobs, err = f(jobs)
	if err != nil {
		return err
	}
	if jobs == nil {
		jobs = []Job{}
	}
	b, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	return home.WriteFileAtomic(jobsPath(stateDir), append(b, '\n'), 0o600)
}

// CancelJob removes job id; false when there is none.
func CancelJob(stateDir, id string) (Job, bool, error) {
	var gone Job
	var found bool
	err := UpdateJobs(stateDir, func(jobs []Job) ([]Job, error) {
		i := slices.IndexFunc(jobs, func(j Job) bool { return j.ID == id })
		if i < 0 {
			return jobs, nil
		}
		gone, found = jobs[i], true
		return slices.Delete(jobs, i, i+1), nil
	})
	return gone, found, err
}
