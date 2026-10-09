package tui

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/skailama/tc/internal/proto"
)

func (f *fakeClient) Groups() []proto.Group {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.groups)
}

// SetGroup mimics the group.update event the real server would send.
func (f *fakeClient) SetGroup(ctx context.Context, name string, members []uint64) error {
	f.record(fmt.Sprintf("group:%s:%v", name, members))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = slices.DeleteFunc(f.groups, func(g proto.Group) bool { return g.Name == name })
	if len(members) > 0 {
		f.groups = append(f.groups, proto.Group{Name: name, Members: members})
		slices.SortFunc(f.groups, func(a, b proto.Group) int { return compareStr(a.Name, b.Name) })
	}
	return nil
}

func compareStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (f *fakeClient) Remind(ctx context.Context, at time.Time, text string, ch uint64) (uint64, error) {
	f.record("remind")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.remindErr != nil {
		return 0, f.remindErr
	}
	f.nextRemID++
	f.reminded = append(f.reminded, remindCall{at, text, ch})
	f.reminders = append(f.reminders, proto.Reminder{ID: f.nextRemID, At: at.UnixMilli(), Text: text, Chan: ch})
	return f.nextRemID, nil
}

func (f *fakeClient) Reminders(ctx context.Context) ([]proto.Reminder, error) {
	f.record("reminders")
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reminders), f.remindErr
}

func (f *fakeClient) DeleteReminder(ctx context.Context, id uint64) error {
	f.record(fmt.Sprintf("remind-delete:%d", id))
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.reminders)
	f.reminders = slices.DeleteFunc(f.reminders, func(r proto.Reminder) bool { return r.ID == id })
	if len(f.reminders) == n {
		return proto.Errf(proto.ErrNotFound, "")
	}
	return nil
}
