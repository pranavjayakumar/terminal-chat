package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

const goodPin = "Ig_HZrSFYtzO5RH2yEAmY79IDGcW5oB305OjPeFMico"

// Every join failure a new user can hit says what went wrong in words and
// what to do next, and keeps the underlying error inspectable.
func TestJoinErrorsAreActionable(t *testing.T) {
	refused := fmt.Errorf("tcp dial h:7443: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED})
	cases := []struct {
		name, pin string
		err       error
		want      []string
	}{
		{"used up", goodPin, &client.ByeError{Code: proto.ByeInviteInvalid, Msg: "invite is invalid, used or expired"},
			[]string{"used up, revoked or expired", "Ask whoever sent it for a new one"}},
		{"wrong pin", goodPin, fmt.Errorf("tcp dial h:7443: %w: got x, want y", transport.ErrPinMismatch),
			[]string{"not the one", "ask your admin for a fresh invite"}},
		{"cut-off pin", "Ig_HZrSFYtzO5", fmt.Errorf("tcp dial h:7443: %w: got x, want y", transport.ErrPinMismatch),
			[]string{"looks cut off", "copy the whole line again"}},
		{"dns", goodPin, errors.Join(fmt.Errorf("quic dial h:7443: %w", &net.DNSError{Err: "no such host", Name: "h", IsNotFound: true})),
			[]string{"can't find the server h:7443", "Check the host name"}},
		{"down", goodPin, errors.Join(refused, errors.New("quic dial h:7443: timeout: no recent network activity")),
			[]string{"can't reach the server at h:7443", "port 7443", "down"}},
		{"disabled", goodPin, &client.ByeError{Code: proto.ByeRevoked},
			[]string{"disabled by an admin", "new SSH key"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, f, _ := testJoinEnv(t, strings.NewReader(""), false)
			writeKey(t, filepath.Join(env.sshDir, "id_ed25519"))
			f.errs = []error{c.err}
			err := runJoin(context.Background(), env, "tc://h:7443/tok#"+c.pin, "ada", "")
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
			if !errors.Is(err, c.err) {
				t.Errorf("underlying error not wrapped: %v", err)
			}
		})
	}
}

// tc send/tail/users on a disabled account say why, not just "revoked".
func TestStoppedErrorExplains(t *testing.T) {
	for msg, want := range map[string]string{
		(&client.ByeError{Code: proto.ByeRevoked}).Error():             "disabled by an admin",
		(&client.ByeError{Code: proto.ByeAuthFailed}).Error():          "no longer accepts",
		"tcp dial h:1: server certificate pin mismatch: got a, want b": "identity changed",
		"something else": "something else",
	} {
		if got := stoppedError(msg).Error(); !strings.Contains(got, want) || !strings.Contains(got, msg) {
			t.Errorf("%q -> %q, want %q", msg, got, want)
		}
	}
}
