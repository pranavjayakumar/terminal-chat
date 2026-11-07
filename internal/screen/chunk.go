package screen

import (
	"encoding/binary"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// Chunking (PROTOCOL.md §14.5): an update travels as up to
// proto.MaxScreenChunks datagrams, each carrying
//
//	u32 update id | u8 index | u8 count | data
//
// inside the (sealed) payload. Ids increase per stream; a resend of the
// same update gets a new id.

// Chunks splits upd into chunk payloads for update id.
func Chunks(id uint32, upd []byte) ([][]byte, error) {
	n := max((len(upd)+proto.ScreenChunkData-1)/proto.ScreenChunkData, 1)
	if n > proto.MaxScreenChunks {
		return nil, ErrTooBig
	}
	out := make([][]byte, n)
	for i := range n {
		part := upd[i*proto.ScreenChunkData : min((i+1)*proto.ScreenChunkData, len(upd))]
		c := make([]byte, proto.ScreenChunkHeader, proto.ScreenChunkHeader+len(part))
		binary.BigEndian.PutUint32(c, id)
		c[4], c[5] = byte(i), byte(n)
		out[i] = append(c, part...)
	}
	return out, nil
}

// Reassembly bounds: a viewer holds at most this many partial updates, for
// at most partialTTL each, so lost chunks or a hostile stream can't grow it.
const (
	maxPartials = 4
	partialTTL  = 5 * time.Second
	recentDone  = 16
)

type partial struct {
	id    uint32
	count int
	got   int
	parts [][]byte
	at    time.Time
}

// Reassembler collects one stream's chunks into updates.
type Reassembler struct {
	parts []*partial
	done  []uint32 // recently completed ids (late duplicates are ignored)
}

// Add takes one chunk payload; it returns the update once all its chunks
// arrived.
func (r *Reassembler) Add(c []byte, now time.Time) ([]byte, bool) {
	if len(c) <= proto.ScreenChunkHeader || len(c) > proto.ScreenChunkHeader+proto.ScreenChunkData {
		return nil, false
	}
	id := binary.BigEndian.Uint32(c)
	idx, count := int(c[4]), int(c[5])
	if count == 0 || idx >= count {
		return nil, false
	}
	for _, d := range r.done {
		if d == id {
			return nil, false
		}
	}
	r.expire(now)
	var p *partial
	for _, q := range r.parts {
		if q.id == id {
			p = q
		}
	}
	if p == nil {
		if len(r.parts) >= maxPartials {
			r.parts = r.parts[1:] // the oldest gives way
		}
		p = &partial{id: id, count: count, parts: make([][]byte, count), at: now}
		r.parts = append(r.parts, p)
	}
	if p.count != count || (idx < count-1 && len(c)-proto.ScreenChunkHeader != proto.ScreenChunkData) {
		r.drop(p) // inconsistent: not one update's chunks
		return nil, false
	}
	if p.parts[idx] == nil {
		p.parts[idx] = append([]byte(nil), c[proto.ScreenChunkHeader:]...)
		p.got++
	}
	if p.got < p.count {
		return nil, false
	}
	r.drop(p)
	if len(r.done) >= recentDone {
		r.done = r.done[1:]
	}
	r.done = append(r.done, id)
	n := 0
	for _, b := range p.parts {
		n += len(b)
	}
	upd := make([]byte, 0, n)
	for _, b := range p.parts {
		upd = append(upd, b...)
	}
	return upd, true
}

func (r *Reassembler) drop(p *partial) {
	for i, q := range r.parts {
		if q == p {
			r.parts = append(r.parts[:i], r.parts[i+1:]...)
			return
		}
	}
}

func (r *Reassembler) expire(now time.Time) {
	keep := r.parts[:0]
	for _, p := range r.parts {
		if now.Sub(p.at) < partialTTL {
			keep = append(keep, p)
		}
	}
	clear(r.parts[len(keep):])
	r.parts = keep
}
