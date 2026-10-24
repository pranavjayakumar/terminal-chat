package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/skailama/tc/internal/agent/home"
)

// Files in the agent home's state/ folder. Only the supervisor writes them;
// `tc agent status|logs` and the TUI panel read them.
const (
	SessionsFile = "sessions.json"  // session key → engine session
	LedgerFile   = "ledger.jsonl"   // one line per run: what it cost
	ActivityFile = "activity.jsonl" // audit log
	QueueFile    = "queue.json"     // asks already handled, first start
	StatusFile   = "status.json"    // what the supervisor is doing now
	SocketFile   = "tc.sock"        // the MCP bridge
)

// ---- sessions ----

// Session is the engine session that holds one thread's conversation.
type Session struct {
	ID      string `json:"id"`
	Tokens  int    `json:"tokens"`   // context in use after the last turn
	Window  int    `json:"window"`   // context window size
	Turns   int    `json:"turns"`    // asks answered in it
	LastMsg uint64 `json:"last_msg"` // newest message the session has seen
	Updated int64  `json:"updated"`
}

type sessions struct {
	path string
	mu   sync.Mutex
	m    map[string]Session
}

func openSessions(dir string) (*sessions, error) {
	s := &sessions{path: filepath.Join(dir, SessionsFile), m: map[string]Session{}}
	if err := readJSON(s.path, &s.m); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *sessions) get(key string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return v, ok
}

func (s *sessions) put(key string, v Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = v
	return writeJSON(s.path, s.m)
}

func (s *sessions) drop(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return writeJSON(s.path, s.m)
}

// ---- ledger ----

// Spend is one run in the ledger.
type Spend struct {
	T     int64   `json:"t"` // unix ms
	Asker uint64  `json:"asker,omitempty"`
	Key   string  `json:"key,omitempty"`
	USD   float64 `json:"usd"`
	Kind  string  `json:"kind"` // ask, rollover, greet, dream
}

// Ledger is the spend log. Days are local days.
type Ledger struct {
	path string
	mu   sync.Mutex
	rows []Spend // the last two days are enough to answer any question
}

// OpenLedger loads the ledger of a state folder.
func OpenLedger(stateDir string) (*Ledger, error) {
	l := &Ledger{path: filepath.Join(stateDir, LedgerFile)}
	cut := time.Now().Add(-48 * time.Hour).UnixMilli()
	err := readLines(l.path, func(b []byte) {
		var s Spend
		if json.Unmarshal(b, &s) == nil && s.T >= cut {
			l.rows = append(l.rows, s)
		}
	})
	return l, err
}

// Add records a run.
func (l *Ledger) Add(s Spend) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, s)
	return appendLine(l.path, s)
}

func sameDay(ms int64, now time.Time) bool {
	y1, m1, d1 := time.UnixMilli(ms).In(now.Location()).Date()
	y2, m2, d2 := now.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// Spent is what today's runs cost.
func (l *Ledger) Spent(now time.Time) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var t float64
	for _, s := range l.rows {
		if sameDay(s.T, now) {
			t += s.USD
		}
	}
	return t
}

// Asks is how many asks user made today.
func (l *Ledger) Asks(user uint64, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.rows {
		if s.Kind == "ask" && s.Asker == user && sameDay(s.T, now) {
			n++
		}
	}
	return n
}

// ---- activity ----

// Activity is one line of the audit log.
type Activity struct {
	T      int64   `json:"t"`
	Who    string  `json:"who,omitempty"` // asker's name
	Where  string  `json:"where,omitempty"`
	Chan   uint64  `json:"chan,omitempty"`
	Thread uint64  `json:"thread,omitempty"`
	Mode   string  `json:"mode,omitempty"`
	What   string  `json:"what"` // ask, reply, refused, approval, issue, pr, error, rollover, greet, dream
	Detail string  `json:"detail,omitempty"`
	USD    float64 `json:"usd,omitempty"`
}

// ReadActivity returns the last n lines of the audit log, oldest first.
func ReadActivity(stateDir string, n int) ([]Activity, error) {
	var out []Activity
	err := readLines(filepath.Join(stateDir, ActivityFile), func(b []byte) {
		var a Activity
		if json.Unmarshal(b, &a) == nil {
			out = append(out, a)
			if len(out) > 4*n+64 {
				out = append(out[:0:0], out[len(out)-n:]...)
			}
		}
	})
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, err
}

// ---- queue ----

type queueState struct {
	Since     int64            `json:"since"`   // first start: older asks are not ours to answer
	Handled   map[uint64]int64 `json:"handled"` // ask message id → when
	Greeted   bool             `json:"greeted"`
	LastDream string           `json:"last_dream,omitempty"` // YYYY-MM-DD
}

type queue struct {
	path string
	mu   sync.Mutex
	st   queueState
}

func openQueue(stateDir string, now time.Time) (*queue, error) {
	q := &queue{path: filepath.Join(stateDir, QueueFile)}
	if err := readJSON(q.path, &q.st); err != nil {
		return nil, err
	}
	if q.st.Handled == nil {
		q.st.Handled = map[uint64]int64{}
	}
	if q.st.Since == 0 {
		q.st.Since = now.UnixMilli()
		if err := writeJSON(q.path, q.st); err != nil {
			return nil, err
		}
	}
	return q, nil
}

// claim marks an ask handled; false if it already was.
func (q *queue) claim(id uint64, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.st.Handled[id]; ok {
		return false
	}
	q.st.Handled[id] = now.UnixMilli()
	cut := now.Add(-72 * time.Hour).UnixMilli()
	for k, t := range q.st.Handled {
		if t < cut {
			delete(q.st.Handled, k)
		}
	}
	_ = writeJSON(q.path, q.st)
	return true
}

func (q *queue) since() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.st.Since
}

func (q *queue) update(f func(*queueState)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	f(&q.st)
	_ = writeJSON(q.path, q.st)
}

func (q *queue) get() queueState {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.st
}

// ---- status ----

// Status is state/status.json: the supervisor's own view, for
// `tc agent status` and the panel.
type Status struct {
	PID      int     `json:"pid"`
	Version  string  `json:"version,omitempty"`
	Started  int64   `json:"started"`
	State    string  `json:"state"` // starting, awake, working, idle, paused, offline, stopped
	Since    int64   `json:"since"`
	Runs     int     `json:"runs"` // in progress
	SpentUSD float64 `json:"spent_usd"`
	CapUSD   float64 `json:"cap_usd"`
	LastRun  int64   `json:"last_run,omitempty"`
	Err      string  `json:"err,omitempty"`
}

// ReadStatus reads state/status.json.
func ReadStatus(stateDir string) (Status, error) {
	var s Status
	b, err := os.ReadFile(filepath.Join(stateDir, StatusFile))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// ---- files ----

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("parsing %s: %w", filepath.Base(path), err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return home.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

func appendLine(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return errors.Join(err, f.Close())
}

func readLines(path string, each func([]byte)) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		each(sc.Bytes())
	}
	return sc.Err()
}
