package work

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// swapRunner lets a test act as a sandboxed process racing tc's git: after
// tc's `git add` it runs swap, as if the agent won the race and left the
// worktree the way the add saw it through a link.
type swapRunner struct {
	*fakeRunner
	swap func()
}

func (s *swapRunner) Run(ctx context.Context, c Cmd) (string, error) {
	out, err := s.fakeRunner.Run(ctx, c)
	if c.Name == "git" && slices.Contains(c.Args, "add") && s.swap != nil {
		s.swap()
		s.swap = nil
	}
	return out, err
}

// A sandboxed process swaps a worktree file for a symlink to a file it
// cannot read, between git's lstat and open during tc's `git add`: the
// index gets the secret. checkStaged must catch that the path no longer
// is a plain file in the worktree; hash-object follows the link and finds
// the same secret, and Linked ignores symlinks, so it passed and tc
// committed the owner's file for push.
func TestPreparePRSymlinkSwappedAfterAdd_Break(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  string // the path that git staged
		link string // what becomes a symlink after the add
	}{
		{"file", "key.txt", "key.txt"},
		{"parent dir", "sub/id_ed25519", "sub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, w, f := realRepo(t)
			ctx := context.Background()
			wt, err := w.Worktree(ctx, "poll")
			if err != nil {
				t.Fatal(err)
			}
			secretDir := filepath.Join(filepath.Dir(w.RepoDir), "owner-secrets")
			secret := filepath.Join(secretDir, filepath.Base(tc.rel))
			write(t, secret, "owner's private key\n")
			// What git read through the link mid-add: the secret's bytes.
			write(t, filepath.Join(wt.Dir, tc.rel), "owner's private key\n")
			target := secret
			if tc.link != tc.rel {
				target = secretDir
			}
			sr := &swapRunner{fakeRunner: f, swap: func() {
				p := filepath.Join(wt.Dir, tc.link)
				if err := os.RemoveAll(p); err != nil {
					t.Error(err)
				}
				if err := os.Symlink(target, p); err != nil {
					t.Error(err)
				}
			}}
			w.Runner = sr
			pr, err := w.PreparePR(ctx, wt.Dir, wt.Branch, "x")
			if err == nil {
				t.Fatalf("committed %v with %s swapped for a link outside the worktree", pr.Files, tc.link)
			}
			if !strings.Contains(err.Error(), "link") {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		})
	}
}
