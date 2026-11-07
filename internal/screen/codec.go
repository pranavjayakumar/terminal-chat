package screen

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"io"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Updates (PROTOCOL.md §14.5). An update is a keyframe (the whole grid) or
// a diff against the latest keyframe: the spans of cells that differ from
// it. Diffs are cumulative, never chained, so a lost diff costs nothing
// once the next one arrives, and only a lost keyframe makes a viewer wait
// (for the next one). Updates are deflate-compressed.
//
// Body (big-endian), before compression:
//
//	u8 version (1) | u8 type (1 key, 2 diff) | u32 key id | u32 diff number (0 in a key)
//	u16 cols | u16 rows | u8 label length | label | u16 span count | spans
//	span: u16 row | u16 x | u16 cells | u16 run count | runs
//	run:  u8 attr | [u24 fg if HasFG] | [u24 bg if HasBG] | u16 text length | text
//
// A run's text is laid out from the span's x for as many cells as it
// fills; whatever of the span's cells no text reaches stays blank.

const (
	version  = 1
	typeKey  = 1
	typeDiff = 2

	// MaxLabel is the longest pane label an update carries.
	MaxLabel = 64
	// MaxUpdate is the largest compressed update: what fits in
	// proto.MaxScreenChunks chunks.
	MaxUpdate = proto.MaxScreenChunks * proto.ScreenChunkData
	// maxBody bounds an update's decompressed size (a deflate bomb stops here).
	maxBody = 2 << 20
)

// ErrBadUpdate is any malformed update.
var ErrBadUpdate = errors.New("screen: malformed update")

// ErrTooBig: the pane's update doesn't fit in MaxUpdate even as a keyframe.
var ErrTooBig = errors.New("screen: pane too big to share")

// Encoder turns successive grids into updates for one stream.
type Encoder struct {
	key    *Grid
	keyID  uint32
	n      uint32
	keyLen int
	label  string
	keyUpd []byte // the latest keyframe, as sent
	last   []byte // the latest diff since it, as sent (nil: none)
}

// Encode returns the update that brings a viewer to g: a keyframe the
// first time, when forced, when the size or label changed, or when the
// diff would be over half the keyframe's size; a diff otherwise.
func (e *Encoder) Encode(g *Grid, label string, forceKey bool) (upd []byte, key bool, err error) {
	label = clipLabel(label)
	if !forceKey && e.key != nil && e.key.W == g.W && e.key.H == g.H && label == e.label {
		upd, err := encode(typeDiff, e.keyID, e.n+1, label, g, diffSpans(e.key, g))
		if err != nil {
			return nil, false, err
		}
		if len(upd) <= e.keyLen/2 || len(upd) < 256 {
			e.n++
			e.last = upd
			return upd, false, nil
		}
	}
	upd, err = encode(typeKey, e.keyID+1, 0, label, g, keySpans(g))
	if err != nil {
		return nil, false, err
	}
	e.keyID++
	e.key, e.n, e.keyLen, e.label = g.Clone(), 0, len(upd), label
	e.keyUpd, e.last = upd, nil
	return upd, true, nil
}

// Current is what brings a viewer that missed anything up to date: the
// latest keyframe and the latest diff since it (if any).
func (e *Encoder) Current() [][]byte {
	if e.keyUpd == nil {
		return nil
	}
	if e.last == nil {
		return [][]byte{e.keyUpd}
	}
	return [][]byte{e.keyUpd, e.last}
}

func clipLabel(s string) string {
	s = ScrubText(sanitize.Line(s)) // pane titles often show the command line
	for len(s) > MaxLabel {
		_, n := lastRune(s)
		s = s[:len(s)-n]
	}
	return s
}

func lastRune(s string) (rune, int) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i]&0xC0 != 0x80 {
			return rune(s[i]), len(s) - i
		}
	}
	return 0, len(s)
}

type span struct{ y, x0, x1 int } // cells [x0, x1) of row y

func keySpans(g *Grid) []span {
	var out []span
	for y := range g.H {
		row := g.Row(y)
		end := len(row)
		for end > 0 && row[end-1] == blank {
			end--
		}
		if end > 0 {
			if end < len(row) && row[end].W == 0 {
				end++
			}
			out = append(out, span{y, 0, end})
		}
	}
	return out
}

