package background

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// maxLog caps the log: past it, the log moves to background.log.1 (one old
// file kept), so a long-running notifier on a flaky network can't fill the
// disk.
const maxLog = 1 << 20

// LogWriter appends to TC_HOME/background.log, rotating at maxLog.
type LogWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

// OpenLog opens home's log.
func OpenLog(home string) (*LogWriter, error) {
	w := &LogWriter{path: filepath.Join(home, LogFile)}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *LogWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, fi.Size()
	return nil
}

func (w *LogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return len(p), nil
	}
	if w.size+int64(len(p)) > maxLog {
		w.f.Close()
		w.f = nil
		_ = os.Rename(w.path, w.path+".1")
		if err := w.open(); err != nil {
			return len(p), nil // logging is best effort
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the log.
func (w *LogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
