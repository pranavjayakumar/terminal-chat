package e2ecrypto

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// File encryption (PROTOCOL.md §16.3): every file is sealed with its own
// random key fk, in a chunked AEAD stream so a server that truncates,
// extends, reorders or swaps ciphertext is caught:
//
//	n       = max(1, ceil(size / FileChunk))
//	nonce_i = 0^15 || i (u64 BE) || final (1 for the last chunk, else 0)
//	ct_i    = XChaCha20-Poly1305(fk, nonce_i, pt_i, AD = "tc-file-v1")
//	blob    = ct_0 || ct_1 || ... || ct_{n-1}
//
// fk is never reused (one key, one file), so the counter nonce is safe. The
// key, name, size, type and SHA-256 of the plaintext travel in the message
// that attaches the file; in an encrypted channel that message is sealed
// and signed (§13.4).

// FileChunk is the plaintext size of every chunk but the last.
const FileChunk = 64 << 10

const fileAD = "tc-file-v1"

// ErrFileTruncated: the stream ended before its final chunk, or went on after it.
var ErrFileTruncated = errors.New("e2ecrypto: file truncated or extended")

// FileSealedSize is the blob size of a size-byte file.
func FileSealedSize(size int64) int64 {
	n := (size + FileChunk - 1) / FileChunk
	if n == 0 {
		n = 1
	}
	return size + n*TagSize
}

// FilePlainSize inverts FileSealedSize; ok is false for a size no file seals to.
func FilePlainSize(sealed int64) (int64, bool) {
	if sealed < TagSize {
		return 0, false
	}
	full := sealed / (FileChunk + TagSize)
	rest := sealed % (FileChunk + TagSize)
	switch {
	case rest == 0 && full > 0:
		return full * FileChunk, true
	case rest >= TagSize && (rest > TagSize || full == 0):
		return full*FileChunk + rest - TagSize, true
	}
	return 0, false
}

func fileNonce(i uint64, final bool) []byte {
	var n [NonceSize]byte
	binary.BigEndian.PutUint64(n[15:23], i)
	if final {
		n[23] = 1
	}
	return n[:]
}

// SealFile reads r to EOF and writes its sealed stream to w. It returns the
// plaintext size and SHA-256. max bounds the plaintext (ErrTooLarge beyond).
func SealFile(w io.Writer, r io.Reader, fk Key, max int64) (int64, [32]byte, error) {
	var sum [32]byte
	aead, err := chacha20poly1305.NewX(fk[:])
	if err != nil {
		return 0, sum, err
	}
	h := sha256.New()
	br := bufio.NewReaderSize(r, FileChunk)
	pt := make([]byte, FileChunk)
	ct := make([]byte, 0, FileChunk+TagSize)
	var total int64
	for i := uint64(0); ; i++ {
		n, err := io.ReadFull(br, pt)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, sum, err
		}
		final := n < FileChunk
		if !final {
			if _, perr := br.Peek(1); errors.Is(perr, io.EOF) {
				final = true
			} else if perr != nil {
				return 0, sum, perr
			}
		}
		total += int64(n)
		if total > max {
			return 0, sum, ErrTooLarge
		}
		h.Write(pt[:n])
		ct = aead.Seal(ct[:0], fileNonce(i, final), pt[:n], []byte(fileAD))
		if _, err := w.Write(ct); err != nil {
			return 0, sum, err
		}
		if final {
			copy(sum[:], h.Sum(nil))
			return total, sum, nil
		}
	}
}

// OpenFile reads a sealed stream from r to EOF and writes the plaintext to
// w. It fails on any chunk that doesn't open in its place, a stream that
// stops before its final chunk or goes on after it, or more than max
// plaintext bytes. Plaintext of chunks that opened may already be in w
// when it fails: write to a temporary file and discard it on error.
func OpenFile(w io.Writer, r io.Reader, fk Key, max int64) (int64, [32]byte, error) {
	var sum [32]byte
	aead, err := chacha20poly1305.NewX(fk[:])
	if err != nil {
		return 0, sum, err
	}
	var h hash.Hash = sha256.New()
	br := bufio.NewReaderSize(r, FileChunk+TagSize)
	ct := make([]byte, FileChunk+TagSize)
	pt := make([]byte, 0, FileChunk)
	var total int64
	for i := uint64(0); ; i++ {
		n, err := io.ReadFull(br, ct)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, sum, err
		}
		if n < TagSize {
			return 0, sum, ErrFileTruncated
		}
		final := n < len(ct)
		if !final {
			if _, perr := br.Peek(1); errors.Is(perr, io.EOF) {
				final = true
			} else if perr != nil {
				return 0, sum, perr
			}
		}
		pt, err = aead.Open(pt[:0], fileNonce(i, final), ct[:n], []byte(fileAD))
		if err != nil {
			if final {
				// The last chunk we got isn't marked final: was there more?
				if _, err2 := aead.Open(nil, fileNonce(i, false), ct[:n], []byte(fileAD)); err2 == nil {
					return 0, sum, ErrFileTruncated
				}
			} else if _, err2 := aead.Open(nil, fileNonce(i, true), ct[:n], []byte(fileAD)); err2 == nil {
				return 0, sum, ErrFileTruncated // final chunk, then more bytes
			}
			return 0, sum, ErrDecrypt
		}
		total += int64(len(pt))
		if total > max {
			return 0, sum, ErrTooLarge
		}
		h.Write(pt)
		if _, err := w.Write(pt); err != nil {
			return 0, sum, err
		}
		if final {
			copy(sum[:], h.Sum(nil))
			return total, sum, nil
		}
	}
}
