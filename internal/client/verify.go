package client

import (
	"bufio"
	"context"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/trust"
)

// Trust UX (docs/E2E.md step 10): safety numbers and /verify, GitHub key
// checks, key-change acknowledgement and deliberate alarm clears. All of
// it works on the trust store, so it works offline (except the GitHub
// fetch).

var (
	// ErrNoKeys means no SSH key of the user has been seen (pinned) yet.
	ErrNoKeys = errors.New("no key of that user seen yet — they need to connect once")
	// ErrSafetyChanged means the keys behind a shown safety number changed
	// before it was confirmed: compare the new number.
	ErrSafetyChanged = errors.New("the keys changed since the safety number was shown — compare again")
	// ErrKeyChange refuses sends into an encrypted channel where a member's
	// SSH key changed and the change isn't acknowledged yet.
	ErrKeyChange = errors.New("sending is blocked: a member's key changed")
	// ErrNoAlarm means there is no alarm to show or clear.
	ErrNoAlarm = errors.New("no alarm here")
)

// safetyDomain and safetyIterations define safety numbers (like Signal's:
// iterated SHA-512, 5-byte chunks mod 100000, 5200 rounds).
const (
	safetyDomain     = "tc-safety-v1"
	safetyIterations = 5200
	safetyGroups     = 12
)

// SafetyParty is one side of a safety number: a user and the fingerprints
// of the SSH keys this device has pinned for them.
type SafetyParty struct {
	User         uint64
	Name         string // sanitized
	Fingerprints []string
}

// SafetyNumber is the 60-digit number two users compare out of band. Each
// computes it from their own trust store; it matches only if both pinned
// the same keys for both of them.
type SafetyNumber struct {
	Digits string // 60 digits
	Me     SafetyParty
	Them   SafetyParty
}

// Groups is Digits in 12 groups of 5.
func (s SafetyNumber) Groups() []string {
	var out []string
	for i := 0; i+5 <= len(s.Digits); i += 5 {
		out = append(out, s.Digits[i:i+5])
	}
	return out
}

// safetyDigits is SHA-512 over the domain, the server id and both parties
// (ordered by user id, fingerprints sorted, so both sides agree), iterated
// safetyIterations times with the input appended, as 12 groups of five
// digits (5 bytes each, big-endian, mod 100000).
func safetyDigits(sid e2ecrypto.ServerID, a, b SafetyParty) string {
	if b.User < a.User {
		a, b = b, a
	}
	var in []byte
	in = append(in, safetyDomain...)
	in = append(in, sid[:]...)
	for _, p := range []SafetyParty{a, b} {
		fps := slices.Clone(p.Fingerprints)
		slices.Sort(fps)
		fps = slices.Compact(fps)
		in = binary.BigEndian.AppendUint64(in, p.User)
		in = binary.BigEndian.AppendUint16(in, uint16(len(fps)))
		for _, fp := range fps {
			in = binary.BigEndian.AppendUint16(in, uint16(len(fp)))
			in = append(in, fp...)
		}
	}
	h := sha512.Sum512(in)
	buf := make([]byte, 0, len(h)+len(in))
	for range safetyIterations {
		buf = append(append(buf[:0], h[:]...), in...)
		h = sha512.Sum512(buf)
	}
	var sb strings.Builder
	for i := range safetyGroups {
		c := h[i*5 : i*5+5]
		v := uint64(c[0])<<32 | uint64(c[1])<<24 | uint64(c[2])<<16 | uint64(c[3])<<8 | uint64(c[4])
		fmt.Fprintf(&sb, "%05d", v%100000)
	}
	return sb.String()
}

func (c *Client) party(sid e2ecrypto.ServerID, user uint64) (SafetyParty, error) {
	ks, err := c.trust.PinnedKeys(sid, user)
	if err != nil {
		return SafetyParty{}, err
	}
	p := SafetyParty{User: user, Name: sanitize.Line(c.User(user).Name)}
	for _, k := range ks {
		p.Fingerprints = append(p.Fingerprints, k.Fingerprint)
	}
	slices.Sort(p.Fingerprints)
	return p, nil
}

// SafetyNumber is the safety number between me and user, from the keys
// this device pinned for both of us. Works offline.
func (c *Client) SafetyNumber(user uint64) (SafetyNumber, error) {
	sid, ok := c.cacheServerID()
	if !ok {
		return SafetyNumber{}, errors.New("not synced with the server yet")
	}
	me := c.Me().ID
	if user == me {
		return SafetyNumber{}, errors.New("that's you — verify someone else")
	}
	mine, err := c.party(sid, me)
	if err != nil {
		return SafetyNumber{}, err
	}
	theirs, err := c.party(sid, user)
	if err != nil {
		return SafetyNumber{}, err
	}
	if len(theirs.Fingerprints) == 0 {
		return SafetyNumber{}, ErrNoKeys
	}
	if len(mine.Fingerprints) == 0 {
		return SafetyNumber{}, errors.New("your own key isn't pinned yet — connect once")
	}
	return SafetyNumber{Digits: safetyDigits(sid, mine, theirs), Me: mine, Them: theirs}, nil
}

