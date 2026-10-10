package proto

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// CapTeamInvites is advertised in WELCOME when invite.create accepts uses
// and ttl_ms (PROTOCOL.md §4.1).
const CapTeamInvites = "team_invites"

// Invite limits (PROTOCOL.md §4.1).
const (
	DefaultInviteTTL = 7 * 24 * time.Hour
	MaxInviteTTL     = 30 * 24 * time.Hour
	MaxInviteUses    = 100
)

// InstallScriptURL is where the one-line install command fetches install.sh:
// the script at the release tag version (vX.Y.Z) when the running binary is
// a release, so a later push to main can't change what an invite runs, else
// main.
func InstallScriptURL(version string) string {
	ref := "main"
	if releaseTagRE.MatchString(version) {
		ref = version
	}
	return "https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/" + ref + "/install.sh"
}

var releaseTagRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// Invite is a parsed invite code (PROTOCOL.md §4.1).
type Invite struct {
	Addr  string // host:port
	Token string
	Pin   string
}

// FormatInvite renders tc://<host>:<port>/<token>#<pin>.
func FormatInvite(addr, token, pin string) string {
	return "tc://" + addr + "/" + token + "#" + pin
}

// ParseInvite parses an invite code.
func ParseInvite(code string) (Invite, error) {
	u, err := url.Parse(strings.TrimSpace(code))
	if err != nil || u.Scheme != "tc" {
		return Invite{}, errors.New("invite must look like tc://host:port/token#pin")
	}
	if _, _, err := net.SplitHostPort(u.Host); err != nil {
		return Invite{}, errors.New("invite is missing host:port")
	}
	inv := Invite{Addr: u.Host, Token: strings.Trim(u.Path, "/"), Pin: u.Fragment}
	if inv.Token == "" || inv.Pin == "" || strings.Contains(inv.Token, "/") {
		return Invite{}, errors.New("invite is missing token or pin")
	}
	return inv, nil
}

// shellSafe reports whether every byte of an invite code is one that can sit
// inside a single-quoted sh word and is plain on a terminal: letters, digits
// and the punctuation hosts, base32 tokens and base64url pins use.
func shellSafe(code string) bool {
	if code == "" {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte(":/#.-_[]", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// InstallCommand is the one-liner that installs tc and joins with code:
// curl … install.sh | sh -s -- '<code>', with install.sh pinned as in
// InstallScriptURL(version). It refuses codes that aren't a valid invite or
// hold anything but the shell-safe charset, so the single quotes can never
// be closed early.
func InstallCommand(code, version string) (string, error) {
	if _, err := ParseInvite(code); err != nil {
		return "", err
	}
	if !shellSafe(code) {
		return "", errors.New("invite code has characters that aren't safe in a shell command")
	}
	return "curl -fsSL " + InstallScriptURL(version) + " | sh -s -- '" + code + "'", nil
}
