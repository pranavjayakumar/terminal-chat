package agent

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/skailama/tc/internal/proto"
)

// IconPresets are tc's animated agent icons (internal/tui/agent.go), by
// name and resting glyph. An Icon is "<preset>" or "<preset>:<0-7>" (a
// theme name color), or a single emoji, shown still.
var IconPresets = []struct{ Name, Glyph string }{
	{"pebble", "▟▙"}, {"orb", "◉"}, {"spark", "✦"}, {"owl", "◕◕"},
	{"hive", "⣿"}, {"moon", "☾"}, {"meter", "▃▅"}, {"kite", "◆"},
}

// ValidIcon reports whether icon is a preset (with an optional color
// 0–7) or one emoji.
func ValidIcon(icon string) bool {
	if icon == "" || len(icon) > proto.MaxAgentIcon {
		return false
	}
	name, col, hasCol := strings.Cut(icon, ":")
	for _, p := range IconPresets {
		if name == p.Name {
			return !hasCol || len(col) == 1 && col[0] >= '0' && col[0] <= '7'
		}
	}
	if hasCol || utf8.RuneCountInString(icon) > 8 {
		return false
	}
	for _, r := range icon {
		if r < 0x80 || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// IconHelp names the presets for a tool description or an error.
func IconHelp() string {
	var b []string
	for _, p := range IconPresets {
		b = append(b, p.Name+" "+p.Glyph)
	}
	return strings.Join(b, ", ")
}
