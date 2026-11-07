package tui

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/media"
	"github.com/skailama/tc/internal/proto"
)

func testThumb(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	th, _, _, err := media.Thumbnail(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func fileOf(name string, size uint64, thumb []byte) proto.File {
	return proto.File{Blob: bytes.Repeat([]byte{1}, 16), Name: name, Size: size, Mime: "image/png",
		Key: make([]byte, 32), Hash: make([]byte, 32), Thumb: thumb, W: 640, H: 480}
}

// TestFileCardsFit: cards and previews never draw past the column, at any
// width, with or without color, with hostile names.
func TestFileCardsFit(t *testing.T) {
	wide := testThumb(t, 900, 200)
	tall := testThumb(t, 100, 900)
	names := []string{"cat.png", strings.Repeat("long-name-", 30) + ".png", "evil\x1b[2J‮.png", "你好世界你好世界你好世界.png", "../../etc/passwd"}
	for _, color := range []bool{true, false} {
		m := blockModel(color)
		for _, name := range names {
			files := []proto.File{fileOf(name, 1234567, wide), fileOf("b.bin", 3, nil), fileOf("t.png", 9, tall)}
			files[1].Blob = bytes.Repeat([]byte{2}, 16)
			files[2].Blob = bytes.Repeat([]byte{3}, 16)
			x := msg(1, 2, client.FallbackBody(files))
			x.Files = files
			for w := 1; w <= 130; w++ {
				for _, header := range []bool{true, false} {
					b, rows := block(m, x, w, header)
					if b.w != w {
						t.Fatalf("w=%d: block width %d", w, b.w)
					}
					for _, l := range rows {
						if lw := strWidth(l); lw > w {
							t.Fatalf("color=%v w=%d name=%q: row %q is %d wide", color, w, name, l, lw)
						}
						if strings.ContainsAny(l, "\x1b‮") {
							t.Fatalf("control character in %q", l)
						}
					}
				}
			}
		}
	}
	// At a usable width: the stand-in text is hidden, the card shows the
	// name and size, and a colored preview is drawn.
	m := blockModel(true)
	files := []proto.File{fileOf("cat.png", 1234567, wide)}
	x := msg(2, 2, client.FallbackBody(files))
	x.Files = files
	_, rows := block(m, x, 80, true)
	plain := strings.Join(rows, "\n")
	if strings.Contains(plain, "📎") || !strings.Contains(plain, "cat.png · 640×480 · 1.2 MB") || !strings.Contains(plain, "▀") {
		t.Fatalf("card:\n%s", plain)
	}
	// No color: the card without a preview.
	_, rows = block(blockModel(false), x, 80, true)
	if strings.Contains(strings.Join(rows, ""), "▀") {
		t.Fatal("preview without color")
	}
	// A caption shows above the card; a waiting message shows no files.
	x.Body, x.ID = "look at this", 3
	_, rows = block(m, x, 80, true)
	if rowWith(rows, "look at this") < 0 || rowWith(rows, "cat.png") < 0 {
		t.Fatalf("caption:\n%s", strings.Join(rows, "\n"))
	}
	x.Sec = client.SecWaiting
	_, rows = block(m, x, 80, true)
	if rowWith(rows, "cat.png") >= 0 {
		t.Fatal("waiting message shows files")
	}
	// A pending upload shows its progress.
	up := msg(0, 1, "f")
	up.Local, up.Pending, up.UpSent, up.UpTotal = -3, true, 40, 100
	up.Files = []proto.File{fileOf("big.bin", 100, nil)}
	_, rows = block(m, up, 80, true)
	if rowWith(rows, "uploading 40%") < 0 {
		t.Fatalf("progress:\n%s", strings.Join(rows, "\n"))
	}
}

// submitRun types text into the input and presses Enter, running every command.
func submitRun(m *Model, text string) {
	m.input.SetValue(text)
	press(m, "enter")
}

func TestUploadCommand(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	dir := t.TempDir()
	p := filepath.Join(dir, "my report.pdf")
	os.WriteFile(p, []byte("%PDF-1.4"), 0o600)
	submitRun(m, "/upload "+p+" have a look")
	if !f.called("sendfiles:" + p + "|have a look") {
		t.Fatalf("calls %v", f.calls)
	}
	submitRun(m, `/upload "`+p+`"`)
	if !f.called("sendfiles:" + p + "|") {
		t.Fatalf("quoted: calls %v", f.calls)
	}
	submitRun(m, "/upload "+filepath.Join(dir, "missing.txt"))
	if !strings.Contains(m.notice, "no such file") {
		t.Fatalf("missing file: %q", m.notice)
	}
	checkView(t, m, "upload")
}

func TestDroppedPathOffer(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a b.png")
	os.WriteFile(p, []byte("x"), 0o600)
	for _, sz := range writeSizes {
		for _, typed := range []string{p, strings.ReplaceAll(p, " ", `\ `), "'" + p + "'", "file://" + strings.ReplaceAll(p, " ", "%20")} {
			m, f := newTestModelFake(t, sz[0], sz[1])
			pasteUp(m, typed)
			if !m.attachArmed() {
				t.Fatalf("%v %q: no offer (%q)", sz, typed, m.notice)
			}
			checkView(t, m, "offer")
			press(m, "tab")
			if !f.called("sendfiles:"+p+"|") || m.input.Value() != "" {
				t.Fatalf("%q: calls %v input %q", typed, f.calls, m.input.Value())
			}
		}
	}
	// Not a file: no offer; typing withdraws it.
	m, f := newTestModelFake(t, 80, 24)
	pasteUp(m, filepath.Join(dir, "nope.png"))
	if m.attachArmed() {
		t.Fatal("offer for a missing file")
	}
	press(m, "ctrl+u")
	pasteUp(m, p)
	typeVia(m, "x")
	press(m, "tab")
	if m.attachArmed() || len(f.calls) > 0 && f.called("sendfiles:"+p+"|") {
		t.Fatal("offer kept after typing")
	}
}

func TestBigPasteOffer(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	big := strings.Repeat("log line\n", proto.MaxBody/9+10)
	pasteUp(m, big)
	if !m.attachArmed() || !strings.Contains(m.notice, "paste.txt") {
		t.Fatalf("no offer: %q", m.notice)
	}
	press(m, "tab")
	if m.input.Value() != "" {
		t.Fatal("paste left in the input")
	}
	found := false
	for _, c := range f.calls {
		found = found || strings.HasPrefix(c, "sendfiles:") && strings.Contains(c, "paste.txt")
	}
	if !found {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestSelectSaveOpen(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	onlySent(m, f)
	x := msg(500, 2, "📎 cat.png")
	x.Chan = 10
	x.Files = []proto.File{fileOf("cat.png", 10, nil)}
	f.msgs[10] = append(f.msgs[10], x)
	run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{10: true}}))
	press(m, "up")
	if f := m.selectFooter(); !strings.Contains(f, "s/o save/open") {
		t.Fatalf("footer %q", m.selectFooter())
	}
	press(m, "s")
	if !f.called("download:500:0") {
		t.Fatalf("calls %v", f.calls)
	}
	checkView(t, m, "saved")
	// o saves and, for a file that isn't what its name says, doesn't open it.
	if m.mode != modeSelect {
		press(m, "up")
	}
	press(m, "o")
	if !strings.Contains(m.notice, "saved") {
		t.Fatalf("open: %q", m.notice)
	}
}

// TestPasteOffersReview: what the TUI review found.
func TestPasteOffersReview(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("all:"), 0o600)
	// A pasted word that names a file in the working directory is text.
	m, _ := newTestModelFake(t, 80, 24)
	pasteUp(m, "Makefile")
	if m.attachArmed() {
		t.Fatal("offered a relative path")
	}
	// Keys are never offered, nor sent by /upload.
	key := filepath.Join(dir, "id_ed25519")
	os.WriteFile(key, []byte("secret"), 0o600)
	press(m, "ctrl+u")
	pasteUp(m, key)
	if m.attachArmed() {
		t.Fatal("offered a private key")
	}
	m2, f2 := newTestModelFake(t, 80, 24)
	submitRun(m2, "/upload "+key)
	if len(f2.calls) > 0 && strings.HasPrefix(f2.calls[len(f2.calls)-1], "sendfiles") || !strings.Contains(m2.notice, "key") {
		t.Fatalf("/upload a key: calls %v notice %q", f2.calls, m2.notice)
	}
	// After a big paste was ignored, a small one gets no offer.
	m3, _ := newTestModelFake(t, 80, 24)
	pasteUp(m3, strings.Repeat("x", proto.MaxBody+10))
	press(m3, "x")
	pasteUp(m3, "hello")
	if m3.attachArmed() {
		t.Fatalf("small paste offered: %q", m3.notice)
	}
	// Tabs survive into paste.txt (the input turns them into spaces).
	m4, _ := newTestModelFake(t, 80, 24)
	big := strings.Repeat("a\tb\n", proto.MaxBody/4+10)
	pasteUp(m4, big)
	if m4.wr.attachText != big {
		t.Fatal("paste.txt is not what was pasted")
	}
}

// TestSelectLettersWithoutFiles: s and o on a message without files leave
// select mode and are typed, as before.
func TestSelectLettersWithoutFiles(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	onlySent(m, f)
	press(m, "up", "s")
	if m.mode == modeSelect || !strings.HasSuffix(m.input.Value(), "s") {
		t.Fatalf("mode %v input %q", m.mode, m.input.Value())
	}
}

func TestUnopenable(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, b, 0o600)
		return p
	}
	png := testThumb(t, 10, 10) // a JPEG
	cases := map[string]bool{
		write("a.jpg", png):                         true,
		write("b.png", png):                         false, // a JPEG named .png
		write("c.txt", []byte("hello")):             true,
		write("d.command", []byte("rm -rf ~")):      false,
		write("e.app", []byte("x")):                 false,
		write("f.pdf", []byte("%PDF-1.4 x")):        true,
		write("g.txt", []byte{0, 1, 2, 0xff, 0xfe}): false,
		write("h.html", []byte("<html>")):           false,
		write("noext", []byte("x")):                 false,
	}
	for p, ok := range cases {
		if got := unopenable(p) == ""; got != ok {
			t.Errorf("%s: opens=%v, want %v (%s)", filepath.Base(p), got, ok, unopenable(p))
		}
	}
}
