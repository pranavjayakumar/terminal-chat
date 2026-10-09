package client

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

const secretChan = 102 // bob and carol only

// applyReact feeds a react event through the same path the connection uses.
func applyReact(t *testing.T, c *Client, seq uint64, kind string, actor, msg uint64, emoji string, at int64) {
	t.Helper()
	ev := proto.Event{Seq: seq, Kind: kind, Chan: general, Time: at, Actor: actor,
		Data: proto.MustRaw(proto.Reaction{Msg: msg, Emoji: emoji})}
	if _, err := c.applyEvent(ev); err != nil {
		t.Fatal(err)
	}
}

type want struct {
	kind   ActivityKind
	msg    uint64
	thread uint64
	ch     uint64
}

func summarize(items []ActivityItem) []want {
	out := make([]want, len(items))
	for i, it := range items {
		out[i] = want{it.Kind, it.Msg, it.Thread, it.Chan}
	}
	return out
}

func seedActivity(t *testing.T) *Client {
	t.Helper()
	c, _ := localClient(t)
	seedLocal(t, c)
	err := c.tx(func(tx *sql.Tx) error {
		return upsertChan(tx, proto.Channel{ID: secretChan, Name: "secret", Kind: proto.ChanPrivate, Members: []uint64{bob, carol}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddHighlight("deploy"); err != nil {
		t.Fatal(err)
	}
	m := func(id, ch, author, thread uint64, body string) {
		x := proto.Message{ID: id, Chan: ch, Author: author, Thread: thread, Body: body, Time: int64(id) * 1000}
		if body == "" {
			x.Deleted = true
		}
		addMsg(t, c, x)
	}
	m(1, general, alice, 0, "what do we do about the deploy?") // my thread
	m(2, general, bob, 0, "unrelated chatter")
	m(3, general, bob, 1, "try rolling back") // reply in my thread
	m(4, general, carol, 0, "hey @alice, lunch?")
	m(5, general, bob, 0, "who owns the cron job?")
	m(6, general, carol, 5, "no idea") // before I joined that thread
	m(7, general, alice, 5, "I think I do")
	m(8, general, carol, 5, "thanks alice") // after I joined it
	m(9, dm, bob, 0, "psst, got a sec?")
	m(10, dm, alice, 0, "sure")
	m(11, general, ciBot, 0, "deploy done @alice") // a bot
	m(12, general, bob, 0, "Deploy window tonight")
	m(13, general, carol, 0, "") // deleted
	m(14, secretChan, bob, 0, "@alice are you here")
	m(15, dm, bob, 0, "@alice are you there?") // DM and mention: once, as a mention
	m(16, general, bob, 1, "deploy is rolled back")

	applyReact(t, c, 1, proto.KReactAdd, carol, 1, "👍", 16500)
	applyReact(t, c, 2, proto.KReactAdd, bob, 7, "🎉", 8500)
	applyReact(t, c, 3, proto.KReactAdd, carol, 1, "👀", 17000)
	applyReact(t, c, 4, proto.KReactRemove, carol, 1, "👀", 17500) // taken back
	applyReact(t, c, 5, proto.KReactAdd, bob, 2, "👍", 18000)      // not my message
	applyReact(t, c, 6, proto.KReactAdd, alice, 1, "👍", 19000)    // my own reaction
	return c
}

func TestActivityKindsOrderAndJumpTargets(t *testing.T) {
	c := seedActivity(t)
	got := summarize(c.Activity(100))
	wantItems := []want{
		{ActReaction, 1, 0, general},   // 👍 from carol, t=16500
		{ActHighlight, 16, 1, general}, // reply in my thread with a keyword: highlight beats reply
		{ActMention, 15, 0, dm},        // a DM that mentions me: once, as a mention
		{ActHighlight, 12, 0, general},
		{ActDM, 9, 0, dm},
		{ActReaction, 7, 5, general}, // 🎉 from bob on my reply, t=8500
		{ActReply, 8, 5, general},    // carol replying after I joined thread 5
		{ActMention, 4, 0, general},
		{ActReply, 3, 1, general}, // bob replying in my thread
	}
	if len(got) != len(wantItems) {
		t.Fatalf("got %d items, want %d:\n%+v", len(got), len(wantItems), c.Activity(100))
	}
	for i := range wantItems {
		if got[i] != wantItems[i] {
			t.Errorf("item %d = %+v, want %+v", i, got[i], wantItems[i])
		}
	}

	items := c.Activity(100)
	for i := 1; i < len(items); i++ {
		if items[i].Time > items[i-1].Time {
			t.Errorf("not newest first at %d: %d after %d", i, items[i].Time, items[i-1].Time)
		}
	}
	if r := items[0]; r.Author != carol || r.Emoji != "👍" || r.Time != 16500 || r.Preview != "what do we do about the deploy?" {
		t.Errorf("reaction item = %+v", r)
	}
	if r := items[4]; r.Author != bob || r.Preview != "psst, got a sec?" || r.Time != 9000 {
		t.Errorf("dm item = %+v", r)
	}
	if r := items[5]; r.Author != bob || r.Emoji != "🎉" || r.Preview != "I think I do" {
		t.Errorf("reaction on a reply = %+v", r)
	}
}

func TestActivityLimitKeepsNewest(t *testing.T) {
	c := seedActivity(t)
	all := c.Activity(100)
	for _, n := range []int{1, 3, 5} {
		got := c.Activity(n)
		if len(got) != n {
			t.Fatalf("Activity(%d) returned %d", n, len(got))
		}
		for i := range got {
			if got[i] != all[i] {
				t.Errorf("Activity(%d)[%d] = %+v, want %+v", n, i, got[i], all[i])
			}
		}
	}
	if len(c.Activity(0)) != len(all) {
		t.Error("limit 0 should mean the default")
	}
}

func TestActivityExcludesOwnBotsDeletedAndOtherChannels(t *testing.T) {
	c := seedActivity(t)
	banned := map[uint64]string{
		10: "my own DM", 11: "a bot (keyword + mention)", 13: "deleted", 14: "a channel I'm not in",
		2: "no relation", 6: "reply before I took part", 5: "someone else's thread root",
	}
	for _, it := range c.Activity(100) {
		if why, bad := banned[it.Msg]; bad && it.Kind != ActReaction {
			t.Errorf("message %d (%s) is in the activity: %+v", it.Msg, why, it)
		}
	}
	// A reaction to a message that was deleted since, or from a bot, disappears.
	addMsg(t, c, proto.Message{ID: 1, Chan: general, Author: alice, Thread: 0, Deleted: true, Time: 1000})
	for _, it := range c.Activity(100) {
		if it.Kind == ActReaction && it.Msg == 1 {
			t.Errorf("reaction to a deleted message: %+v", it)
		}
	}
	// Leaving the channel takes everything with it.
	if err := c.tx(func(tx *sql.Tx) error { return c.forgetChan(tx, general, alice) }); err != nil {
		t.Fatal(err)
	}
	for _, it := range c.Activity(100) {
		if it.Chan == general {
			t.Errorf("activity from a channel I left: %+v", it)
		}
	}
	var n int
	c.db.QueryRow(`SELECT COUNT(*) FROM reacts WHERE msg IN (1, 7)`).Scan(&n)
	if n != 0 {
		t.Errorf("%d reaction rows left for a forgotten channel", n)
	}
}

// Reactions only exist as react.add events: those inside message snapshots
// have no time and are not listed.
func TestActivityReactionsFromEventsOnly(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	addMsg(t, c, proto.Message{ID: 1, Chan: general, Author: alice, Body: "mine", Time: 1000,
		Reactions: map[string][]uint64{"👍": {bob}}})
	if got := c.Activity(100); len(got) != 0 {
		t.Fatalf("snapshot reaction listed: %+v", got)
	}
	applyReact(t, c, 1, proto.KReactAdd, carol, 1, "🎉", 2000)
	// The same event again (replay) neither duplicates nor errors.
	applyReact(t, c, 2, proto.KReactAdd, carol, 1, "🎉", 2500)
	got := c.Activity(100)
	if len(got) != 1 || got[0].Emoji != "🎉" || got[0].Author != carol {
		t.Fatalf("activity = %+v", got)
	}
	// A reaction to a message we haven't cached can't be attributed.
	applyReact(t, c, 3, proto.KReactAdd, bob, 99, "🎉", 3000)
	if got := c.Activity(100); len(got) != 1 {
		t.Fatalf("uncached target listed: %+v", got)
	}
}

func TestActivityRespectsHighlightChanges(t *testing.T) {
	c := seedActivity(t)
	has := func(msg uint64) bool {
		for _, it := range c.Activity(100) {
			if it.Msg == msg && it.Kind == ActHighlight {
				return true
			}
		}
		return false
	}
	if !has(12) {
		t.Fatal("keyword message missing")
	}
	c.RemoveHighlight("deploy")
	if has(12) {
		t.Fatal("still listed after the keyword was removed")
	}
}

func TestActivityPreviewIsOneShortLine(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	addMsg(t, c, proto.Message{ID: 1, Chan: dm, Author: bob, Time: 1, Body: "\n\n  first line  \nsecond"})
	addMsg(t, c, proto.Message{ID: 2, Chan: dm, Author: bob, Time: 2, Body: "```go\nfmt.Println()\n```"})
	addMsg(t, c, proto.Message{ID: 3, Chan: dm, Author: bob, Time: 3, Body: strings.Repeat("é", 1000)})
	got := c.Activity(10)
	if len(got) != 3 {
		t.Fatalf("items = %+v", got)
	}
	if got[0].Msg != 3 || len([]rune(got[0].Preview)) != previewRunes {
		t.Errorf("long preview: %d runes", len([]rune(got[0].Preview)))
	}
	if got[1].Preview != "fmt.Println()" {
		t.Errorf("code preview = %q", got[1].Preview)
	}
	if got[2].Preview != "first line" {
		t.Errorf("preview = %q", got[2].Preview)
	}
}

func TestReactionTableIsBounded(t *testing.T) {
	defer func(old int) { maxReacts = old }(maxReacts)
	maxReacts = 40
	c, _ := localClient(t)
	seedLocal(t, c)
	addMsg(t, c, proto.Message{ID: 1, Chan: general, Author: alice, Body: "mine", Time: 1})
	for i := 0; i < maxReacts+50; i++ {
		applyReact(t, c, uint64(i+1), proto.KReactAdd, bob, 1, fmt.Sprintf("e%d", i), int64(i+1))
	}
	var n int
	c.db.QueryRow(`SELECT COUNT(*) FROM reacts`).Scan(&n)
	if n != maxReacts {
		t.Fatalf("%d rows", n)
	}
	if got := c.Activity(1); len(got) != 1 || got[0].Emoji != fmt.Sprintf("e%d", maxReacts+49) {
		t.Fatalf("newest reaction first: %+v", got)
	}
}

// ---- speed ----

// bigCache fills a cache with n messages shaped so that every activity query
// has to look at most of the table: few mentions, many replies in other
// people's threads, some DMs and reactions.
func bigCache(tb testing.TB, n int) *Client {
	tb.Helper()
	c, _ := localClient(tb)
	seedLocal(tb, c)
	if _, err := c.AddHighlight("deploy"); err != nil {
		tb.Fatal(err)
	}
	err := c.tx(func(tx *sql.Tx) error {
		base := int64(1_700_000_000_000)
		for i := 1; i <= n; i++ {
			m := proto.Message{ID: uint64(i), Chan: general, Author: bob, Time: base + int64(i)*1000,
				Body: fmt.Sprintf("message number %d about nothing in particular, just filler text", i)}
			switch {
			case i%7 == 0:
				m.Chan, m.Body = dm, "dm filler "+m.Body
				if i%97 == 0 {
					m.Author = alice
				}
			case i%3 == 0 && i > 10:
				m.Thread = uint64(i - i%50 + 1) // replies in somebody else's threads
				m.Author = carol
			case i%1500 == 0:
				m.Body = "hey @alice, a rare mention"
			}
			if err := upsertMsg(tx, m, true); err != nil {
				return err
			}
		}
		for i := 1; i <= 800; i++ {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO reacts (msg, actor, emoji, time) VALUES (?, ?, ?, ?)`,
				i, bob, "👍", int64(i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return c
}

// TestActivityIsFastOn10kMessages: the view opens on every Alt-A and
// refreshes while open, so it must stay well under a frame budget.
func TestActivityIsFastOn10kMessages(t *testing.T) {
	c := bigCache(t, 10_500)
	var n int
	c.db.QueryRow(`SELECT COUNT(*) FROM msgs`).Scan(&n)
	if n < 10_000 {
		t.Fatalf("only %d messages", n)
	}
	c.Activity(100) // warm the page cache like a running client has
	best := time.Hour
	var items []ActivityItem
	for i := 0; i < 5; i++ {
		start := time.Now()
		items = c.Activity(100)
		best = min(best, time.Since(start))
	}
	if len(items) != 100 {
		t.Fatalf("got %d items", len(items))
	}
	budget := 50 * time.Millisecond * raceSlowdown
	t.Logf("Activity(100) over %d messages: %v (budget %v)", n, best, budget)
	if best > budget {
		t.Fatalf("Activity took %v, want < %v", best, budget)
	}
}

func BenchmarkActivity10k(b *testing.B) {
	c := bigCache(b, 10_500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(c.Activity(100)) == 0 {
			b.Fatal("no items")
		}
	}
}

func TestActivitySeenPersistsAndOnlyMovesForward(t *testing.T) {
	c, home := localClient(t)
	if got := c.ActivitySeen(); got != 0 {
		t.Fatalf("fresh cache: %d", got)
	}
	if err := c.SetActivitySeen(5000); err != nil {
		t.Fatal(err)
	}
	if err := c.SetActivitySeen(3000); err != nil {
		t.Fatal(err)
	}
	if got := c.ActivitySeen(); got != 5000 {
		t.Fatalf("moved backwards: %d", got)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = reopen(t, home)
	if got := c.ActivitySeen(); got != 5000 {
		t.Fatalf("after reopen: %d", got)
	}
	// Same server's snapshot reset keeps it; another server's ids mean nothing.
	if err := resetCache(c.db, serverID); err != nil {
		t.Fatal(err)
	}
	if got := c.ActivitySeen(); got != 5000 {
		t.Fatalf("after a reset of the same server: %d", got)
	}
	if err := resetCache(c.db, []byte("another server")); err != nil {
		t.Fatal(err)
	}
	if got := c.ActivitySeen(); got != 0 {
		t.Fatalf("after switching servers: %d", got)
	}
}

// The scans look at the newest scanMessages messages only, and at those not
// older than scanAge before the newest one.
func TestActivityScansAreBounded(t *testing.T) {
	t.Run("message count", func(t *testing.T) {
		old := scanMessages
		scanMessages = 20
		t.Cleanup(func() { scanMessages = old })
		c, _ := localClient(t)
		seedLocal(t, c)
		for id := uint64(1); id <= 60; id++ {
			m := proto.Message{ID: id, Chan: general, Author: bob, Body: "filler", Time: int64(id) * 1000}
			switch id {
			case 5:
				m.Body = "old @alice"
			case 6:
				m.Chan, m.Body = dm, "old dm"
			case 7:
				m.Author, m.Body = bob, "old reply in my thread"
				m.Thread = 4
			case 4:
				m.Author, m.Body = alice, "my old thread root"
			case 45:
				m.Body = "recent @alice"
			case 46:
				m.Chan, m.Body = dm, "recent dm"
			case 47:
				m.Thread, m.Body = 44, "recent reply in my thread"
			case 44:
				m.Author, m.Body = alice, "my recent thread root"
			}
			addMsg(t, c, m)
		}
		var got []string
		for _, it := range c.Activity(100) {
			got = append(got, fmt.Sprintf("%s:%d", it.Kind, it.Msg))
		}
		want := []string{"reply:47", "dm:46", "mention:45"}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v (nothing older than the newest 20 messages)", got, want)
		}
	})
	t.Run("age", func(t *testing.T) {
		c, _ := localClient(t)
		seedLocal(t, c)
		day := int64(24 * 3600 * 1000)
		base := int64(1_700_000_000_000)
		addMsg(t, c, proto.Message{ID: 1, Chan: general, Author: bob, Body: "ancient @alice", Time: base})
		addMsg(t, c, proto.Message{ID: 2, Chan: dm, Author: bob, Body: "ancient dm", Time: base + 1})
		addMsg(t, c, proto.Message{ID: 3, Chan: general, Author: bob, Body: "recent @alice", Time: base + 40*day})
		addMsg(t, c, proto.Message{ID: 4, Chan: general, Author: carol, Body: "newest", Time: base + 41*day})
		items := c.Activity(100)
		if len(items) != 1 || items[0].Msg != 3 {
			t.Fatalf("got %+v, want only message 3 (31+ days older than the newest is out)", items)
		}
		// Relative to the newest message, not the clock: an idle cache keeps its last month.
		addMsg(t, c, proto.Message{ID: 5, Chan: general, Author: bob, Body: "@alice again", Time: base + 41*day + 1})
		if items := c.Activity(100); len(items) != 2 {
			t.Fatalf("got %+v", items)
		}
	})
}

// A non-ASCII keyword is matched in SQL (lowercased), so the scan doesn't
// have to decode every row.
func TestNonASCIIKeywordIsFilteredInSQL(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	for id := uint64(1); id <= 100; id++ {
		addMsg(t, c, proto.Message{ID: id, Chan: general, Author: bob, Body: fmt.Sprintf("filler %d", id), Time: int64(id)})
	}
	addMsg(t, c, proto.Message{ID: 101, Chan: general, Author: bob, Body: "ÜNÏ here", Time: 101})
	addMsg(t, c, proto.Message{ID: 102, Chan: general, Author: bob, Body: "süni no", Time: 102})
	addMsg(t, c, proto.Message{ID: 103, Chan: general, Author: bob, Body: "Straße und STRASSE", Time: 103})
	filter, args := highlightFilter("m.body", []string{"alice"}, []string{"ünï"})
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM msgs m WHERE `+filter, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n > 2 { // the exact whole-word check stays in Go; SQL may keep süni
		t.Fatalf("the prefilter lets %d of 103 rows through", n)
	}
	if _, err := c.AddHighlight("ünï"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddHighlight("Straße"); err != nil {
		t.Fatal(err)
	}
	items := c.Activity(10)
	if len(items) != 2 || items[0].Msg != 103 || items[1].Msg != 101 || items[0].Kind != ActHighlight {
		t.Fatalf("activity = %+v", items)
	}
	if v := chanView(t, c, general); !v.Mention {
		t.Fatal("the channel should be flagged")
	}
}
