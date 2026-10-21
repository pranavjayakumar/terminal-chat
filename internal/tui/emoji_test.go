package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// ---- data ----

func TestEmojiTableShortcodesUnique(t *testing.T) {
	seen := map[string]string{}
	for _, d := range emojiTable {
		if d.names == "" {
			t.Fatalf("%s has no shortcode", d.e)
		}
		for n := range strings.FieldsSeq(d.names) {
			if prev, ok := seen[n]; ok {
				t.Errorf(":%s: is both %s and %s", n, prev, d.e)
			}
			if strings.ContainsFunc(n, func(r rune) bool { return !shortcodeChar(r) }) {
				t.Errorf(":%s: has a character a shortcode can't", n)
			}
			seen[n] = d.e
		}
		if int(d.cat) >= len(emojiCats) {
			t.Errorf("%s: category %d", d.e, d.cat)
		}
	}
	if len(emojiTable) < 1000 {
		t.Fatalf("only %d emoji", len(emojiTable))
	}
}

// Every emoji the grid can show is one wide cell, by the TUI's own measure,
// and survives sanitizing unchanged.
func TestEmojiTableWidthSafe(t *testing.T) {
	check := func(e string) {
		t.Helper()
		rs := []rune(e)
		if w := strWidth(e); w != 2 {
			t.Errorf("%q (%U) is %d cells", e, rs, w)
		}
		if sanitize.Line(e) != e {
			t.Errorf("%q changes when sanitized", e)
		}
		if len(rs) > 2 || len(rs) == 2 && rs[1] != 0xFE0F {
			t.Errorf("%q (%U) is a sequence", e, rs)
		}
		for _, r := range rs {
			if r == 0x200D || r >= 0x1F3FB && r <= 0x1F3FF || r == 0x20E3 || r >= 0x1F1E6 && r <= 0x1F1FF {
				t.Errorf("%q (%U): ZWJ, skin tone, keycap or flag", e, rs)
			}
		}
	}
	for _, d := range emojiTable {
		check(d.e)
	}
	for _, c := range emojiCats {
		check(c.icon)
	}
	check(recentIcon)
	for _, e := range freqSeed {
		check(e)
		if emojiDefOf(e) < 0 {
			t.Errorf("seed %s not in the table", e)
		}
	}
}

func TestResolveEmoji(t *testing.T) {
	ok := map[string]string{"+1": "👍", ":tada:": "🎉", "tada": "🎉", "🚀": "🚀", " 👀 ": "👀",
		":fire:": "🔥", "thumbsup": "👍", "check": "✅", ":HEART:": "❤️", "ok": "🆗"}
	for in, want := range ok {
		if got, good := resolveEmoji(in); !good || got != want {
			t.Errorf("%q -> %q %v", in, got, good)
		}
	}
	for _, in := range []string{"", "hello", ":nope:", "a b", "\x1b[1m", "👍 👍"} {
		if got, good := resolveEmoji(in); good {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

// ---- search and completion ranking ----

func TestSearchEmojiRanking(t *testing.T) {
	names := func(hs []emojiHit) []string {
		var out []string
		for _, h := range hs {
			out = append(out, h.name)
		}
		return out
	}
	// An exact name first, then frequently used, then shorter prefixes.
	hits := searchEmoji("fire", []string{"🚒"}, false, 0)
	if got := names(hits); len(got) < 3 || got[0] != "fire" || got[1] != "fire_engine" {
		t.Fatalf("fire: %v", got)
	}
	hits = searchEmoji("fi", []string{"🐟"}, false, 0)
	got := names(hits)
	if got[0] != "fish" {
		t.Fatalf("fi with 🐟 used: %v", got[:5])
	}
	firstSub := slices.IndexFunc(hits, func(h emojiHit) bool { return h.at > 0 })
	lastPrefix := -1
	for i, h := range hits {
		if h.at == 0 {
			lastPrefix = i
		}
	}
	if firstSub >= 0 && firstSub < lastPrefix {
		t.Fatalf("a substring match before a prefix match: %v", got)
	}
	// Keywords only when asked.
	if len(searchEmoji("hooray", nil, false, 0)) != 0 || len(searchEmoji("hooray", nil, true, 0)) == 0 {
		t.Fatal("keyword search")
	}
	if len(searchEmoji("fi", nil, false, 3)) != 3 {
		t.Fatal("limit")
	}
}

// ---- shortcodes on send ----

func TestShortcodesExpandOnSend(t *testing.T) {
	cases := map[string]string{
		"ship it :fire:":                  "ship it 🔥",
		":tada::rocket: done":             "🎉🚀 done",
		"(:+1:)":                          "(👍)",
		":nope: stays":                    ":nope: stays",
		"12:100:3 and http://x:100: keep": "12:100:3 and http://x:100: keep",
		"`:fire:` code":                   "`:fire:` code",
		"```\n:fire:\n```\n:fire:":        "```\n:fire:\n```\n🔥",
		"a:fire: b":                       "a:fire: b",
		":fire:s":                         ":fire:s",
		":FIRE:":                          "🔥",
		"https://e.com/search?q=:fire:":   "https://e.com/search?q=:fire:",
		"https://e.com/#:tada:":           "https://e.com/#:tada:",
		"key=:fire:":                      "key=:fire:",
		`done (:tada:) ":fire:"`:          `done (🎉) "🔥"`,
	}
	if got := expandShortcodes(":tada: was :tada: now :fire:", "was :tada: then", nil); got != ":tada: was :tada: now 🔥" {
		t.Errorf("keep: %q", got)
	}
	for in, want := range cases {
		if got := expandShortcodes(in, "", nil); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	m, f := newTestModelFake(t, 120, 40)
	m.input.SetValue("ship it :fire:")
	press(m, "enter")
	if len(f.sent) != 1 || f.sent[0].Body != "ship it 🔥" {
		t.Fatalf("sent %+v", f.sent)
	}
	runInput(t, m, "/topic :fire: launch")
	if f.called("send /topic") || len(f.sent) != 1 {
		t.Fatalf("a command was sent: %+v", f.sent)
	}
	// An edit leaves the shortcodes the message already had.
	f.msgs[10] = append(f.msgs[10], client.Message{Message: proto.Message{ID: 990, Chan: 10, Author: 1, Body: "raw :tada: here", Time: 1}})
	m.reloadPane(&m.main)
	selectMsg(t, m, 990)
	press(m, "e")
	m.input.SetValue("raw :tada: here :fire:")
	m.input.CursorEnd()
	press(m, "enter")
	if len(f.edits) != 1 || f.edits[0] != "raw :tada: here 🔥" || m.editOrig != "" {
		t.Fatalf("edit: %q", f.edits)
	}
	runInput(t, m, "//x :tada:")
	if len(f.sent) != 2 || f.sent[1].Body != "/x 🎉" {
		t.Fatalf("//: %+v", f.sent)
	}
}
