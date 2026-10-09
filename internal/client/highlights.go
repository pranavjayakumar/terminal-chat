package client

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"modernc.org/sqlite"
)

// The SQL driver's function table is process-wide, so this is registered
// once, here, for every connection.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("tc_lower", 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			switch v := args[0].(type) {
			case string:
				return strings.ToLower(v), nil
			case []byte:
				return strings.ToLower(string(v)), nil
			}
			return "", nil
		})
}

// Highlight keywords are words the user wants treated like an @mention:
// they are stored in the local cache (never sent anywhere), matched
// case-insensitively as whole words, and make an unread message count as a
// mention (ChannelView.Mention) unless it is the user's own or a bot's.
const (
	MaxHighlights   = 50
	maxHighlightLen = 64
)

// NormalizeHighlight validates a keyword and returns its stored form
// (lowercase). A keyword is one word: no whitespace or control characters,
// at least one letter or digit, at most 64 bytes.
func NormalizeHighlight(word string) (string, error) {
	if !utf8.ValidString(word) { // ToLower would turn bad bytes into U+FFFD
		return "", errors.New("keyword is not valid text")
	}
	w := strings.ToLower(strings.TrimSpace(word))
	if w == "" {
		return "", errors.New("empty keyword")
	}
	if len(w) > maxHighlightLen {
		return "", fmt.Errorf("keyword is over %d bytes", maxHighlightLen)
	}
	hasWord := false
	for _, r := range w {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("a keyword is a single word (no spaces)")
		}
		if isWordRune(r) && r != '_' {
			hasWord = true
		}
	}
	if !hasWord {
		return "", errors.New("a keyword needs a letter or digit")
	}
	return w, nil
}

// Highlights lists the keywords, sorted.
func (c *Client) Highlights() []string { return highlightsOf(c.db) }

func highlightsOf(q dbtx) []string {
	rows, err := q.Query(`SELECT word FROM highlights ORDER BY word`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var w string
		if rows.Scan(&w) == nil {
			out = append(out, w)
		}
	}
	return out
}

// AddHighlight adds a keyword; added is false if it was already there.
func (c *Client) AddHighlight(word string) (added bool, err error) {
	w, err := NormalizeHighlight(word)
	if err != nil {
		return false, err
	}
	err = c.tx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM highlights WHERE word != ?`, w).Scan(&n); err != nil {
			return err
		}
		if n >= MaxHighlights {
			return fmt.Errorf("at most %d keywords", MaxHighlights)
		}
		r, err := tx.Exec(`INSERT OR IGNORE INTO highlights (word, time) VALUES (?, ?)`, w, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		k, _ := r.RowsAffected()
		added = k > 0
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("saving keyword: %w", err)
	}
	if added {
		c.post(Update{Channels: true})
	}
	return added, nil
}

// RemoveHighlight removes a keyword; removed is false if it wasn't one.
func (c *Client) RemoveHighlight(word string) (removed bool, err error) {
	w, err := NormalizeHighlight(word)
	if err != nil {
		return false, err
	}
	r, err := c.db.Exec(`DELETE FROM highlights WHERE word = ?`, w)
	if err != nil {
		return false, fmt.Errorf("removing keyword: %w", err)
	}
	n, _ := r.RowsAffected()
	if n > 0 {
		c.post(Update{Channels: true})
	}
	return n > 0, nil
}

// Highlighted reports whether body contains any of words (as returned by
// Highlights) as a whole word, case-insensitively: "deploy" matches
// "Deploy failed" and "deploy-prod" but not "deployment" or "redeploy".
// Text inside a fenced code block never matches: pasted logs and diffs
// would otherwise light up on every keyword.
func Highlighted(body string, words []string) bool {
	if len(words) == 0 {
		return false
	}
	lb := strings.ToLower(outsideCode(body))
	for _, w := range words {
		if containsWord(lb, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

// outsideCode is body without its fenced code blocks, with the same fence
// rule the renderer uses: a line starting with ``` opens or closes a block,
// and a block left open runs to the end.
func outsideCode(body string) string {
	if !strings.Contains(body, "```") {
		return body
	}
	var keep []string
	in := false
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			in = !in
			continue
		}
		if !in {
			keep = append(keep, ln)
		}
	}
	return strings.Join(keep, "\n")
}

// containsWord finds w in s with no letter, digit or underscore touching
// either end. Both are already lowercase.
func containsWord(s, w string) bool {
	if w == "" {
		return false
	}
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(w)
		before, _ := utf8.DecodeLastRuneInString(s[:j])
		after, _ := utf8.DecodeRuneInString(s[end:])
		if (j == 0 || !isWordRune(before)) && (end == len(s) || !isWordRune(after)) {
			return true
		}
		i = j + 1
	}
	return false
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// highlightFilter is an SQL prefilter for messages that may mention the user
// or contain a keyword. LIKE is only case-insensitive for ASCII, so a
// non-ASCII keyword is looked for in tc_lower(body), Go's strings.ToLower
// as an SQL function: the same lowercasing the exact check uses, without
// decoding the row into a Message first. The result is a parenthesised OR
// with its arguments.
func highlightFilter(col, meName string, words []string) (string, []any) {
	conds := []string{col + ` LIKE ? ESCAPE '\'`}
	args := []any{"%@" + likeEscape(meName) + "%"}
	for _, w := range words {
		if !isASCII(w) {
			conds = append(conds, `instr(tc_lower(`+col+`), ?) > 0`)
			args = append(args, strings.ToLower(w))
			continue
		}
		conds = append(conds, col+` LIKE ? ESCAPE '\'`)
		args = append(args, "%"+likeEscape(w)+"%")
	}
	return "(" + strings.Join(conds, " OR ") + ")", args
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
