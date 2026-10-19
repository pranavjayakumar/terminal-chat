package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A typo'd or default --data must not mint a new server identity and print
// an invite for it: that invite matches no running server.
func TestServerInviteRefusesMissingData(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nowhere")
	err := cmdServerInvite([]string{"--data", dir, "--raw"})
	if err == nil || !strings.Contains(err.Error(), "--data") {
		t.Fatalf("err = %v, want a hint about --data", err)
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Fatalf("%s was created (stat err %v)", dir, serr)
	}
}

func TestServerInviteHintCarriesData(t *testing.T) {
	t.Setenv("TC_DATA", "")
	if got := serverInviteHint(defaultDataDir()); strings.Contains(got, "--data") {
		t.Errorf("default dir: %q", got)
	}
	if got := serverInviteHint("/srv/tc data"); !strings.HasSuffix(got, ` --data '/srv/tc data'`) {
		t.Errorf("custom dir: %q", got)
	}
	if got := serverInviteHint("/srv/tc"); !strings.HasSuffix(got, " --data /srv/tc") {
		t.Errorf("plain dir: %q", got)
	}
}
