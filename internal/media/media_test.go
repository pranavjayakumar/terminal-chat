package media

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"strings"
	"testing"
)

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"cat.png":                    "cat.png",
		"../../etc/passwd":           "passwd",
		`..\..\win.ini`:              "win.ini",
		"/abs/path/x.txt":            "x.txt",
		".bashrc":                    "bashrc",
		"-rf":                        "rf",
		"a\x1b[31mred\x07.txt":       "a[31mred.txt",
		"evil‮gnp.exe":               "evilgnp.exe",
		"zero​width":                 "zerowidth",
		"":                           "file",
		"..":                         "file",
		"   ":                        "file",
		"con.txt":                    "_con.txt",
		"a:b|c?.txt":                 "a_b_c_.txt",
		"tab\tand\nnewline.md":       "tab and newline.md",
		"trailing dots...":           "trailing dots",
		"private.txt":               "private.txt",
		"emoji 🎉.png":                "emoji 🎉.png",
		"ｆｕｌｌｗｉｄｔｈ.txt":              "ｆｕｌｌｗｉｄｔｈ.txt",
		"\xff\xfebad utf8.bin":       "bad utf8.bin",
		"x/../../y/.hidden/../z.txt": "z.txt",
	}
	for in, want := range cases {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 300) + ".tar.gz"
	got := SafeName(long)
	if len(got) > 200 || !strings.HasSuffix(got, ".gz") || !strings.HasPrefix(got, "é") {
		t.Errorf("long name: %d bytes %q", len(got), got[len(got)-10:])
	}
}

func TestSniff(t *testing.T) {
	pngBytes := encodePNG(t, 4, 4)
	cases := []struct {
		head []byte
		name string
		want string
	}{
		{pngBytes, "x.txt", "image/png"},
		{[]byte("hello world"), "notes.md", "text/markdown"},
		{[]byte("hello world"), "a.exe", "text/plain"},
		{[]byte{0, 1, 2, 3, 0xff}, "a.json", "application/octet-stream"},
		{[]byte("<html><script>"), "x.png", "text/html"},
		{[]byte("%PDF-1.4"), "doc", "application/pdf"},
	}
	for _, c := range cases {
		if got := Sniff(c.head, c.name); got != c.want {
			t.Errorf("Sniff(%q, %q) = %q, want %q", c.head[:min(8, len(c.head))], c.name, got, c.want)
		}
	}
}

func encodePNG(t *testing.T, w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewSource(1))
	for i := range img.Pix {
		img.Pix[i] = byte(r.Intn(256))
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestThumbnail(t *testing.T) {
	// Noise is the worst case for JPEG size.
	th, w, h, err := Thumbnail(encodePNG(t, 1200, 800))
	if err != nil {
		t.Fatal(err)
	}
	if w != 1200 || h != 800 || len(th) > MaxThumb || len(th) == 0 {
		t.Fatalf("w=%d h=%d len=%d", w, h, len(th))
	}
	img, err := Decode(th)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() > ThumbSide || b.Dy() > ThumbSide || b.Dx() < ThumbSide/2 {
		t.Fatalf("thumb %v", b)
	}
	// Tiny images aren't scaled up; transparency lands on white.
	tiny := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	tiny.Set(0, 0, color.NRGBA{0, 0, 0, 0})
	var buf bytes.Buffer
	png.Encode(&buf, tiny)
	th, _, _, err = Thumbnail(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	img, _ = Decode(th)
	if img.Bounds().Dx() != 3 {
		t.Fatalf("tiny scaled to %v", img.Bounds())
	}
	if r, _, _, _ := img.At(0, 0).RGBA(); r < 0xe000 {
		t.Fatalf("transparent pixel not white: %x", r)
	}
}

// TestPixelBomb: a header claiming a gigantic image is refused before any
// pixel buffer is allocated.
func TestPixelBomb(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 100000)
	binary.BigEndian.PutUint32(ihdr[4:], 100000)
	ihdr[8], ihdr[9] = 8, 6
	chunk := func(typ string, data []byte) {
		binary.Write(&b, binary.BigEndian, uint32(len(data)))
		b.WriteString(typ)
		b.Write(data)
		binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	}
	chunk("IHDR", ihdr)
	chunk("IEND", nil)
	if _, _, _, err := Thumbnail(b.Bytes()); err != ErrTooManyPixels {
		t.Fatalf("bomb: %v", err)
	}
	if _, err := Decode([]byte("not an image")); err != ErrNotImage {
		t.Fatalf("garbage: %v", err)
	}
}

func TestSize(t *testing.T) {
	for n, want := range map[uint64]string{0: "0 B", 999: "999 B", 1000: "1.0 KB", 1234: "1.2 KB", 12345: "12 KB",
		25 << 20: "26 MB", 1_500_000_000: "1.5 GB"} {
		if got := Size(n); got != want {
			t.Errorf("Size(%d) = %q, want %q", n, got, want)
		}
	}
}

func FuzzThumbnail(f *testing.F) {
	f.Add([]byte("\x89PNG\r\n\x1a\n"))
	f.Add([]byte("GIF89a"))
	f.Fuzz(func(t *testing.T, b []byte) {
		th, _, _, err := Thumbnail(b)
		if err == nil && len(th) > MaxThumb {
			t.Fatal("thumbnail over the limit")
		}
	})
}
