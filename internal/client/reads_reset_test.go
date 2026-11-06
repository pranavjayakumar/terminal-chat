package client

import "testing"

// Unconfirmed read markers wait out a reset snapshot and come back only
// when the server's history is the one they were set in.
func TestUnconfirmedReadsAcrossReset(t *testing.T) {
	incA, incB := []byte("AAAAAAAAAAAAAAAA"), []byte("BBBBBBBBBBBBBBBB")
	read := func(c *Client) uint64 { return c.uintMap(`SELECT chan, seq FROM reads`)[general] }
	for _, tc := range []struct {
		name  string
		head  uint64
		inc   []byte
		crash bool // a second reset before the RES (crash mid-snapshot)
		want  uint64
	}{
		{"too far behind, same history", 20000, incA, false, 500},
		{"another incarnation", 20000, incB, false, 40},
		{"cursor past the head", 300, nil, false, 40},
		{"crash then another incarnation", 20000, incB, true, 40},
		{"crash then same history", 20000, incA, true, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := localClient(t)
			if err := resetCache(c.db, serverID, 0, nil); err != nil {
				t.Fatal(err)
			}
			if err := c.MarkRead(general, 500); err != nil { // offline: dirty
				t.Fatal(err)
			}
			if err := resetCache(c.db, serverID, 400, incA); err != nil {
				t.Fatal(err)
			}
			if tc.crash { // restarted: last_seq 0, no inc
				if err := resetCache(c.db, serverID, 0, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := setRead(c.db, general, 40, false); err != nil { // the snapshot's marker
				t.Fatal(err)
			}
			if err := unstashReads(c.db, tc.head, tc.inc); err != nil {
				t.Fatal(err)
			}
			if got := read(c); got != tc.want {
				t.Fatalf("read marker %d, want %d", got, tc.want)
			}
			if st, _ := loadReadStash(c.db); st != nil {
				t.Fatal("stash left behind")
			}
		})
	}
}
