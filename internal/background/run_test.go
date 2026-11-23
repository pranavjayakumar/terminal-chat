package background

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A login written after the failed session but before waitConfig starts
// (the "waiting" status is already on disk, so tc join may write right
// then) must wake the notifier, not become the baseline it waits past.
func TestWaitConfigSeesLoginFromBeforeItStarted(t *testing.T) {
	home := t.TempDir()
	r := &runner{cfg: Config{Home: home, Poll: 10 * time.Millisecond}}
	login := statOf(filepath.Join(home, "config.json"))
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.waitConfig(ctx, login); err != nil {
		t.Fatalf("waitConfig = %v, want the login written before it started", err)
	}
}
