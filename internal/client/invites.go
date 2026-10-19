package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// GitHubBase is where invite keys are fetched from: https://github.com.
const GitHubBase = githubBase

// inviteKeys keeps the keys an invite can be bound to (identity.InviteKey:
// the types tc logs in with, RSA >= 2048 bits), without duplicates.
func inviteKeys(keys []ssh.PublicKey) [][]byte {
	var out [][]byte
	for _, pk := range keys {
		b := pk.Marshal()
		// The server refuses the whole invite over one oversized key
		// (§4.1), e.g. a security key with a long application string.
		if len(b) > proto.MaxInviteKeyLen {
			continue
		}
		if _, err := identity.InviteKey(b); err != nil {
			continue
		}
		if !slices.ContainsFunc(out, func(x []byte) bool { return string(x) == string(b) }) {
			out = append(out, b)
		}
	}
	return out
}

// FetchGitHubInviteKeys fetches base/<login>.keys (base "" = GitHubBase)
// and returns the keys an invite can be bound to (PROTOCOL.md §4.1). The
// fetch happens on the inviter's machine; servers make no outbound
// requests. No usable key, or more than proto.MaxInviteKeys, is an error.
func FetchGitHubInviteKeys(ctx context.Context, base, login string) ([][]byte, error) {
	if base == "" {
		base = githubBase
	}
	all, err := getGitHubKeys(ctx, base, login)
	if err != nil {
		return nil, err
	}
	keys := inviteKeys(all)
	switch {
	case len(keys) == 0 && len(all) == 0:
		return nil, fmt.Errorf("github.com/%s has no SSH keys", login)
	case len(keys) == 0:
		return nil, fmt.Errorf("none of github.com/%s's %d SSH keys can log in to tc (ed25519, ECDSA or RSA ≥ 2048 bits)", login, len(all))
	case len(keys) > proto.MaxInviteKeys:
		return nil, fmt.Errorf("github.com/%s has %d SSH keys; an invite can be bound to at most %d", login, len(keys), proto.MaxInviteKeys)
	}
	return keys, nil
}

// CreateGitHubInvite returns a single-use new-user invite that only a
// holder of one of github.com/<login>'s SSH keys can redeem (cap
// key_invites, checked before anything is fetched: an older server would
// ignore the keys and mint an unbound invite). ttl 0 is the server default.
func (c *Client) CreateGitHubInvite(ctx context.Context, login string, ttl time.Duration) (proto.InviteRes, error) {
	if !githubLogin.MatchString(login) {
		return proto.InviteRes{}, errors.New("not a GitHub user name")
	}
	if err := c.requireCap(proto.CapKeyInvites, "key-bound invites (--key)"); err != nil {
		return proto.InviteRes{}, err
	}
	if ttl != 0 {
		if err := c.requireCap(proto.CapTeamInvites, "custom-expiry invites"); err != nil {
			return proto.InviteRes{}, err
		}
	}
	keys, err := FetchGitHubInviteKeys(ctx, c.githubBase, login)
	if err != nil {
		return proto.InviteRes{}, err
	}
	var r proto.InviteRes
	err = c.call(ctx, proto.OpInviteCreate, proto.InviteCreateArgs{TTLMs: ttl.Milliseconds(), Keys: keys, KeySrc: "github:" + login}, &r, nil)
	if err == nil && r.Keys == 0 {
		// The server dropped the binding: never hand out what looks bound
		// but isn't (a server with key_invites always echoes the count), and
		// try to revoke what it made.
		if inv, perr := proto.ParseInvite(r.Code); perr == nil {
			sum := sha256.Sum256([]byte(inv.Token))
			_ = c.RevokeInvite(ctx, hex.EncodeToString(sum[:])[:proto.InviteIDLen])
		}
		return proto.InviteRes{}, errors.New("the server didn't bind the invite to the keys; not using it")
	}
	return r, err
}
