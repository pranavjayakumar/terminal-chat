package tui

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/skailama/tc/internal/sanitize"
)

//go:generate go run ./emojigen -out emoji_data.go

// emojiDef is one row of emojiTable (emoji_data.go).
type emojiDef struct {
	e     string // the emoji
	names string // shortcodes, space-separated; the first is the main one
	keys  string // search words, space-separated
	cat   uint8  // index into emojiCats
}

func (d emojiDef) name() string {
	n, _, _ := strings.Cut(d.names, " ")
	return n
}

type emojiCat struct{ name, icon string }

// emojiCats are the picker's categories in table order; "Frequently used"
// comes before them.
var emojiCats = []emojiCat{
	{"Smileys & emotion", "😀"}, {"People & body", "👋"}, {"Animals & nature", "🐻"},
	{"Food & drink", "🍕"}, {"Activities", "⚽"}, {"Travel & places", "🚗"},
	{"Objects", "💡"}, {"Symbols", "🔣"},
}

const recentIcon = "🕘"

// emojiExtra are shortcodes tc accepted before the table existed.
var emojiExtra = map[string]string{"check": "✅"}

// emojiIndex maps every shortcode and every emoji to its row.
var emojiIndex = func() map[string]int {
	ix := make(map[string]int, len(emojiTable)*3)
	for i, d := range emojiTable {
		ix[d.e] = i
		for n := range strings.FieldsSeq(d.names) {
			ix[n] = i
		}
	}
	return ix
}()

// emojiByName is the emoji for a shortcode ("tada", no colons).
func emojiByName(name string) (string, bool) {
	name = strings.ToLower(name)
	if i, ok := emojiIndex[name]; ok && !isEmojiRune(name) {
		return emojiTable[i].e, true
	}
	e, ok := emojiExtra[name]
	return e, ok
}

// gridSafe reports whether e draws as one wide cell in terminals: one
// code point, or a base and U+FE0F, two cells by the TUI's measure. ZWJ
// sequences and skin tones measure 2 here but not in every terminal.
func gridSafe(e string) bool {
	rs := []rune(e)
	return (len(rs) == 1 || len(rs) == 2 && rs[1] == 0xFE0F) && strWidth(e) == 2 && sanitize.Line(e) == e
}

func isEmojiRune(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return r >= 0x80
}

// emojiDefOf is the table row of emoji e, or -1.
func emojiDefOf(e string) int {
	if i, ok := emojiIndex[e]; ok && isEmojiRune(e) {
		return i
	}
	return -1
}

// emojiName is e's main shortcode, or "" when it isn't in the table.
func emojiName(e string) string {
	if i := emojiDefOf(e); i >= 0 {
		return emojiTable[i].name()
	}
	return ""
}

// resolveEmoji turns "+1", ":tada:" or a literal emoji into the emoji to
// send.
func resolveEmoji(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if e, ok := emojiByName(strings.Trim(s, ":")); ok {
		return e, true
	}
	if s == "" || strings.ContainsAny(s, " \t\n") || sanitize.Line(s) != s || strWidth(s) > 4 || len(s) > 32 {
		return "", false
	}
	for _, r := range s {
		if r < 0x80 {
			return "", false
		}
	}
	return s, true
}

// emojiHit is a search result: a table row and where the query matched in
// the shortcode shown for it (-1: it matched a keyword, not the name).
type emojiHit struct {
	def  int
	name string // the shortcode that matched (or the main one)
	at   int
}

// searchEmoji ranks the table for q (lowercase, no colons): an exact
// shortcode first, then emoji used often, then shortcode prefixes, then
// shortcodes containing q, then keyword matches. kw false leaves keyword
// matches out (":" completion matches names only).
//
// Several words ("thumbs up") rank by the first; each other word must
// start a keyword or be in a shortcode.
func searchEmoji(q string, freq []string, kw bool, limit int) []emojiHit {
	words := strings.Fields(strings.ToLower(strings.Trim(q, ": ")))
	if len(words) == 0 {
		return nil
	}
	q = words[0]
	more := words[1:]
	type scored struct {
		emojiHit
		rank int
	}
	var out []scored
	for i, d := range emojiTable {
		best := scored{emojiHit{def: i, name: d.name(), at: -1}, 99}
		for n := range strings.FieldsSeq(d.names) {
			r := 99
			at := strings.Index(n, q)
			switch {
			case n == q:
				r = 0
			case at == 0:
				r = 2
			case at > 0 && (n[at-1] == '_' || n[at-1] == '-'):
				r = 3 // a word inside the name: "fire" in "camp_fire"
			case at > 0:
				r = 4
			}
			if r < best.rank {
				best = scored{emojiHit{i, n, at}, r}
			}
		}
		if best.rank == 99 && kw {
			for k := range strings.FieldsSeq(d.keys) {
				if strings.HasPrefix(k, q) {
					best.rank = 5
					break
				}
			}
		}
		if best.rank == 99 || !matchesAll(d, more) {
			continue
		}
		if best.rank > 0 && slices.Contains(freq, d.e) {
			best.rank = 1
		}
		out = append(out, best)
	}
	slices.SortStableFunc(out, func(a, b scored) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		if a.rank == 1 { // used often: the most used first
			return slices.Index(freq, emojiTable[a.def].e) - slices.Index(freq, emojiTable[b.def].e)
		}
		if a.rank < 5 { // the closest name first: "fire" before "fire_engine"
			return len(a.name) - len(b.name)
		}
		return 0
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	hits := make([]emojiHit, len(out))
	for i, s := range out {
		hits[i] = s.emojiHit
	}
	return hits
}

// matchesAll reports whether every word is in one of d's shortcodes or
// starts one of its keywords.
func matchesAll(d emojiDef, words []string) bool {
	for _, w := range words {
		if !strings.Contains(d.names, w) && !slices.ContainsFunc(strings.Fields(d.keys), func(k string) bool { return strings.HasPrefix(k, w) }) {
			return false
		}
	}
	return true
}

// shortcodeChar is a character a :shortcode: may contain.
func shortcodeChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '+' || r == '-'
}

