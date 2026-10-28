package client

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/trust"
)

// Own-key endorsement (#146, PROTOCOL.md §13.2 key.endorse).
//
// A second SSH key of mine (added by an add-key invite) is a pending key
// change on my other devices: nothing verifies it but the server's word,
// and the server could have invented it. Trusting it on a prompt the server
// can trigger would hand its devices every channel key, so /accept-key
// refuses my own keys. Instead the user names the key by fingerprint, read
// off the other computer itself, and this device signs a KeyEndorse for it
// with the SSH key it logs in with. Every client that pinned this key
// (my other devices, everyone I talk to) then pins the endorsed one through
// the KeyEndorse chain (verifyDevices); a key nobody endorsed stays pending.

var (
	// ErrKeyTrusted: the key is already trusted on this device.
	ErrKeyTrusted = errors.New("that key is already trusted here")
	// ErrKeyNotSeen: no device of mine has signed in with that key.
	ErrKeyNotSeen = errors.New("no device of yours has signed in with that SSH key yet")
)

// EndorseResult says which keys an EndorseKey call linked.
type EndorseResult struct {
	By  string // this device's SSH key fingerprint, which signed
	Key string // the endorsed key's fingerprint
}

// OwnKeyFingerprint is the SHA256 fingerprint of the SSH key this client
// logs in with ("" while it has never connected in this process).
func (c *Client) OwnKeyFingerprint() string {
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s != nil && len(s.sshPub) > 0 {
		if pk, err := ssh.ParsePublicKey(s.sshPub); err == nil {
			return ssh.FingerprintSHA256(pk)
		}
	}
	if fp := c.cfg.KeyFP; fp != "" {
		return fp
	}
	if s, err := identity.Load(c.cfg.KeySpec()); err == nil {
		return identity.Fingerprint(s.PublicKey())
	}
	return ""
}

// EndorseKey makes this device's SSH key vouch for my other SSH key fp
// (SHA256:…, as the other computer shows it): it signs a KeyEndorse and
// publishes it with key.endorse. fp must be a key the server registered for
// me that some device of mine signed in with (a pending own-key change
// here); the user, not the server, says which. ErrKeyTrusted if it is
// trusted already, ErrKeyNotSeen if no device of mine uses it.
func (c *Client) EndorseKey(ctx context.Context, fp string) (EndorseResult, error) {
	fp = strings.TrimSpace(fp)
	if !strings.HasPrefix(fp, "SHA256:") || len(fp) < len("SHA256:")+10 || strings.ContainsAny(fp, " \t") {
		return EndorseResult{}, errors.New("give the key's full SHA256 fingerprint, e.g. SHA256:AbC… (tc devices on that computer shows it)")
	}
	if c.IsAgentSession() {
		return EndorseResult{}, errors.New("an agent can't vouch for SSH keys")
	}
	if err := c.requireCap(proto.CapKeyEndorse, "endorsing SSH keys (update the server)"); err != nil {
		return EndorseResult{}, err
	}
	s, sid, err := c.e2eConn(ctx)
	if err != nil {
		return EndorseResult{}, err
	}
	me := c.Me().ID
	mine, err := ssh.ParsePublicKey(s.sshPub)
	if err != nil {
		return EndorseResult{}, fmt.Errorf("this session's SSH key: %w", err)
	}
	res := EndorseResult{By: ssh.FingerprintSHA256(mine), Key: fp}
	if fp == res.By {
		return res, errors.New("that's this computer's own SSH key; run this on another computer of yours to vouch for it")
	}
	// The other device may have registered since our last look.
	if err := c.RefreshDevices(ctx, me); err != nil {
		return res, err
	}
	pinned, err := c.trust.PinnedKeys(sid, me)
	if err != nil {
		return res, err
	}
	for _, k := range pinned {
		if k.Fingerprint == fp {
			if k.How == trust.Agent {
				return res, errors.New("that's your agent's key; it can't be made one of yours")
			}
			return res, ErrKeyTrusted
		}
	}
	pend, err := c.trust.PendingKeyChanges(sid)
	if err != nil {
		return res, err
	}
	var wire []byte
	for _, k := range pend {
		if k.User == me && k.Fingerprint == fp {
			wire = k.Wire
		}
	}
	if wire == nil {
		return res, fmt.Errorf("%w (%s): open tc on that computer first, then try again", ErrKeyNotSeen, sanitize.Line(fp))
	}
	newKey, err := ssh.ParsePublicKey(wire)
	if err != nil || ssh.FingerprintSHA256(newKey) != fp {
		return res, fmt.Errorf("%w (%s)", ErrKeyNotSeen, sanitize.Line(fp))
	}
	signer, err := identity.Load(c.cfg.KeySpec())
	if err != nil {
		return res, fmt.Errorf("loading your SSH key: %w", err)
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), s.sshPub) {
		return res, errors.New("the SSH key changed since this session logged in; reconnect and try again")
	}
	e, err := identity.NewKeyEndorse(signer, sid, me, newKey, time.Now())
	if err != nil {
		return res, fmt.Errorf("signing the endorsement: %w", err)
	}
	if _, err := s.call(ctx, proto.OpKeyEndorse, proto.KeyEndorseArgs{Endorse: proto.Signed(e)}, nil); err != nil {
		return res, err
	}
	// Our own copy counts at once, whatever the event stream does.
	if err := c.tx(func(tx *sql.Tx) error { return storeEndorse(tx, proto.Signed(e)) }); err != nil {
		return res, err
	}
	c.e2eMu.Lock()
	c.verifyDevices(sid, s.sshPub, me)
	c.e2eMu.Unlock()
	c.requeueKeyChange()
	c.kickE2E() // messages waiting on the key decrypt in the next pass
	return res, nil
}
