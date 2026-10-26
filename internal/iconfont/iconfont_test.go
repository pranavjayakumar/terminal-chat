package iconfont

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The committed font is what scripts/iconfont.py makes from icons.txt.
// Needs python3 with fontTools (TC_PYTHON picks the interpreter); skipped
// without it.
func TestFontMatchesSource(t *testing.T) {
	py := os.Getenv("TC_PYTHON")
	if py == "" {
		py = "python3"
	}
	if exec.Command(py, "-c", "import fontTools").Run() != nil {
		t.Skip("python3 with fontTools not available (pip install fonttools, or set TC_PYTHON)")
	}
	out := filepath.Join(t.TempDir(), File)
	cmd := exec.Command(py, "../../scripts/iconfont.py", "../../assets/iconfont/icons.txt", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generator: %v\n%s", err, b)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, TTF) {
		t.Fatal("internal/iconfont/tc-symbols.ttf is stale: rerun scripts/iconfont.py (see its header)")
	}
}

// The font maps exactly First..Last minus the gaps in icons.txt, all
// inside the range Nerd Fonts leave free.
func TestRange(t *testing.T) {
	if First != 0xE400 || Last != 0xE41B || First <= 0xE3E3 || Last >= 0xE5FA {
		t.Fatal("tc Symbols must stay between Nerd Fonts' Weather (to U+E3E3) and Seti-UI (from U+E5FA)")
	}
	if len(TTF) < 1000 || string(TTF[:4]) != "\x00\x01\x00\x00" {
		t.Fatal("embedded font is not a TrueType file")
	}
}

func TestInstallStatusRefreshRemove(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no per-user font folder here")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("PATH", "") // no fc-cache in the test
	if s, _, err := Status(); err != nil || s != Missing || Installed() {
		t.Fatalf("fresh: %v %v", s, err)
	}
	if did, err := Refresh(); did || err != nil {
		t.Fatal("refresh installed a missing font")
	}
	p, err := Install()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "share", "fonts", File)
	if runtime.GOOS == "darwin" {
		want = filepath.Join(home, "Library", "Fonts", File)
	}
	if p != want {
		t.Fatalf("installed at %s, want %s", p, want)
	}
	if s, _, _ := Status(); s != Current {
		t.Fatalf("after install: %v", s)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode())
	}
	os.WriteFile(p, []byte("old build"), 0o644)
	if s, _, _ := Status(); s != Stale {
		t.Fatal("a different file isn't stale")
	}
	if did, err := Refresh(); !did || err != nil {
		t.Fatal("refresh didn't replace a stale font")
	}
	if s, _, _ := Status(); s != Current {
		t.Fatal("still stale")
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}
	if _, existed, err := Remove(); !existed || err != nil {
		t.Fatal(err)
	}
	if _, existed, _ := Remove(); existed {
		t.Fatal("removed twice")
	}
}
