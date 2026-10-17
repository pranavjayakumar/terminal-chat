// Command verifyrelease checks a signed checksums.txt exactly as tc update
// does, against the embedded release.AllowedSigners. make release-sign runs
// it before publishing.
//
//	go run ./internal/release/verifyrelease checksums.txt checksums.txt.sig [--sums]
//
// --sums then prints "<os>_<arch> <sha256>" for every supported build, from
// the verified file, and fails if any is missing (scripts/brew-formula.sh).
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/skailama/tc/internal/release"
)

func main() {
	sums := len(os.Args) == 4 && os.Args[3] == "--sums"
	if len(os.Args) != 3 && !sums {
		fmt.Fprintln(os.Stderr, "usage: verifyrelease checksums.txt checksums.txt.sig [--sums]")
		os.Exit(2)
	}
	msg, err := os.ReadFile(os.Args[1])
	if err == nil {
		var sig []byte
		if sig, err = os.ReadFile(os.Args[2]); err == nil {
			err = release.VerifySignature(release.AllowedSigners, msg, sig)
		}
	}
	if err == nil && sums {
		m := release.ParseChecksums(msg)
		var lines []string
		for _, t := range [][2]string{{"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "arm64"}, {"linux", "amd64"}} {
			h, ok := m[release.AssetName(t[0], t[1])]
			if !ok {
				err = fmt.Errorf("no checksum for %s", release.AssetName(t[0], t[1]))
				break
			}
			lines = append(lines, t[0]+"_"+t[1]+" "+h)
		}
		if err == nil {
			fmt.Println(strings.Join(lines, "\n"))
		}
		os.Exit(exit(err))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "verifyrelease:", err)
		os.Exit(1)
	}
	fmt.Println("signature OK: tc update and install.sh will accept this release")
}

func exit(err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "verifyrelease:", err)
		return 1
	}
	return 0
}
