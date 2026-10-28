// Command verifyrelease checks a signed checksums.txt exactly as tc update
// does, against the embedded release.AllowedSigners, including that its
// "version" line names TAG (release.CheckVersionLine). make release-sign
// runs it before publishing.
//
//	go run ./internal/release/verifyrelease --tag vX.Y.Z checksums.txt checksums.txt.sig [--sums]
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

const usage = "usage: verifyrelease --tag vX.Y.Z checksums.txt checksums.txt.sig [--sums]"

func main() {
	args := os.Args[1:]
	sums := len(args) == 5 && args[4] == "--sums"
	if (len(args) != 4 && !sums) || args[0] != "--tag" || !release.ValidTag(args[1]) {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	tag := args[1]
	msg, err := os.ReadFile(args[2])
	if err == nil {
		var sig []byte
		if sig, err = os.ReadFile(args[3]); err == nil {
			err = release.VerifySignature(release.AllowedSigners, msg, sig)
		}
	}
	if err == nil {
		err = release.CheckVersionLine(msg, tag)
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
	fmt.Printf("signature and version line OK: tc update and install.sh will accept this as %s\n", tag)
}

func exit(err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "verifyrelease:", err)
		return 1
	}
	return 0
}
