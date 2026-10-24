package agent

import (
	"sync"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// live publishes a run's status under its thread, at most once per
// interval: the latest state wins and the last one is always sent.
type live struct {
	chat   Chat
	ch, th uint64
	every  time.Duration
	quiet  bool // rollover and dream turns show nothing

	mu      sync.Mutex
	state   string
	status  string
	preview string
	dirty   bool
	last    time.Time
	timer   *time.Timer
	closed  bool
}

func newLive(c Chat, ch, thread uint64, every time.Duration, quiet bool) *live {
	return &live{chat: c, ch: ch, th: thread, every: every, quiet: quiet}
}

// set updates the state; "" keeps the status line or preview (a working
// status clears the preview only when one is given).
func (l *live) set(state, status, preview string) {
	if l.quiet {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.state = state
	if status != "" {
		l.status = status
	}
	if preview != "" || state == proto.AgentThinking {
		l.preview = preview
	}
	l.dirty = true
	final := state == proto.AgentDone || state == proto.AgentError
	wait := l.every - time.Since(l.last)
	if final && wait > 0 {
		// The server drops statuses over its rate, and a lost final one
		// would leave the thread showing work that has ended.
		time.Sleep(wait)
		wait = 0
	}
	if wait <= 0 {
		l.flushLocked()
		return
	}
	if l.timer == nil {
		l.timer = time.AfterFunc(wait, func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.timer = nil
			if !l.closed {
				l.flushLocked()
			}
		})
	}
}

// setStatus sets the status line (the status tool); the preview stays.
func (l *live) setStatus(status string) {
	l.mu.Lock()
	st := l.state
	l.mu.Unlock()
	if st == "" || st == proto.AgentThinking {
		st = proto.AgentWorking
	}
	l.set(st, status, "")
}

func (l *live) setPreview(text string) {
	l.set(proto.AgentStreaming, "", text)
}

func (l *live) flushLocked() {
	if !l.dirty {
		return
	}
	l.dirty = false
	l.last = time.Now()
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	preview := l.preview
	if l.state == proto.AgentDone || l.state == proto.AgentError {
		preview = "" // the real reply is posted now
	}
	_ = l.chat.SendAgentStatus(l.ch, l.th, l.state, short(l.status, proto.MaxStatus-4), preview)
}

func (l *live) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
	l.closed = true
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
}
