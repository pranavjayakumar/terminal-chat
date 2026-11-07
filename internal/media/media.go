// Package media looks at files people send (PROTOCOL.md §16): it sniffs
// their type from their bytes, makes image thumbnails, and turns a sent
// file name into one that is safe to show and to save. It is pure: no
// network, no tc imports, and it never decodes an image whose header says
// it is bigger than MaxPixels.
package media

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxPixels is the largest image (width × height) tc decodes: a 40 MP
// header can't make it allocate gigabytes.
const MaxPixels = 40_000_000

// MaxThumb is the largest thumbnail Thumbnail returns (proto.MaxThumb).
const MaxThumb = 32 << 10

// ThumbSide is the longest side of a thumbnail, in pixels.
const ThumbSide = 320

// SniffLen is how many leading bytes Sniff looks at.
const SniffLen = 512

var (
	// ErrNotImage: the bytes aren't a PNG, JPEG or GIF.
	ErrNotImage = errors.New("not an image tc can preview")
	// ErrTooManyPixels: the image's header claims more than MaxPixels.
	ErrTooManyPixels = errors.New("image too large to preview")
)

// Sniff names a file's type from its first bytes (head, up to SniffLen)
// and, only for types its bytes can't tell, its name. The result is a bare
// type ("image/png", no parameters) and is advisory: receivers sniff
// again, and nothing ever runs a file because of its type.
func Sniff(head []byte, name string) string {
	t := http.DetectContentType(head)
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimSpace(t)
	if t != "application/octet-stream" && t != "text/plain" {
		return t
	}
	// Text whose bytes don't say more: a few plain formats by extension,
	// never one that could be executable or active content.
	if ext, ok := byExt[strings.ToLower(filepath.Ext(name))]; ok && t == "text/plain" {
		return ext
	}
	return t
}

var byExt = map[string]string{
	".md": "text/markdown", ".csv": "text/csv", ".json": "application/json", ".log": "text/plain",
	".txt": "text/plain", ".diff": "text/x-diff", ".patch": "text/x-diff", ".yaml": "text/yaml", ".yml": "text/yaml",
	".toml": "text/plain", ".go": "text/plain",
}

// IsImage reports whether mime is a type Thumbnail and the TUI preview
// decode (PNG, JPEG, GIF).
func IsImage(mime string) bool {
	return mime == "image/png" || mime == "image/jpeg" || mime == "image/gif"
}

// ImageSize reads an image's header and returns its size, failing for a
// non-image or one over MaxPixels.
func ImageSize(r io.Reader) (w, h int, format string, err error) {
	cfg, format, err := image.DecodeConfig(r)
	if err != nil {
		return 0, 0, "", ErrNotImage
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return cfg.Width, cfg.Height, format, ErrTooManyPixels
	}
	return cfg.Width, cfg.Height, format, nil
}

// Decode decodes a PNG, JPEG or GIF (first frame) after checking its
// header against MaxPixels.
func Decode(b []byte) (image.Image, error) {
	if _, _, _, err := ImageSize(bytes.NewReader(b)); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, ErrNotImage
	}
	return img, nil
}

// MaxThumbPixels bounds a received thumbnail: tc makes them at most
// ThumbSide a side, and a hostile 32 KiB one mustn't decode to gigabytes.
const MaxThumbPixels = 1 << 20

// DecodeThumb decodes a received thumbnail (PNG, JPEG or GIF) of at most
// MaxThumbPixels.
func DecodeThumb(b []byte) (image.Image, error) {
	w, h, _, err := ImageSize(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if w*h > MaxThumbPixels {
		return nil, ErrTooManyPixels
	}
	return Decode(b)
}

// Thumbnail makes a JPEG of at most MaxThumb bytes and ThumbSide pixels a
// side from an image file's bytes, and returns the image's own size.
func Thumbnail(b []byte) (thumb []byte, w, h int, err error) {
	img, err := Decode(b)
	if err != nil {
		return nil, 0, 0, err
	}
	w, h = img.Bounds().Dx(), img.Bounds().Dy()
	for _, side := range []int{ThumbSide, 240, 160, 96} {
		small := Fit(img, side, side)
		for _, q := range []int{70, 50, 35} {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: q}); err != nil {
				return nil, 0, 0, err
			}
			if buf.Len() <= MaxThumb {
				return buf.Bytes(), w, h, nil
			}
		}
	}
	return nil, w, h, errors.New("thumbnail too large")
}

