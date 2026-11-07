package screen

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

func TestParseStylesAndControls(t *testing.T) {
	in := "\x1b[0m\x1b[38;2;177;185;249m#lobby\x1b[0m ok\r\n" +
		"a\tb\x1b]0;evil title\x07c\x1bPq#0\x1b\\d\x07\x1b[2Je\x1b(Bf\r\n" +
		"wide 世界 \x1b[1;4;7mX‮Y\r\n"
	g := Parse(in, 80, 24)
	if g.H != 3 {
		t.Fatalf("rows %d", g.H)
	}
	if got := g.Text(); got != "#lobby ok\na       bcdef\nwide 世界 XY" {
		t.Fatalf("text %q", got)
	}
	c := g.Row(0)[0]
	if c.S != "#" || c.St.A&HasFG == 0 || c.St.FG != 0xb1b9f9 {
		t.Fatalf("styled cell %+v", c)
	}
	if g.Row(0)[6].St != (Style{}) {
		t.Fatalf("reset: %+v", g.Row(0)[6])
	}
	r := g.Row(2)
	if r[5].S != "世" || r[5].W != 2 || r[6].W != 0 {
		t.Fatalf("wide: %+v %+v", r[5], r[6])
	}
	if x := r[10]; x.S != "X" || x.St.A&(Bold|Underline|Reverse) != Bold|Underline|Reverse {
		t.Fatalf("attrs %+v", x)
	}
	for _, c := range g.C {
		if strings.ContainsAny(c.S, "\x1b\x07\r\n\t‮") {
			t.Fatalf("control in cell %q", c.S)
		}
	}
}

func TestParseLimits(t *testing.T) {
	var b strings.Builder
	for i := range 300 {
		fmt.Fprintf(&b, "%d %s\r\n", i, strings.Repeat("x", 600))
	}
	g := Parse(b.String(), MaxCols+50, MaxRows+50)
	if g.W != MaxCols || g.H != MaxRows {
		t.Fatalf("size %dx%d", g.W, g.H)
	}
	if !strings.HasPrefix(g.Text(), "100 ") {
		t.Fatalf("kept the wrong rows: %.10q", g.Text())
	}
	// A wide grapheme never straddles the right edge.
	g = Parse("abc世", 4, 1)
	if g.W != 3 || g.Text() != "abc" {
		t.Fatalf("edge %d %q", g.W, g.Text())
	}
}

func roundTrip(t *testing.T, e *Encoder, d *Decoder, g *Grid, force bool) bool {
	t.Helper()
	upd, key, err := e.Encode(g, "build", force)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Apply(upd); err != nil {
		t.Fatal(err)
	}
	if !d.Grid().Equal(g) {
		t.Fatalf("decoded grid differs (key %v):\n%s\n----\n%s", key, d.Grid().Text(), g.Text())
	}
	return key
}

func TestEncodeDecodeDiffs(t *testing.T) {
	var e Encoder
	var d Decoder
	g := Parse("\x1b[32m$ go test ./...\x1b[0m\r\nok  pkg  1.2s\r\n世界\r\n", 40, 6)
	if !roundTrip(t, &e, &d, g, false) {
		t.Fatal("first update not a keyframe")
	}
	if d.Label() != "build" {
		t.Fatalf("label %q", d.Label())
	}
	rng := rand.New(rand.NewPCG(1, 2))
	diffs := 0
	for range 200 {
		g = g.Clone()
		y, x := rng.IntN(g.H), rng.IntN(g.W)
		switch rng.IntN(3) {
		case 0:
			put(g.Row(y), x, Cell{S: string(rune('a' + rng.IntN(26))), W: 1, St: Style{FG: rng.Uint32() & 0xffffff, A: HasFG}})
		case 1:
			if x+1 < g.W {
				put(g.Row(y), x, Cell{S: "界", W: 2})
			}
		default:
			put(g.Row(y), x, blank)
		}
		if !roundTrip(t, &e, &d, g, false) {
			continue
		}
		diffs++
	}
	if diffs > 40 {
		t.Fatalf("%d keyframes for single-cell edits", diffs)
	}
}

// A row of endless emoji joiner chains still encodes: each cell's text is
// capped, so a run's 16-bit length can't wrap.
func TestLongGraphemes(t *testing.T) {
	chain := "👩" + strings.Repeat("‍👩", 60)
	g := Parse(strings.Repeat(chain, 300), MaxCols, 1)
	var e Encoder
	var d Decoder
	roundTrip(t, &e, &d, g, false)
	for _, c := range g.C {
		if len(c.S) > maxCellBytes {
			t.Fatalf("cell of %d bytes", len(c.S))
		}
	}
}

// put writes c at x the way a terminal would: no half wide graphemes left.
func put(row []Cell, x int, c Cell) {
	clearAt := func(x int) {
		if row[x].W == 0 && x > 0 && row[x-1].W == 2 {
			row[x-1] = blank
		}
		if row[x].W == 2 && x+1 < len(row) {
			row[x+1] = blank
		}
	}
	clearAt(x)
	if c.W == 2 {
		clearAt(x + 1)
	}
	row[x] = c
	if c.W == 2 {
		row[x+1] = Cell{St: c.St}
	}
}