// diffSpans lists, per row, the run of cells from the first to the last
// that differ from key, widened so it never splits a wide grapheme.
func diffSpans(key, g *Grid) []span {
	var out []span
	for y := range g.H {
		a, b := key.Row(y), g.Row(y)
		x0 := 0
		for x0 < len(a) && a[x0] == b[x0] {
			x0++
		}
		if x0 == len(a) {
			continue
		}
		x1 := len(a)
		for x1 > x0 && a[x1-1] == b[x1-1] {
			x1--
		}
		for x0 > 0 && (a[x0].W == 0 || b[x0].W == 0) {
			x0--
		}
		for x1 < len(a) && (a[x1].W == 0 || b[x1].W == 0) {
			x1++
		}
		out = append(out, span{y, x0, x1})
	}
	return out
}

func encode(typ byte, keyID, n uint32, label string, g *Grid, spans []span) ([]byte, error) {
	var b []byte
	b = append(b, version, typ)
	b = binary.BigEndian.AppendUint32(b, keyID)
	b = binary.BigEndian.AppendUint32(b, n)
	b = binary.BigEndian.AppendUint16(b, uint16(g.W))
	b = binary.BigEndian.AppendUint16(b, uint16(g.H))
	b = append(b, byte(len(label)))
	b = append(b, label...)
	b = binary.BigEndian.AppendUint16(b, uint16(len(spans)))
	for _, s := range spans {
		b = appendSpan(b, g.Row(s.y), s)
	}
	var z bytes.Buffer
	w, err := newFlate(&z)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if z.Len() > MaxUpdate {
		return nil, ErrTooBig
	}
	return z.Bytes(), nil
}

func appendSpan(b []byte, row []Cell, s span) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(s.y))
	b = binary.BigEndian.AppendUint16(b, uint16(s.x0))
	b = binary.BigEndian.AppendUint16(b, uint16(s.x1-s.x0))
	nAt := len(b)
	b = append(b, 0, 0)
	runs := 0
	for x := s.x0; x < s.x1; {
		st := row[x].St
		var text []byte
		for x < s.x1 && row[x].St == st {
			if row[x].W > 0 {
				text = append(text, row[x].S...)
			}
			x++
		}
		b = append(b, byte(st.A&attrMask))
		if st.A&HasFG != 0 {
			b = append(b, byte(st.FG>>16), byte(st.FG>>8), byte(st.FG))
		}
		if st.A&HasBG != 0 {
			b = append(b, byte(st.BG>>16), byte(st.BG>>8), byte(st.BG))
		}
		b = binary.BigEndian.AppendUint16(b, uint16(len(text)))
		b = append(b, text...)
		runs++
	}
	binary.BigEndian.PutUint16(b[nAt:], uint16(runs))
	return b
}

// Decoder rebuilds a stream's grid from its updates.
type Decoder struct {
	key, cur *Grid
	keyID, n uint32
	touched  []int // rows of cur that differ from key's (the applied diff's)
	label    string
}

// Grid is the current picture (nil before the first keyframe). The
// caller must not change it.
func (d *Decoder) Grid() *Grid { return d.cur }

// Restart drops the picture but keeps the newest key id as a floor: the
// same stream restarted keeps raising its ids, so only a newer keyframe
// is taken (never one of the old stream, replayed).
func (d *Decoder) Restart() {
	d.key, d.cur, d.n, d.touched, d.label = nil, nil, 0, nil, ""
}

// Label is the pane's label, as the streamer named it (sanitized).
func (d *Decoder) Label() string { return d.label }

