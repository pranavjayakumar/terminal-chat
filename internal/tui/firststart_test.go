package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
)

func newModelWith(t *testing.T, f *fakeClient) *Model {
	t.Helper()
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

func TestFirstStartOpensGeneral(t *testing.T) {
	m := newTestModel(t, 100, 30)
	if m.active != 10 {
		t.Fatalf("active = %d, want #general (10)", m.active)
	}
}

func TestFirstStartSnapshotArrivesLate(t *testing.T) {
	f := newFake()
	all := f.chans
	f.chans = nil
	m := newModelWith(t, f)
	if m.active != 0 {
		t.Fatalf("active = %d before snapshot", m.active)
	}
	checkView(t, m, "empty")
	f.chans = all
	m.Update(updateMsg(client.Update{Reset: true}))
	if m.active != 10 {
		t.Fatalf("after snapshot active = %d, want 10", m.active)
	}
	checkView(t, m, "late snapshot")
}

func TestFirstStartLateChannelsNoGeneral(t *testing.T) {
	f := newFake()
	all := f.chans
	f.chans = nil
	m := newModelWith(t, f)
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

func TestFirstStartNoChannels(t *testing.T) {
	f := newFake()
	f.chans = nil
	m := newModelWith(t, f)
	m.Update(updateMsg(client.Update{Reset: true}))
	m.Update(updateMsg(client.Update{Channels: true}))
	if m.active != 0 {
		t.Fatalf("active = %d, want 0", m.active)
	}
	checkView(t, m, "no channels")
}

func TestRememberedChannelWins(t *testing.T) {
	m := newTestModel(t, 100, 30)
	m.openChannel(11)
	m.Update(updateMsg(client.Update{Reset: true}))
	m.Update(updateMsg(client.Update{Channels: true}))
	if m.active != 11 {
		t.Fatalf("active = %d, want 11 kept", m.active)
	}
}