// MarkVerified records that the user compared safety number digits with
// user out of band and it matched: user's pinned keys (exactly those the
// number covers) become Verified, so their devices count as verified out
// of band and UnverifiedUsers stops listing them. ErrSafetyChanged if the
// keys changed since digits was computed.
func (c *Client) MarkVerified(user uint64, digits string) error {
	sn, err := c.SafetyNumber(user)
	if err != nil {
		return err
	}
	if sn.Digits != digits {
		return ErrSafetyChanged
	}
	return c.verifyKeys(user, sn.Them.Fingerprints, "safety number")
}

func (c *Client) verifyKeys(user uint64, fps []string, method string) error {
	sid, ok := c.cacheServerID()
	if !ok {
		return errors.New("not synced with the server yet")
	}
	if err := c.trust.VerifyKeys(sid, user, fps, method, c.Me().ID); err != nil {
		if errors.Is(err, trust.ErrPinsChanged) {
			return ErrSafetyChanged
		}
		return err
	}
	c.kickE2E()
	c.post(Update{Channels: true})
	return nil
}

// ---- GitHub ----

// githubBase is where VerifyGitHub fetches <login>.keys (tests replace it).
const githubBase = "https://github.com"

// maxGitHubKeys bounds the response read from GitHub.
const maxGitHubKeys = 256 << 10

var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// GitHubCheck compares a user's pinned SSH keys with a GitHub account's
// public keys.
type GitHubCheck struct {
	Login   string
	User    uint64
	Name    string   // sanitized
	Pinned  []string // fingerprints of user's pinned keys
	GitHub  []string // fingerprints of the account's keys
	Missing []string // pinned keys that aren't on the account
	Match   bool     // every pinned key is on the account: now Verified
}

// VerifyGitHub fetches https://github.com/<login>.keys (only ever on this
// explicit request) and compares it with user's pinned keys. If every
// pinned key is listed, they become Verified ("github:<login>" in the
// audit log); otherwise nothing changes and Missing says which aren't.
func (c *Client) VerifyGitHub(ctx context.Context, user uint64, login string) (GitHubCheck, error) {
	if !githubLogin.MatchString(login) {
		return GitHubCheck{}, errors.New("not a GitHub user name")
	}
	sid, ok := c.cacheServerID()
	if !ok {
		return GitHubCheck{}, errors.New("not synced with the server yet")
	}
	p, err := c.party(sid, user)
	if err != nil {
		return GitHubCheck{}, err
	}
	if len(p.Fingerprints) == 0 {
		return GitHubCheck{}, ErrNoKeys
	}
	base := c.githubBase
	if base == "" {
		base = githubBase
	}
	gh, err := fetchGitHubKeys(ctx, base, login)
	if err != nil {
		return GitHubCheck{}, err
	}
	res := GitHubCheck{Login: login, User: user, Name: p.Name, Pinned: p.Fingerprints, GitHub: gh}
	for _, fp := range p.Fingerprints {
		if !slices.Contains(gh, fp) {
			res.Missing = append(res.Missing, fp)
		}
	}
	res.Match = len(res.Missing) == 0
	if res.Match {
		if err := c.verifyKeys(user, p.Fingerprints, "github:"+login); err != nil {
			return res, err
		}
	}
	return res, nil
}

func fetchGitHubKeys(ctx context.Context, base, login string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+url.PathEscape(login)+".keys", nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching GitHub keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching GitHub keys: HTTP %d", resp.StatusCode)
	}
	var out []string
	sc := bufio.NewScanner(io.LimitReader(resp.Body, maxGitHubKeys))
	sc.Buffer(make([]byte, 0, 4096), maxGitHubKeys)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			continue
		}
		if fp := ssh.FingerprintSHA256(pk); !slices.Contains(out, fp) {
			out = append(out, fp)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading GitHub keys: %w", err)
	}
	slices.Sort(out)
	return out, nil
}

// ---- key changes ----

// KeyChange is a user's SSH key that verifies but isn't pinned or endorsed:
// nothing is sealed to it until AcceptKeyChange.
type KeyChange struct {
	User        uint64
	Fingerprint string
	FirstSeen   time.Time
}

// KeyChanges lists other users' unacknowledged key changes. Works offline.
func (c *Client) KeyChanges() []KeyChange {
	sid, ok := c.cacheServerID()
	if !ok {
		return nil
	}
	ks, err := c.trust.PendingKeyChanges(sid)
	if err != nil {
		return nil
	}
	me := c.Me().ID
	var out []KeyChange
	for _, k := range ks {
		if k.User != me {
			out = append(out, KeyChange{User: k.User, Fingerprint: k.Fingerprint, FirstSeen: k.FirstSeen})
		}
	}
	return out
}

