package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
)

// freshModel is a client whose cache is empty and whose connection is still
// syncing, as on a first login.
func freshModel(t *testing.T) (*Model, *fakeClient, []client.ChannelView) {
	t.Helper()
	f := newFake()
	all := f.chans
	f.chans = nil
	f.status.State = client.Syncing
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if m.active != 0 {
		t.Fatalf("active = %d on an empty cache", m.active)
	}
	return m, f, all
}

func checkSizes(t *testing.T, m *Model, step string) {
	t.Helper()
	for _, sz := range [][2]int{{100, 30}, {40, 12}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkView(t, m, step)
	}
}

func TestFirstStartSnapshotArrivesLate(t *testing.T) {
	m, f, all := freshModel(t)
	// Server order: Reset (users/read/groups), then Channels, then messages.
	m.Update(updateMsg(client.Update{Reset: true}))
	if m.active != 0 {
		t.Fatalf("active = %d after Reset with no channels", m.active)
	}
	f.chans = all
	m.Update(updateMsg(client.Update{Channels: true}))
	if m.active != 10 {
		t.Fatalf("active = %d, want #general (10)", m.active)
	}
	if len(f.reads) != 0 {
		t.Fatalf("marked read while syncing: %v", f.reads)
	}
	for _, v := range m.chans {
		if v.ID == 10 && v.Unread != 2 {
			t.Fatalf("#general unread cleared while syncing: %d", v.Unread)
		}
		if v.ID == 13 && (v.Unread != 1 || !v.Mention) {
			t.Fatalf("#secret unread/mention changed: %+v", v)
		}
	}
	checkSizes(t, m, "syncing")

	f.status.State = client.Online
	f.chans[1].Last = 5
	m.Update(updateMsg(client.Update{Channels: true}))
	m.Update(updateMsg(client.Update{Status: true}))
	if f.reads[10] < 5 {
		t.Fatalf("reads = %v, want #general read at Online", f.reads)
	}
	checkSizes(t, m, "online")
}

func TestFirstStartNoGeneralPicksFirstJoined(t *testing.T) {
	m, f, all := freshModel(t)
	for _, v := range all {
		if v.Name != "general" {
			f.chans = append(f.chans, v)
		}
	}
	m.Update(updateMsg(client.Update{Channels: true}))
	v, ok := m.chanView(m.active)
	if !ok || !v.Member || v.Name == "general" {
		t.Fatalf("active = %d, want a joined non-general channel", m.active)
	}
}

func TestFirstStartNoChannelsThenAdded(t *testing.T) {
	m, f, all := freshModel(t)
	m.Update(updateMsg(client.Update{Reset: true}))
	m.Update(updateMsg(client.Update{Channels: true}))
	if m.active != 0 {
		t.Fatalf("active = %d, want 0", m.active)
	}
	checkSizes(t, m, "no channels")
	f.chans = all
	m.Update(updateMsg(client.Update{Channels: true}))
	if m.active != 10 {
		t.Fatalf("after being added active = %d, want 10", m.active)
	}
}

func TestOpenChannelNotReplaced(t *testing.T) {
	m, _, all := freshModel(t)
	m.c.(*fakeClient).chans = all
	m.Update(updateMsg(client.Update{Channels: true}))
	m.openChannel(11)
	m.Update(updateMsg(client.Update{Reset: true}))
	m.Update(updateMsg(client.Update{Channels: true}))
	if m.active != 11 {
		t.Fatalf("active = %d, want 11 kept", m.active)
	}
}

func TestEmptiedSidebarNotYanked(t *testing.T) {
	m, f, all := freshModel(t)
	f.chans = all
	m.Update(updateMsg(client.Update{Channels: true}))
	// The user is removed from everything, then gets a new DM later.
	var rest []client.ChannelView
	for _, v := range all {
		v.Member = false
		rest = append(rest, v)
	}
	f.chans = rest
	m.Update(updateMsg(client.Update{Channels: true}))
	m.active = 0
	f.chans = all
	m.Update(updateMsg(client.Update{Channels: true}))
	if m.active != 0 {
		t.Fatalf("active = %d, want 0: auto-open must happen once", m.active)
	}
}
