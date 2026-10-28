package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/client"
)

type fakeEndorse struct {
	err error
	got string
}

func (f *fakeEndorse) OwnKeyFingerprint() string { return "SHA256:desk" }
func (f *fakeEndorse) EndorseKey(_ context.Context, fp string) (client.EndorseResult, error) {
	f.got = fp
	return client.EndorseResult{By: "SHA256:desk", Key: fp}, f.err
}

func TestParseDevicesEndorse(t *testing.T) {
	if a, err := parseDevices([]string{"endorse", "SHA256:abc"}); err != nil || a.endorse != "SHA256:abc" {
		t.Fatalf("%+v %v", a, err)
	}
	for _, args := range [][]string{{"endorse"}, {"endorse", "abc"}, {"endorse", "SHA256:a", "SHA256:b"}} {
		if _, err := parseDevices(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

// tc devices endorse (#146) vouches for the key and prints the command for
// the other computer; an already trusted key is not an error.
func TestDevicesEndorse(t *testing.T) {
	f := &fakeEndorse{}
	var out bytes.Buffer
	if err := endorseKey(context.Background(), f, "SHA256:laptop", &out); err != nil || f.got != "SHA256:laptop" {
		t.Fatalf("%v %q", err, f.got)
	}
	if s := out.String(); !strings.Contains(s, "tc devices endorse SHA256:desk") || !strings.Contains(s, "vouches for SHA256:laptop") {
		t.Fatal(s)
	}
	out.Reset()
	f.err = client.ErrKeyTrusted
	if err := endorseKey(context.Background(), f, "SHA256:laptop", &out); err != nil || !strings.Contains(out.String(), "already trusted") {
		t.Fatalf("%v %s", err, out.String())
	}
	f.err = errors.New("boom")
	if err := endorseKey(context.Background(), f, "SHA256:laptop", &out); err == nil {
		t.Fatal("error swallowed")
	}

	out.Reset()
	printOwnKey(&out, "SHA256:desk", []client.DeviceInfo{{Verified: true}, {Verified: false}})
	if s := out.String(); !strings.Contains(s, "This computer's SSH key: SHA256:desk") || !strings.Contains(s, "tc devices endorse") {
		t.Fatal(s)
	}
	out.Reset()
	printOwnKey(&out, "SHA256:desk\x1b[2J", []client.DeviceInfo{{Verified: true}})
	if s := out.String(); strings.Contains(s, "endorse") || strings.Contains(s, "\x1b") {
		t.Fatalf("%q", s)
	}
}
