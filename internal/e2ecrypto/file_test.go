package e2ecrypto

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
)

func sealBytes(t *testing.T, fk Key, pt []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	n, sum, err := SealFile(&buf, bytes.NewReader(pt), fk, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(pt)) || sum != sha256.Sum256(pt) {
		t.Fatalf("seal: n=%d sum mismatch", n)
	}
	if int64(buf.Len()) != FileSealedSize(int64(len(pt))) {
		t.Fatalf("sealed %d bytes, FileSealedSize says %d", buf.Len(), FileSealedSize(int64(len(pt))))
	}
	if p, ok := FilePlainSize(int64(buf.Len())); !ok || p != int64(len(pt)) {
		t.Fatalf("FilePlainSize(%d) = %d, %v; want %d", buf.Len(), p, ok, len(pt))
	}
	return buf.Bytes()
}

func TestFileRoundTrip(t *testing.T) {
	var fk Key
	rand.Read(fk[:])
	for _, size := range []int{0, 1, FileChunk - 1, FileChunk, FileChunk + 1, 2 * FileChunk, 3*FileChunk + 17} {
		pt := make([]byte, size)
		rand.Read(pt)
		ct := sealBytes(t, fk, pt)
		var out bytes.Buffer
		n, sum, err := OpenFile(&out, bytes.NewReader(ct), fk, 1<<30)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if n != int64(size) || !bytes.Equal(out.Bytes(), pt) || sum != sha256.Sum256(pt) {
			t.Fatalf("size %d: round trip differs", size)
		}
	}
}

func TestFilePlainSizeRejects(t *testing.T) {
	for _, s := range []int64{0, 15, FileChunk + TagSize + TagSize, 2*(FileChunk+TagSize) + 5} {
		if _, ok := FilePlainSize(s); ok {
			t.Errorf("FilePlainSize(%d) ok, want not", s)
		}
	}
}

// TestFileTamper: what a server can do to a stored blob is caught.
func TestFileTamper(t *testing.T) {
	var fk, other Key
	rand.Read(fk[:])
	rand.Read(other[:])
	pt := make([]byte, 3*FileChunk+100)
	rand.Read(pt)
	ct := sealBytes(t, fk, pt)
	chunk := FileChunk + TagSize
	cases := map[string][]byte{
		"flip a bit":           func() []byte { b := bytes.Clone(ct); b[chunk+5] ^= 1; return b }(),
		"drop the last chunk":  ct[:3*chunk],
		"cut mid chunk":        ct[:2*chunk+100],
		"cut to one tag":       ct[:TagSize],
		"empty":                nil,
		"append bytes":         append(bytes.Clone(ct), 1, 2, 3),
		"append a copy":        append(bytes.Clone(ct), ct[3*chunk:]...),
		"swap two chunks":      append(append(append(bytes.Clone(ct[chunk:2*chunk]), ct[:chunk]...), ct[2*chunk:]...)),
		"repeat a chunk":       append(append(bytes.Clone(ct[:chunk]), ct[:chunk]...), ct[2*chunk:]...),
		"other file's blob":    sealBytes(t, other, pt),
		"same key, other file": sealBytes(t, fk, pt[:FileChunk*2]),
	}
	for name, bad := range cases {
		var out bytes.Buffer
		_, sum, err := OpenFile(&out, bytes.NewReader(bad), fk, 1<<30)
		if name == "same key, other file" {
			// A key is used for one file only; if a sender reused one, the
			// hash in the message still tells the files apart.
			if err == nil && sum == sha256.Sum256(pt) {
				t.Errorf("%s: accepted as the original", name)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: opened without error", name)
		}
	}
	// Truncation exactly at a chunk boundary says so.
	if _, _, err := OpenFile(&bytes.Buffer{}, bytes.NewReader(ct[:2*chunk]), fk, 1<<30); !errors.Is(err, ErrFileTruncated) {
		t.Errorf("cut at a boundary: %v, want ErrFileTruncated", err)
	}
}

func TestFileMax(t *testing.T) {
	var fk Key
	pt := make([]byte, 2*FileChunk)
	if _, _, err := SealFile(&bytes.Buffer{}, bytes.NewReader(pt), fk, FileChunk); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("seal over max: %v", err)
	}
	ct := sealBytes(t, fk, pt)
	if _, _, err := OpenFile(&bytes.Buffer{}, bytes.NewReader(ct), fk, FileChunk); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("open over max: %v", err)
	}
}

func FuzzOpenFile(f *testing.F) {
	var fk Key
	f.Add(sealBytesF(fk, []byte("hello")))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, ct []byte) {
		_, _, _ = OpenFile(&bytes.Buffer{}, bytes.NewReader(ct), fk, 1<<20)
	})
}

func sealBytesF(fk Key, pt []byte) []byte {
	var buf bytes.Buffer
	SealFile(&buf, bytes.NewReader(pt), fk, 1<<30)
	return buf.Bytes()
}
