package client

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// markDownloaded quarantines a saved file the way browsers do, so macOS
// asks before anything in it runs (Gatekeeper). Best effort.
func markDownloaded(path string) {
	v := fmt.Sprintf("0081;%x;tc;", time.Now().Unix())
	_ = unix.Setxattr(path, "com.apple.quarantine", []byte(v), 0)
}
