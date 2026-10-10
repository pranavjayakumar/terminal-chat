// Command verifyrelease checks a signed checksums.txt exactly as tc update
// does, against the embedded release.AllowedSigners. make release-sign runs
// it before publishing.
//
//	go run ./internal/release/verifyrelease checksums.txt checksums.txt.sig
package main

import (
	"fmt"
	"os"

	"github.com/skailama/tc/internal/release"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: verifyrelease checksums.txt checksums.txt.sig")
		os.Exit(2)
	}
	msg, err := os.ReadFile(os.Args[1])
	if err == nil {
		var sig []byte
		if sig, err = os.ReadFile(os.Args[2]); err == nil {
			err = release.VerifySignature(release.AllowedSigners, msg, sig)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "verifyrelease:", err)
		os.Exit(1)
	}
	fmt.Println("signature OK: tc update and install.sh will accept this release")
}
