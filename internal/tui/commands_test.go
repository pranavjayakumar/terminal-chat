package tui

import (
	"strings"
	"testing"
)

// Every row of the table the menu and /help are built from is a command
// runCommand knows, so neither can offer one that doesn't exist.
func TestCommandTableMatchesRunCommand(t *testing.T) {
	for _, c := range commandTable {
		if c.name == "" {
			continue
		}
		m := newTestModel(t, 80, 24)
		_, err := m.runCommand("/" + c.name)
		if err != nil && strings.HasPrefix(err.Error(), "unknown command") {
			t.Errorf("/%s is in commandTable but runCommand doesn't know it", c.name)
		}
		if !isCommandName(c.name) {
			t.Errorf("isCommandName(%q) = false", c.name)
		}
	}
	m := newTestModel(t, 80, 24)
	if _, err := m.runCommand("/nosuchcommand"); err == nil || !strings.HasPrefix(err.Error(), "unknown command") {
		t.Fatalf("unknown command not reported: %v", err)
	}
}