// expandShortcodes turns each known :name: in a message into its emoji,
// outside code (fenced blocks and inline spans), and only as a word of its
// own: after a space, the line's start, an opening bracket or quote, or
// another converted shortcode (":tada::rocket:"), so "12:100:3", "?q=:x:"
// and URLs stay as typed. A :name: that keep already holds stays too: an
// edit doesn't rewrite text the author left alone. used gets every emoji
// it put in.
func expandShortcodes(s, keep string, used func(string)) string {
	if !strings.Contains(s, ":") {
		return s
	}
	var b strings.Builder
	fence := false
	for _, line := range strings.SplitAfter(s, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			fence = !fence
			b.WriteString(line)
			continue
		}
		if fence {
			b.WriteString(line)
			continue
		}
		b.WriteString(expandLine(line, keep, used))
	}
	return b.String()
}

// opensWord are the characters a shortcode may follow.
const opensWord = " \t([{\"'>"

func expandLine(line, keep string, used func(string)) string {
	rs := []rune(line)
	var b strings.Builder
	code := false
	after := -1 // index just past the last converted shortcode
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == '`' {
			code = !code
		}
		if r != ':' || code || i > 0 && i != after && !strings.ContainsRune(opensWord, rs[i-1]) {
			b.WriteRune(r)
			continue
		}
		j := i + 1
		for j < len(rs) && shortcodeChar(rs[j]) {
			j++
		}
		if j == i+1 || j >= len(rs) || rs[j] != ':' || j+1 < len(rs) && shortcodeChar(rs[j+1]) {
			b.WriteRune(r)
			continue
		}
		e, ok := emojiByName(string(rs[i+1 : j]))
		if !ok || keep != "" && strings.Contains(keep, string(rs[i:j+1])) {
			b.WriteRune(r)
			continue
		}
		b.WriteString(e)
		if used != nil {
			used(e)
		}
		i, after = j, j+1
	}
	return b.String()
}

// inCode reports whether rune index col of the input's line row is inside
// a fenced block or an inline code span, where ":" never completes.
func inCode(value string, row, col int) bool {
	lines := strings.Split(value, "\n")
	fence := false
	for i := 0; i < row && i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimLeft(lines[i], " \t"), "```") {
			fence = !fence
		}
	}
	if fence || row >= len(lines) {
		return fence
	}
	line := []rune(lines[row])
	ticks := 0
	for _, r := range line[:min(col, len(line))] {
		if r == '`' {
			ticks++
		}
	}
	return ticks%2 == 1
}

// ---- frequently used ----

// maxFreq caps the frequently used list kept in ui.json.
const maxFreq = 40

// freqSeed is what a fresh install offers before anything was used.
var freqSeed = []string{"👍", "🚀", "👀", "🎉", "🔥", "😂", "🙏", "💯", "👏", "🤔"}

// emojiUse is one frequently used emoji and how often it was picked.
type emojiUse struct {
	E string `json:"e"`
	N int    `json:"n"`
}

// freqList is the frequently used emoji, the most used first.
func (m *Model) freqList() []string {
	if m.ui.Emoji == nil {
		return freqSeed
	}
	out := make([]string, 0, len(m.ui.Emoji))
	for _, u := range m.ui.Emoji {
		out = append(out, u.E)
	}
	return out
}

// noteEmoji counts one use of e and saves the list.
func (m *Model) noteEmoji(e string) {
	if m.countEmoji(e) {
		m.savePrefs()
	}
}

// countEmoji counts one use of e in memory, reporting whether the list
// changed. The newest use wins a tie, so something picked once just now
// sorts before older single uses.
func (m *Model) countEmoji(e string) bool {
	if !gridSafe(e) {
		return false // only what the picker grid can draw
	}
	list := m.ui.Emoji
	if list == nil {
		for _, s := range freqSeed {
			list = append(list, emojiUse{E: s})
		}
	}
	n := 1
	if i := slices.IndexFunc(list, func(u emojiUse) bool { return u.E == e }); i >= 0 {
		n = min(n+list[i].N, 1<<30)
		list = slices.Delete(list, i, i+1)
	}
	i := 0
	for i < len(list) && list[i].N > n {
		i++
	}
	list = slices.Insert(list, i, emojiUse{E: e, N: n})
	if len(list) > maxFreq {
		list = list[:maxFreq]
	}
	m.ui.Emoji = list
	return true
}
