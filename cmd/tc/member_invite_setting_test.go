package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestMemberInvitesFlagKeepsPersistedValue(t *testing.T) {
	dir := t.TempDir()
	parse := func(args ...string) bool {
		fs := flag.NewFlagSet("server", flag.ContinueOnError)
		v := fs.Bool("member-invites", true, "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return memberInvitesSetting(fs, dir, *v)
	}
	if !parse() {
		t.Fatal("fresh data dir: default on")
	}
	os.WriteFile(filepath.Join(dir, memberInvitesFile), []byte("false"), 0o600)
	if parse() {
		t.Fatal("unset flag must keep the persisted false")
	}
	if !parse("--member-invites=true") {
		t.Fatal("explicit true must win")
	}
	os.WriteFile(filepath.Join(dir, memberInvitesFile), []byte("true"), 0o600)
	if parse("--member-invites=false") {
		t.Fatal("explicit false must win")
	}
}
