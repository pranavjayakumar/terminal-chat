package client

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

const (
	carol = 3
	ciBot = 9
)

// seedLocal gives a localClient some people and two channels: #general
// (alice, bob, carol, the bot) and a DM with bob.
func seedLocal(t testing.TB, c *Client) {
	t.Helper()
	us := []proto.User{
		{ID: alice, Name: "alice"}, {ID: bob, Name: "bob"}, {ID: carol, Name: "carol"},
		{ID: ciBot, Name: "ci-bot", Bot: true},
	}
	err := c.tx(func(tx *sql.Tx) error {
		for _, u := range us {
			if err := upsertUser(tx, u); err != nil {
				return err
			}
		}
		for _, ch := range []proto.Channel{
			{ID: general, Name: "general", Kind: proto.ChanPublic, Members: []uint64{alice, bob, carol, ciBot}},
			{ID: dm, Kind: proto.ChanDM, Members: []uint64{alice, bob}},
		} {
			if err := upsertChan(tx, ch); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c.memUsers(us)
}

func addMsg(t testing.TB, c *Client, m proto.Message) {
	t.Helper()
	if err := upsertMsg(c.db, m, true); err != nil {
		t.Fatal(err)
	}
}

func TestHighlightedWholeWords(t *testing.T) {
	words := []string{"deploy", "ünï", "c++", "on-call"}
	for body, want := range map[string]bool{
		"deploy":                     true,
		"Deploy failed":              true,
		"DEPLOY":                     true,
		"about the deploy, thanks":   true,
		"(deploy)":                   true,
		"deploy-prod is down":        true,
		"@deploy":                    true,
		"redeploy":                   false,
		"deployment":                 false,
		"deploys":                    false,
		"my_deploy":                  false,
		"deploy_2":                   false,
		"deploy2":                    false,
		"redeploy then deploy":       true,
		"":                           false,
		"ÜNÏ here":                   true,
		"süni":                       false,
		"fixing c++ build":           true,
		"c++x":                       false,
		"who is on-call?":            true,
		"on-calling":                 false,
		"nothing relevant":           false,
		"dep loy":                    false,
		"mail me at x@deploy.com ok": true, // an address is still words around dots
	} {
		if got := Highlighted(body, words); got != want {
			t.Errorf("Highlighted(%q) = %v, want %v", body, got, want)
		}
	}
	if Highlighted("deploy", nil) || Highlighted("deploy", []string{""}) {
		t.Error("no keywords must never match")
	}
}

func TestNormalizeHighlight(t *testing.T) {
	for in, want := range map[string]string{"Deploy": "deploy", "  ONCALL ": "oncall", "c++": "c++", "Ünï": "ünï"} {
		if got, err := NormalizeHighlight(in); err != nil || got != want {
			t.Errorf("NormalizeHighlight(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "  ", "two words", "tab\tx", "ctl\x01", "---", "_", "@", strings.Repeat("a", 65), "bad\xff"} {
		if got, err := NormalizeHighlight(in); err == nil {
			t.Errorf("NormalizeHighlight(%q) = %q, want an error", in, got)
		}
	}
}

func TestHighlightsAddRemovePersist(t *testing.T) {
	c, home := localClient(t)
	if h := c.Highlights(); len(h) != 0 {
		t.Fatalf("fresh client has highlights: %v", h)
	}
	for _, w := range []string{"Deploy", "oncall"} {
		if added, err := c.AddHighlight(w); err != nil || !added {
			t.Fatalf("AddHighlight(%q) = %v, %v", w, added, err)
		}
	}
	if added, err := c.AddHighlight("DEPLOY"); err != nil || added {
		t.Fatalf("duplicate added again: %v, %v", added, err)
	}
	if _, err := c.AddHighlight("two words"); err == nil {
		t.Fatal("invalid keyword accepted")
	}
	if got := c.Highlights(); !slices.Equal(got, []string{"deploy", "oncall"}) {
		t.Fatalf("Highlights = %v", got)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c = reopen(t, home)
	if got := c.Highlights(); !slices.Equal(got, []string{"deploy", "oncall"}) {
		t.Fatalf("after reopen: %v", got)
	}
	if removed, err := c.RemoveHighlight("Deploy"); err != nil || !removed {
		t.Fatalf("RemoveHighlight = %v, %v", removed, err)
	}
	if removed, err := c.RemoveHighlight("deploy"); err != nil || removed {
		t.Fatalf("second RemoveHighlight = %v, %v", removed, err)
	}
	if got := c.Highlights(); !slices.Equal(got, []string{"oncall"}) {
		t.Fatalf("after remove: %v", got)
	}
	// A snapshot reset replaces server data, not the user's keywords.
	if err := resetCache(c.db, serverID); err != nil {
		t.Fatal(err)
	}
	if got := c.Highlights(); !slices.Equal(got, []string{"oncall"}) {
		t.Fatalf("after reset: %v", got)
	}
}

func TestHighlightsCapped(t *testing.T) {
	c, _ := localClient(t)
	for i := 0; i < MaxHighlights; i++ {
		if _, err := c.AddHighlight("word" + strings.Repeat("x", i)); err != nil {
			t.Fatalf("keyword %d: %v", i, err)
		}
	}
	if _, err := c.AddHighlight("onetoomany"); err == nil {
		t.Fatal("cap not enforced")
	}
	// Re-adding an existing one at the cap is not an error.
	if added, err := c.AddHighlight("word"); err != nil || added {
		t.Fatalf("existing keyword at the cap: %v, %v", added, err)
	}
}

func TestHighlightsPostChannelsUpdate(t *testing.T) {
	c, _ := localClient(t)
	if _, err := c.AddHighlight("deploy"); err != nil {
		t.Fatal(err)
	}
	if u := <-c.Updates(); !u.Channels {
		t.Fatalf("update = %+v", u)
	}
	if _, err := c.RemoveHighlight("deploy"); err != nil {
		t.Fatal(err)
	}
	if u := <-c.Updates(); !u.Channels {
		t.Fatalf("update = %+v", u)
	}
}

// TestHighlightCountsAsMention: a keyword makes an unread message by a
// person a mention exactly like @me, and never for my own or a bot's.
func TestHighlightCountsAsMention(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	addMsg(t, c, proto.Message{ID: 10, Chan: general, Author: bob, Body: "The Deploy failed again"})
	addMsg(t, c, proto.Message{ID: 11, Chan: general, Author: alice, Body: "my own deploy note"})
	addMsg(t, c, proto.Message{ID: 12, Chan: general, Author: ciBot, Body: "deploy finished"})
	addMsg(t, c, proto.Message{ID: 13, Chan: general, Author: carol, Body: "redeployment tomorrow"})

	if v := chanView(t, c, general); v.Unread != 3 || v.Mention {
		t.Fatalf("before any keyword: %+v", v)
	}
	if _, err := c.AddHighlight("deploy"); err != nil {
		t.Fatal(err)
	}
	if v := chanView(t, c, general); v.Unread != 3 || !v.Mention {
		t.Fatalf("with keyword: %+v", v)
	}

	// Only the non-person matches: no mention.
	if _, err := c.RemoveHighlight("deploy"); err != nil {
		t.Fatal(err)
	}
	if v := chanView(t, c, general); v.Mention {
		t.Fatalf("after removing the keyword: %+v", v)
	}
	c.AddHighlight("redeployment") // carol's message is a person's
	if v := chanView(t, c, general); !v.Mention {
		t.Fatalf("carol's whole-word match: %+v", v)
	}
	c.RemoveHighlight("redeployment")

	// Own and bot messages alone never mention, whatever the keyword.
	c.AddHighlight("deploy")
	addMsg(t, c, proto.Message{ID: 10, Chan: general, Author: bob, Body: "nothing here"})
	if v := chanView(t, c, general); v.Mention {
		t.Fatalf("own/bot/partial matches counted: %+v", v)
	}
	// Edited into a match, then deleted again.
	addMsg(t, c, proto.Message{ID: 10, Chan: general, Author: bob, Body: "deploy!"})
	if v := chanView(t, c, general); !v.Mention {
		t.Fatalf("edit into a match: %+v", v)
	}
	addMsg(t, c, proto.Message{ID: 10, Chan: general, Author: bob, Body: "", Deleted: true})
	if v := chanView(t, c, general); v.Mention {
		t.Fatalf("deleted message counted: %+v", v)
	}
	// Read messages stop counting.
	addMsg(t, c, proto.Message{ID: 14, Chan: general, Author: bob, Body: "deploy now"})
	if v := chanView(t, c, general); !v.Mention {
		t.Fatalf("new match: %+v", v)
	}
	if err := c.MarkRead(general, 14); err != nil {
		t.Fatal(err)
	}
	if v := chanView(t, c, general); v.Mention || v.Unread != 0 {
		t.Fatalf("after reading: %+v", v)
	}
	// @me still works alongside keywords.
	addMsg(t, c, proto.Message{ID: 15, Chan: general, Author: bob, Body: "@alice ping"})
	if v := chanView(t, c, general); !v.Mention {
		t.Fatalf("@mention: %+v", v)
	}
}

func TestHighlightNonASCIIKeywordCountsAsMention(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	addMsg(t, c, proto.Message{ID: 10, Chan: general, Author: bob, Body: "Wie ist die GRÖSSE?"})
	if _, err := c.AddHighlight("Grösse"); err != nil {
		t.Fatal(err)
	}
	if v := chanView(t, c, general); !v.Mention {
		t.Fatalf("non-ASCII keyword is case-insensitive too: %+v", v)
	}
}

// A keyword with SQL LIKE wildcards matches literally.
func TestHighlightLikeWildcardsAreLiteral(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	addMsg(t, c, proto.Message{ID: 10, Chan: general, Author: bob, Body: "anything at all"})
	if _, err := c.AddHighlight("%"); err == nil { // no letter or digit
		t.Fatal("%% accepted")
	}
	c.AddHighlight("100%")
	if v := chanView(t, c, general); v.Mention {
		t.Fatalf("wildcard matched: %+v", v)
	}
	addMsg(t, c, proto.Message{ID: 11, Chan: general, Author: bob, Body: "we hit 100% today"})
	if v := chanView(t, c, general); !v.Mention {
		t.Fatalf("literal %%: %+v", v)
	}
}

func TestHighlightedIgnoresFencedCode(t *testing.T) {
	words := []string{"deploy"}
	for name, tc := range map[string]struct {
		body string
		want bool
	}{
		"only in a block":      {"```\ndeploy failed\n```", false},
		"block with language":  {"look:\n```go\nfunc deploy() {}\n```", false},
		"indented fence":       {"  ```\ndeploy\n  ```", false},
		"unterminated block":   {"intro\n```\ndeploy forever", false},
		"before the block":     {"deploy now\n```\nx\n```", true},
		"after the block":      {"```\nx\n```\nplease deploy", true},
		"between two blocks":   {"```\na\n```\ndeploy\n```\nb\n```", true},
		"inline code is prose": {"run `deploy` please", true},
		"fence line itself":    {"```deploy\nx\n```", false},
		"no fence":             {"deploy", true},
	} {
		if got := Highlighted(tc.body, words); got != tc.want {
			t.Errorf("%s: Highlighted(%q) = %v, want %v", name, tc.body, got, tc.want)
		}
	}
}

// A keyword inside a pasted log neither flags the channel nor lands in the
// activity view.
func TestHighlightInCodeBlockIsNotAMention(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	if _, err := c.AddHighlight("deploy"); err != nil {
		t.Fatal(err)
	}
	addMsg(t, c, proto.Message{ID: 10, Chan: general, Author: bob, Body: "build log:\n```\ndeploy step 3 failed\n```"})
	if v := chanView(t, c, general); v.Unread != 1 || v.Mention {
		t.Fatalf("channel: %+v", v)
	}
	if items := c.Activity(10); len(items) != 0 {
		t.Fatalf("activity: %+v", items)
	}
	addMsg(t, c, proto.Message{ID: 11, Chan: general, Author: bob, Body: "did the deploy go out?\n```\nok\n```"})
	if v := chanView(t, c, general); !v.Mention {
		t.Fatalf("a keyword in prose still counts: %+v", v)
	}
	if items := c.Activity(10); len(items) != 1 || items[0].Kind != ActHighlight || items[0].Msg != 11 {
		t.Fatalf("activity: %+v", items)
	}
}
