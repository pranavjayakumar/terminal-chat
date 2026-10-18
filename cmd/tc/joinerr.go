package main

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/transport"
)

// pinLen is the length of a server pin: unpadded base64url of a SHA-256.
const pinLen = 43

// checkPin explains a pin mismatch caused by a cut-off or mangled #pin.
func checkPin(pin string) error {
	ok := len(pin) == pinLen
	for i := 0; ok && i < len(pin); i++ {
		c := pin[i]
		ok = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
	}
	if !ok {
		return fmt.Errorf("the invite's #pin part should be %d letters and digits but isn't, so the invite looks cut off or mistyped; copy the whole line again (it ends after the #)", pinLen)
	}
	return nil
}

// joinErrorHint turns a failed join or login into something a new user can
// act on. err stays wrapped so callers can still inspect it.
func joinErrorHint(inv proto.Invite, err error) error {
	if err == nil {
		return nil
	}
	addr := inv.Addr
	a := sanitize.Line(addr)
	var bye *client.ByeError
	if errors.As(err, &bye) {
		switch bye.Code {
		case proto.ByeInviteInvalid:
			return fmt.Errorf("this invite doesn't work: it's mistyped, used up, revoked or expired. Ask whoever sent it for a new one (%w)", err)
		case proto.ByeRevoked:
			return fmt.Errorf("this account or device was shut out by an admin; ask them to invite you again (%w)", err)
		}
		return err
	}
	if errors.Is(err, transport.ErrPinMismatch) {
		if perr := checkPin(inv.Pin); perr != nil {
			return fmt.Errorf("%w (%w)", perr, err)
		}
		return fmt.Errorf("the server at %s is not the one this tc:// line was made for (its pin doesn't match). Check that you copied the whole invite; if you did, the server was reinstalled or something is intercepting the connection: ask your admin for a fresh invite (%w)", a, err)
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return fmt.Errorf("can't find the server %s: no such host. Check the host name in the invite, or your network/VPN (%w)", a, err)
	}
	if strings.Contains(err.Error(), " dial ") || strings.HasPrefix(err.Error(), "quic dial") || strings.HasPrefix(err.Error(), "tcp dial") {
		_, port, _ := net.SplitHostPort(addr)
		return fmt.Errorf("can't reach the server at %s: it may be down, or a firewall blocks port %s (TCP and UDP). Check the address with your admin, or try again later (%w)", a, sanitize.Line(port), err)
	}
	return err
}