// KeyChangedMembers lists the members of encrypted channel ch (not me)
// with an unacknowledged key change; sending there is blocked until each
// is accepted or verified. Sorted; nil for plaintext channels.
func (c *Client) KeyChangedMembers(ch uint64) []uint64 {
	sid, ok := c.cacheServerID()
	if !ok || !c.chanIsE2E(c.db, sid, ok, ch) {
		return nil
	}
	cv, ok, err := getChan(c.db, ch)
	if err != nil || !ok {
		return nil
	}
	changed := map[uint64]bool{}
	for _, k := range c.KeyChanges() {
		changed[k.User] = true
	}
	var out []uint64
	for _, u := range cv.Members {
		if changed[u] && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	slices.Sort(out)
	return out
}

// keyChangeAllowed refuses sends into an encrypted channel with a member
// whose key change isn't acknowledged.
func (c *Client) keyChangeAllowed(ch uint64) error {
	us := c.KeyChangedMembers(ch)
	if len(us) == 0 {
		return nil
	}
	names := make([]string, len(us))
	for i, u := range us {
		names[i] = "@" + sanitize.Line(c.User(u).Name)
	}
	return fmt.Errorf("%w: %s — /verify or /accept-key in tc", ErrKeyChange, strings.Join(names, ", "))
}

// AcceptKeyChange acknowledges every pending key change of user (the user
// read the banner and accepts the new key): the keys are pinned (trusted
// on first use again, so the user shows as unverified) and their devices
// verify on the next pass.
func (c *Client) AcceptKeyChange(user uint64) error {
	sid, ok := c.cacheServerID()
	if !ok {
		return errors.New("not synced with the server yet")
	}
	n := 0
	for _, k := range c.KeyChanges() {
		if k.User != user {
			continue
		}
		if err := c.trust.AcknowledgeKeyChange(sid, user, k.Fingerprint); err != nil && !errors.Is(err, trust.ErrNoKeyChange) {
			return err
		}
		n++
	}
	if n == 0 {
		return errors.New("no pending key change for that user")
	}
	c.kickE2E()
	c.post(Update{Channels: true})
	return nil
}

// ---- alarms ----

// AlarmInfo explains a channel's alarm.
type AlarmInfo struct {
	Detail   string    // sanitized
	Why      string    // what it means and how it can happen
	Since    time.Time // zero if not stored
	Server   bool      // the server-wide alarm (it affects every encrypted channel)
	Computed bool      // derived from the current state, not stored: clears by itself
}

// Alarm explains ch's alarm; ok is false if it has none.
func (c *Client) Alarm(ch uint64) (AlarmInfo, bool) {
	st := c.E2EStatus(ch)
	if st.State != E2EAlarm {
		return AlarmInfo{}, false
	}
	sid, _ := c.cacheServerID()
	info := AlarmInfo{Detail: sanitize.Line(st.Detail)}
	if d, at, ok, _ := c.trust.ServerAlarmSince(sid); ok {
		info.Detail, info.Since, info.Server = sanitize.Line(d), at, true
	} else if d, at, ok, _ := c.trust.AlarmSince(sid, ch); ok {
		info.Detail, info.Since = sanitize.Line(d), at
	} else {
		info.Computed = true
	}
	info.Why = explainAlarm(info)
	return info, true
}

func explainAlarm(a AlarmInfo) string {
	d := a.Detail
	switch {
	case a.Server:
		return "The server stopped offering end-to-end encryption after offering it before. An honest server " +
			"doesn't downgrade; a server that wants to read new messages would."
	case strings.HasPrefix(d, unconfirmedPrefix):
		return "Someone is in the member list but no signed key update from a member includes them: only the " +
			"server says they belong. Nothing is sealed to them. It clears once a member adds them properly " +
			"(/add) or they're removed. It also shows for a moment while an add is in flight."
	case strings.Contains(d, "no longer encrypted"):
		return "This device saw the channel encrypted, and now the server says it's plaintext. An honest server " +
			"never turns encryption off; nothing is sent in the clear."
	case strings.Contains(d, "fork"):
		return "The server showed two different key bundles for the same epoch. An honest server never does: it " +
			"may be giving members different keys to split the channel."
	case strings.Contains(d, "rollback"):
		return "The server showed an older key epoch than this device already verified: it may be replaying old " +
			"state, for example to hide a membership change."
	}
	return "A key bundle from the server failed verification (bad signature, wrong rotator, key sealed to a " +
		"non-member or revoked device). An honest server and honest members can't produce one."
}

// ClearAlarm clears ch's stored alarm (or, if that's where it comes from,
// the server-wide alarm), audited as done by me. Computed alarms can't be
// cleared: they end when their cause does. If the server still presents
// what raised the alarm, it is raised again.
func (c *Client) ClearAlarm(ch uint64) error {
	info, ok := c.Alarm(ch)
	if !ok {
		return ErrNoAlarm
	}
	if info.Computed {
		return errors.New("this alarm comes from the current state and clears when its cause is gone")
	}
	sid, _ := c.cacheServerID()
	me := c.Me().ID
	var err error
	if info.Server {
		err = c.trust.ClearTrustAlarm(sid, me)
	} else {
		err = c.trust.ClearChannelAlarm(sid, ch, me)
	}
	if errors.Is(err, trust.ErrNoAlarm) {
		return ErrNoAlarm
	}
	if err != nil {
		return err
	}
	c.kickE2E()
	c.post(Update{Channels: true})
	return nil
}
