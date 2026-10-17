package voice

// window is a 64-frame sliding replay window over a stream's seq
// (PROTOCOL.md §14.3), DTLS/IPsec style. Callers check before decrypting
// and accept only after the AEAD opened, so forgeries can't move it.
type window struct {
	top  uint32 // highest accepted seq
	bits uint64 // bit i = top-i was accepted
	init bool
}

const windowSize = 64

func (w *window) check(seq uint32) bool {
	if !w.init || seq > w.top {
		return true
	}
	d := w.top - seq
	return d < windowSize && w.bits&(1<<d) == 0
}

func (w *window) accept(seq uint32) {
	switch {
	case !w.init:
		w.init, w.top, w.bits = true, seq, 1
	case seq > w.top:
		d := seq - w.top
		if d >= windowSize {
			w.bits = 1
		} else {
			w.bits = w.bits<<d | 1
		}
		w.top = seq
	default:
		w.bits |= 1 << (w.top - seq)
	}
}