// A lost diff costs nothing once a later one arrives (diffs are cumulative);
// stale, duplicate and replayed updates are ignored.
func TestDecoderLossAndReplay(t *testing.T) {
	var e Encoder
	g := Parse("one\r\ntwo\r\nthree\r\n", 20, 3)
	key, _, _ := e.Encode(g, "p", false)
	g1 := g.Clone()
	g1.Row(0)[0] = Cell{S: "O", W: 1}
	d1, _, _ := e.Encode(g1, "p", false)
	g2 := g1.Clone()
	g2.Row(2)[0] = Cell{S: "T", W: 1}
	d2, _, _ := e.Encode(g2, "p", false)

	var d Decoder
	if ch, _ := d.Apply(d2); ch {
		t.Fatal("diff applied without its keyframe")
	}
	mustApply(t, &d, key)
	mustApply(t, &d, d2) // d1 lost
	if !d.Grid().Equal(g2) {
		t.Fatalf("after d2: %q", d.Grid().Text())
	}
	for _, old := range [][]byte{d1, d2, key} {
		if ch, err := d.Apply(old); ch || err != nil {
			t.Fatalf("stale update applied: %v %v", ch, err)
		}
	}
	// A new keyframe resets; the old key's diffs no longer apply.
	k2, isKey, _ := e.Encode(g2, "p", true)
	if !isKey {
		t.Fatal("forced keyframe")
	}
	mustApply(t, &d, k2)
	if ch, _ := d.Apply(d1); ch {
		t.Fatal("diff of an old keyframe applied")
	}
	if len(e.Current()) != 1 {
		t.Fatalf("current after a keyframe: %d updates", len(e.Current()))
	}
}

func mustApply(t *testing.T, d *Decoder, u []byte) {
	t.Helper()
	ch, err := d.Apply(u)
	if err != nil || !ch {
		t.Fatalf("apply: %v %v", ch, err)
	}
}

