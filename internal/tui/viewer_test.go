package tui

import (
	"strings"
	"testing"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// A long code line is cut with "…" in the log but readable in the viewer by
// scrolling sideways.
func TestViewerScrollsSideways(t *testing.T) {
	m, _ := newTestModelFake(t, 40, 12)
	long := "```\nstart-" + strings.Repeat("x", 60) + "-TAILMARK\n```"
	m.openViewer(client.Message{Message: proto.Message{ID: 1, Author: 1, Body: long}})
	if m.mode != modeViewer {
		t.Fatal("viewer didn't open")
	}
	plain := checkView(t, m, "viewer")
	if strings.Contains(plain, "TAILMARK") || !strings.Contains(plain, "start-") {
		t.Fatalf("expected the start of the line only:\n%s", plain)
	}
	for range 10 {
		m.viewerKey("right")
	}
	plain = checkView(t, m, "viewer scrolled")
	if !strings.Contains(plain, "TAILMARK") {
		t.Fatalf("scrolling right didn't reveal the end:\n%s", plain)
	}
	for range 50 {
		m.viewerKey("right") // clamped: never scrolls past the widest line
	}
	if m.view.x > 70 {
		t.Fatalf("x not clamped: %d", m.view.x)
	}
	m.viewerKey(kEsc)
	if m.mode != modeSelect {
		t.Fatal("Esc should return to select mode")
	}
}

func TestViewerOpensFromSelectMode(t *testing.T) {
	m, _ := newTestModelFake(t, 80, 24)
	run(m, m.openChannel(10))
	m.enterSelect()
	run(m, m.handleKey(keyMsg("v")))
	if m.mode != modeViewer {
		t.Fatalf("v in select mode should open the viewer, mode=%v", m.mode)
	}
	checkView(t, m, "viewer from select")
}