// Apply decodes one update. It reports whether the picture changed;
// updates that are stale (an older keyframe, a diff not newer than the
// applied one) or for a keyframe it never got are ignored.
func (d *Decoder) Apply(upd []byte) (bool, error) {
	if len(upd) > MaxUpdate {
		return false, ErrBadUpdate
	}
	body, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(upd)), maxBody+1))
	if err != nil || len(body) > maxBody {
		return false, ErrBadUpdate
	}
	r := reader{b: body}
	ver, typ := r.u8(), r.u8()
	keyID, n := r.u32(), r.u32()
	w, h := int(r.u16()), int(r.u16())
	label := string(r.bytes(int(r.u8())))
	nspans := int(r.u16())
	// At most one span per row, rows in order: a hostile update can't make
	// a viewer redraw a row more than once.
	if r.bad || ver != version || (typ != typeKey && typ != typeDiff) || w < 1 || h < 1 || w > MaxCols || h > MaxRows ||
		len(label) > MaxLabel || nspans > h || (typ == typeKey) != (n == 0) {
		return false, ErrBadUpdate
	}
	switch {
	case typ == typeKey && keyID <= d.keyID:
		return false, nil // a keyframe we have or one older
	case typ == typeDiff && (d.cur == nil || keyID != d.keyID || n <= d.n || w != d.key.W || h != d.key.H):
		return false, nil
	}
	spans := make([]decSpan, 0, nspans)
	for i := range nspans {
		s, ok := r.span(w, h)
		if !ok || i > 0 && s.y <= spans[i-1].y {
			return false, ErrBadUpdate
		}
		spans = append(spans, s)
	}
	if r.bad || r.left() != 0 {
		return false, ErrBadUpdate
	}
	if typ == typeKey {
		g := NewGrid(w, h)
		for _, s := range spans {
			s.apply(g)
		}
		d.key, d.cur, d.keyID, d.n, d.touched = g, g.Clone(), keyID, 0, nil
		d.label = sanitize.Line(label)
		return true, nil
	}
	for _, y := range d.touched {
		copy(d.cur.Row(y), d.key.Row(y))
	}
	d.touched = d.touched[:0]
	for _, s := range spans {
		s.apply(d.cur)
		d.touched = append(d.touched, s.y)
	}
	d.n = n
	return true, nil
}

type decRun struct {
	st   Style
	text string
}

type decSpan struct {
	y, x, cells int
	runs        []decRun
}

// apply lays s into g. Rows are fixed up so no wide grapheme is left half
// overwritten at either edge.
func (s decSpan) apply(g *Grid) {
	row := g.Row(s.y)
	x0, x1 := s.x, s.x+s.cells
	if x0 > 0 && row[x0].W == 0 {
		row[x0-1] = blank
	}
	if x1 < len(row) && row[x1].W == 0 {
		row[x1] = blank
	}
	for x := x0; x < x1; x++ {
		row[x] = blank
	}
	x := x0
	for _, r := range s.runs {
		cells := layText(nil, sanitize.Line(r.text), r.st, x1-x)
		copy(row[x:], cells)
		x += len(cells)
		if x >= x1 {
			break
		}
	}
}

type reader struct {
	b   []byte
	bad bool
}

func (r *reader) left() int { return len(r.b) }

func (r *reader) bytes(n int) []byte {
	if r.bad || n > len(r.b) {
		r.bad = true
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) u8() byte {
	if b := r.bytes(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *reader) u16() uint16 {
	if b := r.bytes(2); b != nil {
		return binary.BigEndian.Uint16(b)
	}
	return 0
}

func (r *reader) u24() uint32 {
	if b := r.bytes(3); b != nil {
		return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
	}
	return 0
}

func (r *reader) u32() uint32 {
	if b := r.bytes(4); b != nil {
		return binary.BigEndian.Uint32(b)
	}
	return 0
}

func (r *reader) span(w, h int) (decSpan, bool) {
	s := decSpan{y: int(r.u16()), x: int(r.u16()), cells: int(r.u16())}
	nruns := int(r.u16())
	if r.bad || s.y >= h || s.x >= w || s.cells < 1 || s.cells > w-s.x || nruns > s.cells {
		return s, false
	}
	s.runs = make([]decRun, 0, nruns)
	text := 0
	for range nruns {
		a := Attr(r.u8())
		if a&^attrMask != 0 {
			return s, false
		}
		st := Style{A: a}
		if a&HasFG != 0 {
			st.FG = r.u24()
		}
		if a&HasBG != 0 {
			st.BG = r.u24()
		}
		t := r.bytes(int(r.u16()))
		// No more text than the span's cells can hold: a hostile run
		// can't make the viewer sanitize kilobytes it will throw away.
		if text += len(t); r.bad || text > s.cells*maxCellBytes {
			return s, false
		}
		s.runs = append(s.runs, decRun{st, string(t)})
	}
	return s, true
}

// newFlate is the update compressor: speed over ratio, it runs up to 10
// times a second.
func newFlate(w io.Writer) (*flate.Writer, error) { return flate.NewWriter(w, flate.BestSpeed) }
