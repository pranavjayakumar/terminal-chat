package background

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
)

// StatusFile is where the running notifier reports how it's doing, for
// `tc background status`. It never holds message text.
const StatusFile = "background.json"

// LogFile is the notifier's log (state changes and errors, never text).
const LogFile = "background.log"

// Status is the notifier's last report.
type Status struct {
	PID       int    `json:"pid"`
	Version   string `json:"version"`
	Started   int64  `json:"started"` // unix ms
	State     string `json:"state"`   // offline, connecting, syncing, online, stopped, waiting
	Transport string `json:"transport,omitempty"`
	Since     int64  `json:"since"` // unix ms of the last state change
	Err       string `json:"err,omitempty"`
	ErrAt     int64  `json:"err_at,omitempty"`
	Passive   bool   `json:"passive"` // the server honors cap passive (else we look online)
	Notified  int    `json:"notified"`
	LastNote  int64  `json:"last_note,omitempty"`
}

// ReadStatus reads home's status file.
func ReadStatus(home string) (Status, error) {
	var st Status
	f, err := os.OpenFile(filepath.Join(home, StatusFile), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return st, err
	}
	defer f.Close()
	err = json.NewDecoder(f).Decode(&st)
	return st, err
}

// writeStatus replaces home's status file atomically.
func writeStatus(home string, st Status) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(home, ".background.json.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(home, StatusFile))
}
