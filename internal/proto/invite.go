package proto

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

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
