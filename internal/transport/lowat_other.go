//go:build !linux && !darwin

package transport

import "syscall"

func limitUnsent(_, _ string, _ syscall.RawConn) error { return nil }
