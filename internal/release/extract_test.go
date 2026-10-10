package release_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/skailama/tc/internal/release"
)

// bomb is a small .tar.gz whose first member, "README.md", decompresses to
// size zero bytes, followed by a real "tc".
func bomb(t *testing.T, size int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: size, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(tw, zeros{}, size); err != nil {
		t.Fatal(err)
	}
	tw.WriteHeader(&tar.Header{Name: "tc", Mode: 0o755, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("TC"))
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// Skipping an oversized non-tc member stops at the decompression cap
// instead of inflating it all.
func TestExtractBoundsDecompression(t *testing.T) {
	ctx := context.Background()
	if b, err := release.Extract(ctx, bomb(t, 1<<20)); err != nil || string(b) != "TC" {
		t.Fatalf("small archive: %q %v", b, err)
	}
	arc := bomb(t, release.MaxUnpacked+1<<20)
	if len(arc) > 1<<20 {
		t.Fatalf("test bomb is %d bytes; meant to be small", len(arc))
	}
	if b, err := release.Extract(ctx, arc); err == nil {
		t.Fatalf("bomb extracted (%d bytes)", len(b))
	}
}

// A cancelled context stops the unpacking.
func TestExtractHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := release.Extract(ctx, bomb(t, 64<<20))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancelled extract kept working")
	}
}
