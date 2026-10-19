package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// A mistyped GitHub login says so instead of "HTTP 404".
func TestInviteKeyUnknownGitHubUser(t *testing.T) {
	gh := httptest.NewServer(http.NotFoundHandler())
	defer gh.Close()
	_, err := client.FetchGitHubInviteKeys(context.Background(), gh.URL, "no-such-user")
	err = githubKeysErr(err, "no-such-user")
	if err == nil || strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), `GitHub has no user "no-such-user"`) {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("TC_DATA", "")
	testGitHubBase = gh.URL
	defer func() { testGitHubBase = "" }()
	data := t.TempDir()
	os.WriteFile(filepath.Join(data, "tc.db"), nil, 0o600) // fails before the database is opened
	if err := cmdServerInvite([]string{"--data", data, "--key", "github:no-such-user"}); err == nil ||
		!strings.Contains(err.Error(), "GitHub has no user") {
		t.Fatalf("server invite: %v", err)
	}
}

// Too many open invites, from the CLI, points at the CLI commands.
func TestInviteConflictPointsAtCLI(t *testing.T) {
	err := inviteErr(&proto.Error{Code: proto.ErrConflict, Msg: "you already have 5 open invites; revoke one (/invites) first"}, "invite people", "")
	if !strings.Contains(err.Error(), "tc invite --list") || !strings.Contains(err.Error(), "tc invite --revoke") {
		t.Fatalf("err = %v", err)
	}
}
