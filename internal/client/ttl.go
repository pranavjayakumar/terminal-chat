package client

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// ParseTTL reads an invite lifetime like 72h, 30m or 3d (days, 1-30), the
// same in the TUI and the CLI.
func ParseTTL(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 1 || days > 30 {
			return 0, errors.New("days must be a whole number 1-30 (like 3d), or use hours like 72h")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
