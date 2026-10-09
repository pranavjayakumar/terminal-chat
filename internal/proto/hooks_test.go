package proto

import (
	"strings"
	"testing"
)

func TestBotName(t *testing.T) {
	for in, want := range map[string]string{
		"ci":                             "ci-bot",
		"deploy-bot":                     "deploy-bot",
		"x":                              "x-bot",
		strings.Repeat("a", 28):          strings.Repeat("a", 28) + "-bot",
		strings.Repeat("a", 28) + "-bot": strings.Repeat("a", 28) + "-bot",
	} {
		if got, ok := BotName(in); !ok || got != want {
			t.Errorf("BotName(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "Bad", "-x", strings.Repeat("a", 29), "has space"} {
		if got, ok := BotName(in); ok {
			t.Errorf("BotName(%q) = %q, want invalid", in, got)
		}
	}
}
