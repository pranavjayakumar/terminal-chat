package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/release/releasetest"
)

func TestUpdateHomebrewDefersToBrew(t *testing.T) {
	srv := releasetest.New(t, "v0.3.0")
	srv.Publish(t, "v0.3.0", map[string][]byte{"linux_amd64": []byte("NEW")})
	for _, check := range []bool{false, true} {
		exe := filepath.Join(t.TempDir(), "Cellar", "tc", "0.2.0", "bin", "tc")
		if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(exe, []byte("OLD"), 0o755)
		var out bytes.Buffer
		if err := runUpdate(context.Background(), &out, srv.Source(), exe, "v0.2.0", "linux", "amd64", updateReq{check: check}); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(exe); string(b) != "OLD" {
			t.Fatalf("check=%v: brew-managed binary was replaced", check)
		}
		if !strings.Contains(out.String(), "brew upgrade tc") {
			t.Fatalf("check=%v output: %s", check, out.String())
		}
	}
}
