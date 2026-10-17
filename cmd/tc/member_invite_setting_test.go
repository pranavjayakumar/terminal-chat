package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemberInvitesFlagKeepsPersistedValue(t *testing.T) {
	dir := t.TempDir()
	parse := func(args ...string) bool {
		fs := flag.NewFlagSet("server", flag.ContinueOnError)
		var o optBool
		fs.Var(&o, "member-invites", "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return memberInvitesSetting(&o, dir)
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

func TestMemberInvitesHelpHasNoDefault(t *testing.T) {
	var sb strings.Builder
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(&sb)
	var o optBool
	fs.Var(&o, "member-invites", "x")
	fs.PrintDefaults()
	if strings.Contains(sb.String(), "default") {
		t.Fatal(sb.String())
	}
}