// Fit scales img down (never up) to fit maxW × maxH onto an opaque white
// background, averaging up to 4×4 samples of the source block under each
// target pixel (no full-size copy of a large source).
func Fit(img image.Image, maxW, maxH int) *image.RGBA {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dw, dh := sw, sh
	if dw > maxW {
		dw, dh = maxW, max(1, sh*maxW/sw)
	}
	if dh > maxH {
		dw, dh = max(1, dw*maxH/dh), maxH
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		y0, y1 := y*sh/dh, max((y+1)*sh/dh, y*sh/dh+1)
		ky := min(4, y1-y0)
		for x := 0; x < dw; x++ {
			x0, x1 := x*sw/dw, max((x+1)*sw/dw, x*sw/dw+1)
			kx := min(4, x1-x0)
			var r, g, bl, n uint32
			for i := 0; i < ky; i++ {
				sy := b.Min.Y + y0 + (2*i+1)*(y1-y0)/(2*ky)
				for j := 0; j < kx; j++ {
					sx := b.Min.X + x0 + (2*j+1)*(x1-x0)/(2*kx)
					c := color.RGBAModel.Convert(img.At(sx, sy)).(color.RGBA) // premultiplied
					a := 0xff - uint32(c.A)                                   // over white
					r, g, bl, n = r+uint32(c.R)+a, g+uint32(c.G)+a, bl+uint32(c.B)+a, n+1
				}
			}
			o := dst.Pix[y*dst.Stride+x*4:]
			o[0], o[1], o[2], o[3] = uint8(r/n), uint8(g/n), uint8(bl/n), 0xff
		}
	}
	return dst
}

// PNG encodes img as PNG (for terminals that take only PNG).
func PNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, img)
	return buf.Bytes(), err
}

var _ = gif.Decode // registers GIF with image.Decode

// SafeName turns a sent file name into one that is safe to show and to
// save: no directories, no control, bidi or invisible characters, no
// leading dots or dashes, at most maxName bytes, never empty.
func SafeName(name string) string {
	const maxName = 200
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteRune(' ')
		case r == utf8.RuneError, unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r),
			unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r), invisible(r):
			// dropped: controls, format (bidi, zero-width), private use,
			// invisible and unassigned characters (a filesystem may refuse
			// the latter: APFS says "illegal byte sequence")
		case strings.ContainsRune(`<>:"|?*`, r):
			b.WriteRune('_')
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	name = strings.Join(strings.Fields(b.String()), " ")
	name = strings.TrimLeft(name, ".- ")
	name = strings.TrimRight(name, ". ")
	if len(name) > maxName {
		ext := filepath.Ext(name)
		if len(ext) > 16 || !utf8.ValidString(ext) {
			ext = ""
		}
		cut := maxName - len(ext)
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = strings.TrimRight(name[:cut], ". ") + ext
	}
	if name == "" {
		return "file"
	}
	if reserved[strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))] {
		name = "_" + name
	}
	return name
}

// invisible: characters that draw nothing (default-ignorable code points,
// variation selectors, Hangul fillers, the braille blank) or that no
// Unicode version tc knows assigns.
func invisible(r rune) bool {
	switch {
	case unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r), unicode.Is(unicode.Variation_Selector, r),
		r == 0x034F, r >= 0x180B && r <= 0x180F, r == 0x17B4, r == 0x17B5, r == 0x2800,
		r == 0x115F, r == 0x1160, r == 0x3164, r == 0xFFA0:
		return true
	}
	return !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Zs)
}

// Windows device names, in case the file travels on.
var reserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "LPT1": true, "LPT2": true, "LPT3": true}

// Size formats a byte count for people: 999 B, 1.2 KB, 34 MB, 1.1 GB.
func Size(n uint64) string {
	switch {
	case n < 1000:
		return itoa(n) + " B"
	case n < 1000*1000:
		return dec(n, 1000) + " KB"
	case n < 1000*1000*1000:
		return dec(n, 1000*1000) + " MB"
	}
	return dec(n, 1000*1000*1000) + " GB"
}

func dec(n, unit uint64) string {
	if n >= 10*unit {
		return itoa((n + unit/2) / unit)
	}
	t := (n*10 + unit/2) / unit
	return itoa(t/10) + "." + itoa(t%10)
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