func TestDecoderRejectsHostile(t *testing.T) {
	var d Decoder
	if _, err := d.Apply([]byte("not deflate")); err == nil {
		t.Fatal("garbage accepted")
	}
	// A deflate bomb: far over maxBody once inflated.
	var e Encoder
	_ = e
	bomb := deflate(t, bytes.Repeat([]byte{0}, maxBody+10))
	if _, err := d.Apply(bomb); err == nil {
		t.Fatal("bomb accepted")
	}
	hdr := func(typ byte, w, h uint16, nspans uint16) []byte {
		b := []byte{version, typ, 0, 0, 0, 1, 0, 0, 0, 0, byte(w >> 8), byte(w), byte(h >> 8), byte(h), 0, byte(nspans >> 8), byte(nspans)}
		return b
	}
	for name, body := range map[string][]byte{
		"too wide":       hdr(typeKey, MaxCols+1, 1, 0),
		"zero rows":      hdr(typeKey, 1, 0, 0),
		"spans > rows":   hdr(typeKey, 2, 1, 2),
		"span past edge": append(hdr(typeKey, 2, 1, 1), 0, 0, 0, 1, 0, 2, 0, 0),
		"row past end":   append(hdr(typeKey, 2, 1, 1), 0, 1, 0, 0, 0, 1, 0, 0),
		"bad attr":       append(hdr(typeKey, 2, 1, 1), 0, 0, 0, 0, 0, 1, 0, 1, 0x80, 0, 0),
		"text past end":  append(hdr(typeKey, 2, 1, 1), 0, 0, 0, 0, 0, 1, 0, 1, 0, 0, 9, 'a'),
		"trailing bytes": append(hdr(typeKey, 2, 1, 0), 1),
		"bad version":    append([]byte{9}, hdr(typeKey, 2, 1, 0)[1:]...),
	} {
		var d Decoder
		if _, err := d.Apply(deflate(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Escape sequences in cell text never survive decoding.
	body := append(hdr(typeKey, 10, 1, 1), 0, 0, 0, 0, 0, 10, 0, 1, 0, 0, 7)
	body = append(body, "\x1b[2Ja\x07b"...)
	if _, err := d.Apply(deflate(t, body)); err != nil {
		t.Fatal(err)
	}
	for _, c := range d.Grid().C {
		if strings.ContainsAny(c.S, "\x1b\x07") {
			t.Fatalf("control survived: %q", c.S)
		}
	}
}

func deflate(t *testing.T, b []byte) []byte {
	t.Helper()
	var z bytes.Buffer
	w, _ := newFlate(&z)
	_, _ = w.Write(b)
	_ = w.Close()
	return z.Bytes()
}

func TestScrub(t *testing.T) {
	for _, c := range []struct{ in, keep, gone string }{
		{"export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE", "export", "AKIAIOSFODNN7EXAMPLE"},
		{"token ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789", "token", "ghp_aBcDeF"},
		{"ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz", "ANTHROPIC_API_KEY=", "abcdefghij"},
		{"password: hunter22", "password:", "hunter22"},
		{"curl https://bob:s3cretpw@example.com/x", "https://bob:", "s3cretpw"},
		{"Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345", "Authorization", "abcdefghijklmnop"},
		{"https://tc.example:7443/hooks/abcdefghijklmnopqrstuvwxyz", "/hooks/", "abcdefghijklmnopqrstuvwxyz"},
	} {
		g := Parse(c.in, 120, 1)
		if Scrub(g) == 0 {
			t.Errorf("%q: nothing scrubbed", c.in)
		}
		txt := g.Text()
		if strings.Contains(txt, c.gone) || !strings.Contains(txt, c.keep) {
			t.Errorf("%q → %q", c.in, txt)
		}
	}
	g := Parse("ok\r\n-----BEGIN OPENSSH PRIVATE KEY-----\r\nb3BlbnNzaC1rZXktdjEAAAAA\r\n-----END OPENSSH PRIVATE KEY-----\r\nafter", 60, 10)
	if Scrub(g) != 1 {
		t.Fatal("key block not scrubbed once")
	}
	txt := g.Text()
	if strings.Contains(txt, "b3BlbnNz") || strings.Contains(txt, "PRIVATE") || !strings.Contains(txt, "after") || !strings.HasPrefix(txt, "ok") {
		t.Fatalf("key block: %q", txt)
	}
	// A token soft-wrapped over three rows is masked on every row.
	tok := "ghp_" + strings.Repeat("Ab1", 25)
	g = Parse(tok[:30]+"\r\n"+tok[30:60]+"\r\n"+tok[60:]+" tail\r\n", 30, 3)
	Scrub(g)
	if txt := g.Text(); strings.ContainsAny(txt, "Ab1") || !strings.Contains(txt, "tail") {
		t.Fatalf("wrapped token: %q", txt)
	}
	// A key whose BEGIN scrolled off: the base64 rows above END go too.
	g = Parse("$ cat key\r\nb3BlbnNzaC1rZXktdjEAAAAA\r\nQUFBQUFBQUFBQUFBQUFB\r\n-----END OPENSSH PRIVATE KEY-----\r\n", 60, 4)
	Scrub(g)
	if txt := g.Text(); strings.Contains(txt, "b3Bl") || strings.Contains(txt, "QUFB") || !strings.Contains(txt, "cat key") {
		t.Fatalf("key body: %q", txt)
	}
	if l := ScrubText("psql PGPASSWORD=hunter22"); strings.Contains(l, "hunter22") {
		t.Fatalf("label %q", l)
	}
	// The stream's scrubber remembers rows, and still masks every picture.
	var sc Scrubber
	for i := range 3 {
		g := Parse(fmt.Sprintf("line %d\r\npassword=hunter22\r\n", i%2), 40, 2)
		if sc.Scrub(g) != 1 || strings.Contains(g.Text(), "hunter22") {
			t.Fatalf("picture %d: %q", i, g.Text())
		}
	}
	if len(sc.seen) > 2 {
		t.Fatalf("scrubber keeps %d rows", len(sc.seen))
	}
	g = Parse("go test ./... ok 1.2s", 60, 1)
	if Scrub(g) != 0 {
		t.Fatalf("scrubbed plain output: %q", g.Text())
	}
}

func TestChunksReassemble(t *testing.T) {
	upd := make([]byte, 3*proto.ScreenChunkData+17)
	for i := range upd {
		upd[i] = byte(i * 7)
	}
	cs, err := Chunks(9, upd)
	if err != nil || len(cs) != 4 {
		t.Fatalf("%d chunks %v", len(cs), err)
	}
	for _, c := range cs {
		if n := proto.DgEncHeaderLen + len(c) + 16; n > proto.MaxDatagram {
			t.Fatalf("sealed chunk %d bytes", n)
		}
	}
	var r Reassembler
	now := time.Now()
	order := []int{2, 0, 0, 3}
	for _, i := range order {
		if _, ok := r.Add(cs[i], now); ok {
			t.Fatal("complete too early")
		}
	}
	got, ok := r.Add(cs[1], now)
	if !ok || !bytes.Equal(got, upd) {
		t.Fatal("reassembly")
	}
	if _, ok := r.Add(cs[1], now); ok {
		t.Fatal("late duplicate completed again")
	}
	// Partials are bounded and expire.
	for id := uint32(100); id < 120; id++ {
		c, _ := Chunks(id, upd)
		r.Add(c[0], now)
	}
	if len(r.parts) > maxPartials {
		t.Fatalf("%d partials", len(r.parts))
	}
	c, _ := Chunks(500, upd)
	r.Add(c[0], now.Add(partialTTL+time.Second))
	if len(r.parts) != 1 {
		t.Fatalf("expired partials kept: %d", len(r.parts))
	}
	if _, err := Chunks(1, make([]byte, MaxUpdate+1)); err == nil {
		t.Fatal("oversized update chunked")
	}
}
