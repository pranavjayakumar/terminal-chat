//go:build !tctamper

package server

import "github.com/skailama/tc/internal/proto"

// TamperBuild reports whether this binary was built with the tctamper tag,
// which compiles in the malicious-server hooks of tamper_on.go (tests only).
// Production builds never set the tag, so they contain no hook at all: the
// stubs below are empty and tamperEnabled lets the compiler drop the calls.
const TamperBuild = false

const tamperEnabled = false

type tamperState struct{}

func (*tamperState) welcome(uint64, *proto.Welcome) {}

func (*tamperState) out(_ uint64, f outFrame) []outFrame { return []outFrame{f} }
