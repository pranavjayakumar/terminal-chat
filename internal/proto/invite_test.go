package proto

import (
	"strings"
	"testing"
)

func TestInstallCommand(t *testing.T) {
	code := FormatInvite("chat.example.com:7443", "abcdefghijklmnopqrstuvwxyz", "AbC-_09xyz")
	got, err := InstallCommand(code, "dev")
	if err != nil {
		t.Fatal(err)
	}
	want := "curl -fsSL https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/main/install.sh | sh -s -- 'tc://chat.example.com:7443/abcdefghijklmnopqrstuvwxyz#AbC-_09xyz'"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if strings.Count(got, "'") != 2 {
		t.Fatalf("quotes: %s", got)
	}
	// A release build pins install.sh to its own tag; anything else uses main.
	got, _ = InstallCommand(code, "v0.2.0")
	if want := "curl -fsSL https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/v0.2.0/install.sh | sh -s -- '" + code + "'"; got != want {
		t.Fatalf("release build:\ngot  %s\nwant %s", got, want)
	}
	for _, v := range []string{"", "dev", "v0.2.0-3-gabcdef", "v0.2.0-dirty", "v0.2.0-rc1", "v0.2", "v0.2.0/../../evil", "v0.2.0' x"} {
		if u := InstallScriptURL(v); !strings.Contains(u, "/terminal-chat/main/install.sh") {
			t.Errorf("version %q: %s", v, u)
		}
	}
	// IPv6 hosts keep their brackets.
	if _, err := InstallCommand(FormatInvite("[2001:db8::1]:7443", "tok", "pin"), "dev"); err != nil {
		t.Fatal(err)
	}
}

func TestInstallCommandRejectsUnsafe(t *testing.T) {
	for _, code := range []string{
		"",
		"not-an-invite",
		"tc://host:7443/tok#pin'; rm -rf ~; echo '",
		"tc://host:7443/tok#pi'n",
		"tc://host:7443/to$(id)k#pin",
		"tc://host:7443/tok#pin`id`",
		"tc://host:7443/tok#pin\n",
		"tc://host:7443/tok#pin\x1b]52;c;x\a",
		"tc://ho st:7443/tok#pin",
		"tc://host:7443/tok#pin\\",
		"tc://host:7443/tok#p\"in",
		"tc://host:7443/tok#pin&x",
		"tc://host:7443/tok#pïn",
	} {
		if cmd, err := InstallCommand(code, "v1.0.0"); err == nil {
			t.Errorf("%q accepted: %s", code, cmd)
		}
	}
}

func TestKeySrc(t *testing.T) {
	for _, s := range []string{"github:alice", "github:A", "github:a-b-9", "github:" + strings.Repeat("a", 39)} {
		if !ValidKeySrc(s) {
			t.Errorf("%q refused", s)
		}
		if login, ok := GitHubKeySpec(s); !ok || "github:"+login != s {
			t.Errorf("GitHubKeySpec(%q) = %q %v", s, login, ok)
		}
	}
	if !ValidKeySrc("") {
		t.Error("empty key_src refused")
	}
	if _, ok := GitHubKeySpec(""); ok {
		t.Error("empty --key accepted")
	}
	for _, s := range []string{"alice", "github:", "github:-alice", "github:a_b", "github:a b", "github:a/b", "github:../x",
		"gitlab:alice", "GITHUB:alice", "github:alice\n", "github:\x1b[2J", "github:" + strings.Repeat("a", 40), "github:ålice"} {
		if ValidKeySrc(s) {
			t.Errorf("%q accepted", s)
		}
		if _, ok := GitHubKeySpec(s); ok {
			t.Errorf("GitHubKeySpec(%q) accepted", s)
		}
	}
}

// The key-invite fields are optional: a v0.2.0 peer's frames decode with
// them empty, and v0.2.0 ignores them (which is why clients gate keys on
// cap key_invites).
func TestInviteFieldsCompat(t *testing.T) {
	type oldArgs struct {
		User  uint64 `msgpack:"user,omitempty"`
		Uses  int64  `msgpack:"uses,omitempty"`
		TTLMs int64  `msgpack:"ttl_ms,omitempty"`
	}
	var a InviteCreateArgs
	if err := Unmarshal(MustRaw(oldArgs{Uses: 3}), &a); err != nil || a.Uses != 3 || a.Keys != nil || a.KeySrc != "" {
		t.Fatalf("old args: %+v %v", a, err)
	}
	b := MustRaw(InviteCreateArgs{Uses: 1, Keys: [][]byte{{1, 2}}, KeySrc: "github:x"})
	var o oldArgs
	if err := Unmarshal(b, &o); err != nil || o.Uses != 1 {
		t.Fatalf("new args on an old peer: %+v %v", o, err)
	}
	if err := Unmarshal(b, &a); err != nil || len(a.Keys) != 1 || a.KeySrc != "github:x" {
		t.Fatalf("round trip: %+v %v", a, err)
	}
	var info InviteInfo
	if err := Unmarshal(MustRaw(InviteInfo{ID: "x", Keys: 2, KeySrc: "github:y"}), &info); err != nil || info.Keys != 2 || info.KeySrc != "github:y" {
		t.Fatalf("info: %+v %v", info, err)
	}
	if raw := MustRaw(InviteInfo{ID: "x"}); strings.Contains(string(raw), "key_src") {
		t.Fatal("unbound invite carries key fields")
	}
}

// Every byte FormatInvite can produce from a valid server is shell-safe:
// base32 tokens, base64url pins and validated hosts.
func TestShellSafeCharset(t *testing.T) {
	for c := 0; c < 256; c++ {
		ok := shellSafe(string(rune(c)))
		if c >= 0x80 {
			ok = shellSafe(string([]byte{byte(c)}))
		}
		if ok && strings.ContainsRune("'\"`$\\;&|<>()*?!~{} \t\n", rune(c)) {
			t.Fatalf("%q considered shell-safe", c)
		}
	}
}
