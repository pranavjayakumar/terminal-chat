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
